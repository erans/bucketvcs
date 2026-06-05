package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/byob"
	"github.com/bucketvcs/bucketvcs/internal/doctor"
	"github.com/bucketvcs/bucketvcs/internal/gateway"
	"github.com/bucketvcs/bucketvcs/internal/replica"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// runDoctor is `bucketvcs doctor`: read-only diagnostics over the same flag
// surface as serve ("swap serve for doctor"). It validates storage, the auth
// DB, config coherence, and host dependencies WITHOUT binding ports or
// mutating user data (the storage.writable probe PUTs and DELETEs one object
// under the reserved _doctor/ prefix).
//
// Exit codes: 0 all checks pass (warn/skip allowed), 1 any check fails,
// 2 usage error.
func runDoctor(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sf := registerServeFlags(fs)
	repoFlag := fs.String("repo", "", "Optional tenant/name to deep-check (manifest loads, schema gate, sampled storage keys exist)")
	asJSON := fs.Bool("json", false, "Emit one NDJSON object per check instead of the human table")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *sf.storeURL == "" {
		fmt.Fprintln(stderr, "doctor: --store is required")
		return 2
	}
	var repoTenant, repoName string
	if *repoFlag != "" {
		var ok bool
		repoTenant, repoName, ok = strings.Cut(*repoFlag, "/")
		if !ok || repoTenant == "" || repoName == "" {
			fmt.Fprintln(stderr, "doctor: --repo must be <tenant>/<name>")
			return 2
		}
	}

	// Shared state across checks: storage.reachable opens the store; the
	// authdb.open check opens the inspection handle and records liveness so
	// authdb.migrations never misreads "0" from a dead connection as a
	// virgin db (see sqlitestore.SchemaVersion's CAVEAT).
	var store storage.ObjectStore
	var authStore *sqlitestore.Store
	var authLive bool
	defer func() {
		if store != nil {
			closeStore(store)
		}
		if authStore != nil {
			authStore.Close()
		}
	}()

	// Order matters: checks that close over `store`/`authStore` (storage.writable,
	// authdb.migrations, repo.*) must run AFTER storage.reachable / authdb.open,
	// which populate them — reordering silently degrades those checks to SKIP.
	checks := []doctor.Check{
		{Name: "storage.reachable", Run: func(ctx context.Context) doctor.Result {
			s, err := openStore(*sf.storeURL)
			if err != nil {
				return doctor.Result{Status: doctor.StatusFail, Detail: err.Error()}
			}
			store = s
			if _, err := store.List(ctx, "", &storage.ListOptions{MaxKeys: 1}); err != nil {
				return doctor.Result{Status: doctor.StatusFail, Detail: "list: " + err.Error()}
			}
			return doctor.Result{Status: doctor.StatusOK, Detail: store.Name() + " backend, list ok"}
		}},

		{Name: "storage.writable", Run: func(ctx context.Context) doctor.Result {
			if *sf.replicaOf != "" {
				return doctor.Result{Status: doctor.StatusSkip, Detail: "read-only replica (--replica-of set)"}
			}
			if store == nil {
				return doctor.Result{Status: doctor.StatusSkip, Detail: "store unavailable"}
			}
			var buf [8]byte
			if _, err := rand.Read(buf[:]); err != nil {
				return doctor.Result{Status: doctor.StatusFail, Detail: "rand: " + err.Error()}
			}
			key := "_doctor/probe-" + hex.EncodeToString(buf[:])
			ver, err := store.PutIfAbsent(ctx, key, strings.NewReader("bucketvcs doctor probe"), nil)
			if err != nil {
				return doctor.Result{Status: doctor.StatusFail, Detail: "put " + key + ": " + err.Error()}
			}
			if err := store.DeleteIfVersionMatches(ctx, key, ver); err != nil {
				return doctor.Result{Status: doctor.StatusFail, Detail: "delete " + key + ": " + err.Error()}
			}
			return doctor.Result{Status: doctor.StatusOK, Detail: "probe put+delete ok"}
		}},

		{Name: "authdb.open", Run: func(ctx context.Context) doctor.Result {
			path, err := resolveAuthDB(*sf.authDB, realEnv())
			if err != nil {
				return doctor.Result{Status: doctor.StatusFail, Detail: err.Error()}
			}
			// Local file backends: stat first so a missing db is reported,
			// not silently created on open (keeps doctor read-only).
			if !strings.Contains(path, "://") {
				if _, err := os.Stat(path); err != nil {
					return doctor.Result{Status: doctor.StatusFail, Detail: fmt.Sprintf("not found at %s (created on first serve/user command)", path)}
				}
			}
			s, err := sqlitestore.OpenForInspection(path, sqlitestore.WithMaxConns(*sf.authDBMaxConns))
			if err != nil {
				return doctor.Result{Status: doctor.StatusFail, Detail: err.Error()}
			}
			authStore = s
			var one int
			if err := authStore.DB().QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
				return doctor.Result{Status: doctor.StatusFail, Detail: "ping: " + err.Error()}
			}
			authLive = true
			return doctor.Result{Status: doctor.StatusOK, Detail: path}
		}},

		{Name: "authdb.migrations", Run: func(ctx context.Context) doctor.Result {
			if authStore == nil || !authLive {
				return doctor.Result{Status: doctor.StatusSkip, Detail: "authdb unavailable"}
			}
			have, _ := authStore.SchemaVersion(ctx)
			want := sqlitestore.LatestMigrationVersion()
			switch {
			case have == want:
				return doctor.Result{Status: doctor.StatusOK, Detail: fmt.Sprintf("schema version %d (current)", have)}
			case have < want:
				return doctor.Result{Status: doctor.StatusFail, Detail: fmt.Sprintf("db at version %d, binary expects %d — any serve/CLI command that opens the auth-db will migrate it", have, want)}
			default:
				return doctor.Result{Status: doctor.StatusFail, Detail: fmt.Sprintf("db at version %d, binary only knows %d — this binary is older than the database", have, want)}
			}
		}},

		{Name: "config.lfs", Run: func(ctx context.Context) doctor.Result {
			if !*sf.lfsEnabled {
				return doctor.Result{Status: doctor.StatusSkip, Detail: "--lfs=false"}
			}
			if *sf.proxiedKeyFile == "" || *sf.proxiedBaseURL == "" {
				return doctor.Result{Status: doctor.StatusFail, Detail: "--lfs=true requires both --proxied-url-signing-key and --proxied-url-base (serve refuses to start)"}
			}
			return doctor.Result{Status: doctor.StatusOK, Detail: "signing key + base URL configured"}
		}},

		{Name: "config.proxied", Run: func(ctx context.Context) doctor.Result {
			bMode, ok := gateway.ParseURIMode(*sf.bundleURIMode)
			if !ok {
				return doctor.Result{Status: doctor.StatusFail, Detail: fmt.Sprintf("invalid --bundle-uri-mode %q", *sf.bundleURIMode)}
			}
			pMode, ok := gateway.ParseURIMode(*sf.packURIMode)
			if !ok {
				return doctor.Result{Status: doctor.StatusFail, Detail: fmt.Sprintf("invalid --pack-uri-mode %q", *sf.packURIMode)}
			}
			needsKey := bMode == gateway.URIModeAuto || bMode == gateway.URIModeProxied ||
				pMode == gateway.URIModeAuto || pMode == gateway.URIModeProxied ||
				(*sf.lfsEnabled && *sf.proxiedKeyFile != "" && *sf.proxiedBaseURL != "")
			if !needsKey {
				return doctor.Result{Status: doctor.StatusSkip, Detail: "no proxied/auto URI mode configured"}
			}
			if *sf.proxiedKeyFile == "" || *sf.proxiedBaseURL == "" {
				return doctor.Result{Status: doctor.StatusFail, Detail: "proxied/auto URI mode requires both --proxied-url-signing-key and --proxied-url-base"}
			}
			raw, err := os.ReadFile(*sf.proxiedKeyFile)
			if err != nil {
				return doctor.Result{Status: doctor.StatusFail, Detail: "read signing key: " + err.Error()}
			}
			if n := len(strings.TrimSpace(string(raw))); n < 16 {
				return doctor.Result{Status: doctor.StatusFail, Detail: fmt.Sprintf("signing key too short (%d bytes); need >= 16", n)}
			}
			u, err := url.Parse(*sf.proxiedBaseURL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return doctor.Result{Status: doctor.StatusFail, Detail: fmt.Sprintf("invalid --proxied-url-base %q; want http(s)://host", *sf.proxiedBaseURL)}
			}
			return doctor.Result{Status: doctor.StatusOK, Detail: "signing key readable, base URL parses"}
		}},

		{Name: "config.hooks", Run: func(ctx context.Context) doctor.Result {
			if !*sf.hooksEnabled {
				return doctor.Result{Status: doctor.StatusSkip, Detail: "--hooks-enabled=false"}
			}
			root := *sf.hooksRoot
			if root == "" || !filepath.IsAbs(root) {
				return doctor.Result{Status: doctor.StatusFail, Detail: "--hooks-root must be a non-empty absolute directory"}
			}
			fi, err := os.Stat(root)
			if err != nil || !fi.IsDir() {
				return doctor.Result{Status: doctor.StatusFail, Detail: fmt.Sprintf("--hooks-root %s is not a directory", root)}
			}
			if *sf.hooksUnsafeNoSandbox {
				return doctor.Result{Status: doctor.StatusWarn, Detail: "running without bwrap sandbox — NOT multi-tenant safe"}
			}
			if runtime.GOOS != "linux" {
				return doctor.Result{Status: doctor.StatusFail, Detail: "sandboxed hooks need Linux (bwrap); set --hooks-unsafe-no-sandbox=true elsewhere"}
			}
			p, err := exec.LookPath("bwrap")
			if err != nil {
				return doctor.Result{Status: doctor.StatusFail, Detail: "bwrap not on PATH (sandboxed hooks need bubblewrap)"}
			}
			return doctor.Result{Status: doctor.StatusOK, Detail: "bwrap at " + p}
		}},

		{Name: "deps.git", Run: func(ctx context.Context) doctor.Result {
			p, err := exec.LookPath("git")
			if err != nil {
				return doctor.Result{Status: doctor.StatusFail, Detail: "git not on PATH (import/export/maintenance need it)"}
			}
			vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			out, err := exec.CommandContext(vctx, p, "version").Output()
			if err != nil {
				return doctor.Result{Status: doctor.StatusFail, Detail: "git version: " + err.Error()}
			}
			return doctor.Result{Status: doctor.StatusOK, Detail: strings.TrimSpace(string(out))}
		}},
	}

	if *sf.byobKeyFile != "" {
		rawKey, _ := os.ReadFile(*sf.byobKeyFile)
		rawKey = bytes.TrimSpace(rawKey)
		var encKey []byte
		if len(rawKey) >= 32 {
			encKey = rawKey[:32]
		}
		checks = append(checks, doctor.Check{
			Name: "byob.bindings",
			Run: func(ctx context.Context) doctor.Result {
				if authStore == nil || !authLive {
					return doctor.Result{Status: doctor.StatusSkip, Detail: "authdb unavailable"}
				}
				if len(encKey) < 32 {
					return doctor.Result{Status: doctor.StatusFail, Detail: "--byob-encryption-key file is too short (< 32 bytes)"}
				}
				bindings, err := authStore.ListStorageBindings(ctx)
				if err != nil {
					return doctor.Result{Status: doctor.StatusFail, Detail: "list bindings: " + err.Error()}
				}
				if len(bindings) == 0 {
					return doctor.Result{Status: doctor.StatusOK, Detail: "no bindings configured"}
				}
				const staleThreshold = 30 * 24 * time.Hour
				nowUnix := time.Now().Unix()
				var fails, warns, oks int
				for _, b := range bindings {
					plain, err := byob.Decrypt(encKey, b.CredsJSON)
					if err != nil {
						fails++
						continue
					}
					s, err := openStoreWithCreds(b.StoreURL, plain)
					if err != nil {
						fails++
						continue
					}
					_, lerr := s.List(ctx, "", &storage.ListOptions{MaxKeys: 1})
					closeStore(s)
					if lerr != nil {
						fails++
						continue
					}
					if nowUnix-b.VerifiedAt > int64(staleThreshold.Seconds()) {
						warns++
					} else {
						oks++
					}
				}
				if fails > 0 {
					return doctor.Result{Status: doctor.StatusFail,
						Detail: fmt.Sprintf("%d/%d bindings unreachable or undecodable", fails, len(bindings))}
				}
				if warns > 0 {
					return doctor.Result{Status: doctor.StatusWarn,
						Detail: fmt.Sprintf("%d/%d bindings have stale verified_at (>30 days); run `bucketvcs tenant storage verify`", warns, len(bindings))}
				}
				return doctor.Result{Status: doctor.StatusOK,
					Detail: fmt.Sprintf("%d binding(s) reachable, verified_at current", oks)}
			},
		})
	}

	if *repoFlag != "" {
		checks = append(checks, doctor.Check{Name: "repo." + repoTenant + "/" + repoName, Run: func(ctx context.Context) doctor.Result {
			if store == nil {
				return doctor.Result{Status: doctor.StatusSkip, Detail: "store unavailable"}
			}
			r, err := repo.Open(ctx, store, repoTenant, repoName)
			switch {
			case errors.Is(err, repo.ErrRepoNotFound):
				return doctor.Result{Status: doctor.StatusFail, Detail: "repo not found"}
			case errors.Is(err, repo.ErrUnsupportedSchema):
				return doctor.Result{Status: doctor.StatusFail, Detail: "schema gate: " + err.Error()}
			case err != nil:
				return doctor.Result{Status: doctor.StatusFail, Detail: err.Error()}
			}
			view, err := r.ReadRoot(ctx)
			if err != nil {
				return doctor.Result{Status: doctor.StatusFail, Detail: "read root: " + err.Error()}
			}
			body, err := manifest.UnmarshalBody(view.Body)
			if err != nil {
				return doctor.Result{Status: doctor.StatusFail, Detail: "parse body: " + err.Error()}
			}
			keys := make([]string, 0, 50)
			for _, p := range body.Packs {
				keys = append(keys, p.PackKey, p.IdxKey)
			}
			for _, s := range body.RefShards {
				keys = append(keys, s.Key)
			}
			if len(keys) > 50 {
				keys = keys[:50]
			}
			for _, k := range keys {
				if k == "" {
					continue
				}
				if _, err := store.Head(ctx, k); err != nil {
					return doctor.Result{Status: doctor.StatusFail, Detail: fmt.Sprintf("manifest references missing key %s: %v", k, err)}
				}
			}
			return doctor.Result{Status: doctor.StatusOK, Detail: fmt.Sprintf("schema v%d, %d sampled keys present", view.Header.SchemaVersion, len(keys))}
		}})
	}

	if *sf.replicaOf != "" {
		checks = append(checks,
			doctor.Check{Name: "replica.canonical", Run: func(ctx context.Context) doctor.Result {
				c, err := openStore(*sf.replicaOf)
				if err != nil {
					return doctor.Result{Status: doctor.StatusFail, Detail: err.Error()}
				}
				defer closeStore(c)
				if _, err := c.List(ctx, "", &storage.ListOptions{MaxKeys: 1}); err != nil {
					return doctor.Result{Status: doctor.StatusFail, Detail: "list: " + err.Error()}
				}
				return doctor.Result{Status: doctor.StatusOK, Detail: c.Name() + " backend, list ok"}
			}},
			doctor.Check{Name: "config.replica", Run: func(ctx context.Context) doctor.Result {
				if _, ok := replica.ParseMode(*sf.replicaMode); !ok {
					return doctor.Result{Status: doctor.StatusFail, Detail: "--replica-mode=" + *sf.replicaMode + " must be strong-current|bounded-stale"}
				}
				if *sf.replicaLagBudget < 30*time.Second {
					return doctor.Result{Status: doctor.StatusFail, Detail: "--replica-lag-budget must be >= 30s"}
				}
				path, err := resolveAuthDB(*sf.authDB, realEnv())
				if err == nil && !strings.HasPrefix(path, "postgres://") && !strings.HasPrefix(path, "postgresql://") {
					return doctor.Result{Status: doctor.StatusFail, Detail: "replicas require a postgres --auth-db (shared central pg); got a file/libsql backend"}
				}
				return doctor.Result{Status: doctor.StatusOK, Detail: "mode " + *sf.replicaMode + ", budget " + sf.replicaLagBudget.String()}
			}},
		)
	}

	if failed := doctor.Run(ctx, stdout, *asJSON, checks); failed > 0 {
		fmt.Fprintf(stderr, "doctor: %d check(s) failed\n", failed)
		return 1
	}
	return 0
}
