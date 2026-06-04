package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/ratelimit"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/gateway"
	"github.com/bucketvcs/bucketvcs/internal/gitbrowse"
	"github.com/bucketvcs/bucketvcs/internal/hooks"
	"github.com/bucketvcs/bucketvcs/internal/lfs"
	"github.com/bucketvcs/bucketvcs/internal/lfs/locks"
	"github.com/bucketvcs/bucketvcs/internal/lfs/quota"
	"github.com/bucketvcs/bucketvcs/internal/mirror"
	"github.com/bucketvcs/bucketvcs/internal/oidc"
	"github.com/bucketvcs/bucketvcs/internal/policy"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/sshd"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/web"
	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

const defaultMirrorSubdir = "bucketvcs/mirrors"

// buildVersion is the version advertised by the gateway (agent=) and reported
// by the CLI. Defaults to a dev marker; overridden at release time via
// -ldflags "-X main.buildVersion=<tag>".
var buildVersion = "0.1-dev" // matches gateway agent= advertisement

// runServe is the M4 serve entry point. The legacy M3-era
// --auth-token / --auth-scope / --auth-mode flags were removed; auth is
// now driven by the SQLite auth.db (managed via `bucketvcs user`,
// `bucketvcs token`, and `bucketvcs repo`). Pass --auth-db <path> to
// override the default location resolved by openAuthDB.
func runServe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runServeWithListener(ctx, args, stdout, stderr, nil)
}

func runServeWithListener(ctx context.Context, args []string, stdout, stderr io.Writer, ln net.Listener) int {
	// Fail-fast on legacy flags BEFORE flag.Parse so the error message
	// is actionable instead of a generic "flag provided but not defined".
	if rc := rejectLegacyAuthFlags(args, stderr); rc != 0 {
		return rc
	}

	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sf := registerServeFlags(fs)
	addr, storeURL, mirrorDir, authDB := sf.addr, sf.storeURL, sf.mirrorDir, sf.authDB
	authDBMaxConns, maxBody, shutdownTimeout := sf.authDBMaxConns, sf.maxBody, sf.shutdownTimeout
	sshAddr, sshHostKey, sshGrace := sf.sshAddr, sf.sshHostKey, sf.sshGrace
	bundleURIMode, packURIMode, proxiedKeyFile := sf.bundleURIMode, sf.packURIMode, sf.proxiedKeyFile
	proxiedBundleTTL, proxiedPackTTL, warmCommits, warmAge := sf.proxiedBundleTTL, sf.proxiedPackTTL, sf.warmCommits, sf.warmAge
	proxiedBaseURL := sf.proxiedBaseURL
	lfsEnabled, lfsPresignTTL, lfsSSHTokenTTL := sf.lfsEnabled, sf.lfsPresignTTL, sf.lfsSSHTokenTTL
	authRateLimitBurst, authRateLimitRefillPerMin := sf.authRateLimitBurst, sf.authRateLimitRefillPerMin
	trustProxyHeaders, authRateLimitDisabled := sf.trustProxyHeaders, sf.authRateLimitDisabled
	hooksEnabled, hooksRoot, hooksUnsafeNoSandbox := sf.hooksEnabled, sf.hooksRoot, sf.hooksUnsafeNoSandbox
	hooksOnInternalError, hooksTimeoutSec, hooksCPUSec := sf.hooksOnInternalError, sf.hooksTimeoutSec, sf.hooksCPUSec
	hooksMemoryMB, hooksOutputMaxKB := sf.hooksMemoryMB, sf.hooksOutputMaxKB
	hooksAllowNetwork, hooksEnv := sf.hooksAllowNetwork, sf.hooksEnv
	hooksPostReceiveConcurrency, hooksPostReceiveQueue := sf.hooksPostReceiveConcurrency, sf.hooksPostReceiveQueue
	oidcEnabled, oidcSweepInterval := sf.oidcEnabled, sf.oidcSweepInterval
	uiEnabled, uiAddr, uiDir, uiSessionTTL, uiBrowseTimeout := sf.uiEnabled, sf.uiAddr, sf.uiDir, sf.uiSessionTTL, sf.uiBrowseTimeout
	oidcLogin, oidcIssuer, oidcClientID := sf.oidcLogin, sf.oidcIssuer, sf.oidcClientID
	oidcSecretFile, oidcRedirect, oidcScopes, oidcLabel := sf.oidcSecretFile, sf.oidcRedirect, sf.oidcScopes, sf.oidcLabel

	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *lfsEnabled {
		if *proxiedKeyFile == "" || *proxiedBaseURL == "" {
			fmt.Fprintln(stderr, "serve: --lfs=true requires both --proxied-url-signing-key and --proxied-url-base. The LFS verify action mints HMAC tokens regardless of which backend serves the upload, so the proxied-URL config is mandatory whenever LFS is enabled. Set both flags or pass --lfs=false.")
			return 2
		}
	}

	bMode, ok := gateway.ParseURIMode(*bundleURIMode)
	if !ok {
		fmt.Fprintf(stderr, "serve: --bundle-uri-mode=%q must be one of auto|direct|proxied|off\n", *bundleURIMode)
		return 2
	}
	pMode, ok := gateway.ParseURIMode(*packURIMode)
	if !ok {
		fmt.Fprintf(stderr, "serve: --pack-uri-mode=%q must be one of auto|direct|proxied|off\n", *packURIMode)
		return 2
	}
	needsKey := bMode == gateway.URIModeAuto || bMode == gateway.URIModeProxied ||
		pMode == gateway.URIModeAuto || pMode == gateway.URIModeProxied
	// LFS on a non-presigning backend (e.g. localfs) also needs the
	// proxied-URL key: WithProxied falls back to /_lfs/ URLs minted with
	// this key. Without it, Batch returns per-object 503.
	if *lfsEnabled && *proxiedKeyFile != "" && *proxiedBaseURL != "" {
		needsKey = true
	}
	var signingKey []byte
	if needsKey {
		if *proxiedKeyFile == "" {
			fmt.Fprintln(stderr, "serve: --proxied-url-signing-key is required when bundle-uri-mode or pack-uri-mode is auto or proxied")
			return 2
		}
		if *proxiedBaseURL == "" {
			fmt.Fprintln(stderr, "serve: --proxied-url-base is required when bundle-uri-mode or pack-uri-mode is auto or proxied")
			return 2
		}
		raw, err := os.ReadFile(*proxiedKeyFile)
		if err != nil {
			fmt.Fprintf(stderr, "serve: read --proxied-url-signing-key: %v\n", err)
			return 1
		}
		raw = bytes.TrimSpace(raw)
		if len(raw) < 16 {
			fmt.Fprintf(stderr, "serve: --proxied-url-signing-key file contents too short (%d bytes); need >= 16\n", len(raw))
			return 2
		}
		signingKey = raw
	}

	// Apply the legacy default: when the user passes neither --addr nor
	// --ssh-addr, default HTTP to loopback (matches historical behaviour).
	if *addr == "" && *sshAddr == "" {
		if ln != nil {
			// A test has pre-bound a listener; honour that.
			*addr = ln.Addr().String()
		} else {
			fmt.Fprintln(stderr, "serve: at least one of --addr or --ssh-addr must be set")
			return 2
		}
	}

	if *storeURL == "" {
		fmt.Fprintln(stderr, "serve: --store is required")
		return 2
	}
	if *oidcLogin {
		if *oidcIssuer == "" || *oidcClientID == "" || *oidcRedirect == "" {
			fmt.Fprintln(stderr, "serve: --oidc-login requires --oidc-login-issuer, --oidc-login-client-id, and --oidc-login-redirect-url")
			return 2
		}
	}
	if *mirrorDir == "" {
		*mirrorDir = defaultMirrorDir()
	}

	store, err := openStore(*storeURL)
	if err != nil {
		fmt.Fprintf(stderr, "serve: open store: %v\n", err)
		return 1
	}
	defer closeStore(store)

	authS, _, err := openAuthDB(*authDB, sqlitestore.WithMaxConns(*authDBMaxConns))
	if err != nil {
		fmt.Fprintf(stderr, "serve: auth-db: %v\n", err)
		return 1
	}
	defer authS.Close()

	logger := slog.Default()

	// M24: the web UI is served only on an HTTP listener. If an operator asked
	// for a separate --ui-addr but provided no main HTTP listener (--addr), the
	// UI block below is skipped and nothing is served on --ui-addr — warn loudly
	// rather than fail silently.
	if *uiEnabled && *uiAddr != "" && *addr == "" {
		logger.Warn("web UI not served: --ui-addr requires a main HTTP listener (--addr); the UI shares or sits alongside --addr",
			"ui_addr", *uiAddr)
	}
	if *oidcLogin && !*uiEnabled {
		logger.Warn("--oidc-login has no effect when --ui=false; OIDC login routes will not be served")
	}

	// M14 protected-refs enforcement. Always constructed against the
	// same authdb the gateway uses; when the operator has added no
	// rules via `bucketvcs policy refs add`, CheckUpdate's List call
	// returns zero rows and the engine accepts every update.
	policySvc := policy.New(authS.DB())

	// M15 webhook service. Backed by the same authdb (webhook_endpoints +
	// webhook_deliveries tables added by migration 0006). The worker is
	// started below once HTTP and SSH listeners are configured.
	webhookSvc := webhooks.New(authS.DB())

	// M24 Phase 3 web admin services. Both are db-backed and cheap to
	// construct unconditionally; the web layer only invokes them on demand
	// from the settings/admin pages. hooksStore is the CRUD Store (distinct
	// from the optional hooksSvc Runner constructed below for receive-pack).
	// quotaSvc is gated on --lfs: M13.5 quota enforcement lives in the LFS
	// Batch handler, so with LFS off a configured quota would be inert —
	// leaving the web Deps nil makes the quota pages degrade to their
	// "unavailable" notices instead of offering unenforceable knobs.
	hooksStore := hooks.NewStore(authS.DB())
	var quotaSvc *quota.Service
	if *lfsEnabled {
		quotaSvc = quota.New(authS.DB(), logger)
	}

	// M20 Tier 3 hooks service (optional). Constructed only when
	// --hooks-enabled=true. bwrap detection: on Linux, the binary must
	// be on PATH unless --hooks-unsafe-no-sandbox=true is also set. On
	// non-Linux, --hooks-unsafe-no-sandbox=true is required (we refuse
	// to start otherwise). When nil, EngineRequest.Hooks stays nil and
	// receivepack's Step 8c is a no-op.
	var hooksSvc *hooks.Service
	if *hooksEnabled {
		if *hooksRoot == "" {
			fmt.Fprintln(stderr, "serve: --hooks-enabled requires --hooks-root")
			return 2
		}
		if !filepath.IsAbs(*hooksRoot) {
			fmt.Fprintf(stderr, "serve: --hooks-root must be an absolute path: %s\n", *hooksRoot)
			return 2
		}
		if st, err := os.Stat(*hooksRoot); err != nil || !st.IsDir() {
			fmt.Fprintf(stderr, "serve: --hooks-root must be an existing directory: %s\n", *hooksRoot)
			return 2
		}
		useSandbox := !*hooksUnsafeNoSandbox
		var bwrapPath string
		if useSandbox {
			if runtime.GOOS != "linux" {
				fmt.Fprintf(stderr, "serve: --hooks-enabled on %s requires --hooks-unsafe-no-sandbox=true (bwrap is Linux-only)\n", runtime.GOOS)
				return 2
			}
			p, err := exec.LookPath("bwrap")
			if err != nil {
				fmt.Fprintf(stderr, "serve: --hooks-enabled requires bwrap on PATH (install bubblewrap) or set --hooks-unsafe-no-sandbox=true: %v\n", err)
				return 2
			}
			bwrapPath = p
		} else {
			logger.Error("hooks: running without sandbox; NOT multi-tenant safe; for single-tenant local development only")
		}
		allowNet := make(map[string]struct{})
		for _, n := range splitCSV(*hooksAllowNetwork) {
			allowNet[n] = struct{}{}
		}
		// --hooks-env entries must be KEY=VALUE. Reject entries without an
		// `=` (would otherwise become a malformed env entry that confuses
		// downstream processes). Values containing `,` are not supported
		// because the flag is comma-split; document this limit in --help.
		var extraEnv []string
		for _, kv := range splitCSV(*hooksEnv) {
			if !strings.Contains(kv, "=") {
				fmt.Fprintf(stderr, "serve: --hooks-env entry %q is not KEY=VALUE\n", kv)
				return 2
			}
			extraEnv = append(extraEnv, kv)
		}
		var onErr hooks.InternalErrorBehavior
		switch *hooksOnInternalError {
		case "reject":
			onErr = hooks.InternalErrorReject
		case "allow":
			onErr = hooks.InternalErrorAllow
		default:
			fmt.Fprintf(stderr, "serve: --hooks-on-internal-error must be reject|allow, got %q\n", *hooksOnInternalError)
			return 2
		}
		runnerCfg := hooks.RunnerConfig{
			HooksRoot:       *hooksRoot,
			UseSandbox:      useSandbox,
			BwrapPath:       bwrapPath,
			TimeoutSec:      *hooksTimeoutSec,
			CPUSec:          *hooksCPUSec,
			MemoryMB:        *hooksMemoryMB,
			OutputMaxKB:     *hooksOutputMaxKB,
			AllowNetworkSet: allowNet,
			ExtraEnv:        extraEnv,
			Logger:          logger,
		}
		svcCfg := hooks.ServiceConfig{
			PostReceiveConcurrency: *hooksPostReceiveConcurrency,
			PostReceiveQueueSize:   *hooksPostReceiveQueue,
			OnInternalError:        onErr,
			Logger:                 logger,
		}
		hooksSvc = hooks.NewService(hooks.NewStore(authS.DB()), runnerCfg, svcCfg)
		defer hooksSvc.Close()
	}

	// M18 auth rate-limiter. Shared between the HTTPS gateway and the SSH
	// server so a single attacker hitting both transports converges on the
	// same per-IP bucket. Nil disables enforcement on both ends.
	var rateLimiter *ratelimit.Limiter
	if !*authRateLimitDisabled {
		rateLimiter = ratelimit.NewLimiter(ratelimit.Config{
			Burst:           *authRateLimitBurst,
			RefillPerMinute: *authRateLimitRefillPerMin,
			SweepInterval:   5 * time.Minute,
			Now:             time.Now,
		})
		defer rateLimiter.Close()
		if !*trustProxyHeaders {
			// Common deployment foot-gun: gateway behind a reverse proxy /
			// load balancer + trustProxyHeaders=false means every request
			// is keyed on the single proxy IP. Burst credential failures
			// from any attacker fill the shared bucket and 429 every
			// client behind that proxy — a self-inflicted DoS. We can't
			// detect "behind a proxy" automatically, so emit a one-time
			// startup WARN that the operator can grep for.
			logger.Warn(
				"M18 rate-limit: --trust-proxy-headers=false. If bucketvcs runs behind a reverse proxy / load balancer, every request will be keyed on the single proxy IP and Burst credential failures from any attacker will 429 every other client behind that proxy. Enable --trust-proxy-headers when behind a trusted proxy; ignore this warning when listening on a public interface directly.",
			)
		}
	}

	// Build URLBuilder-backed closures once and share between the HTTP
	// gateway and the SSH listener; minting and serving use the same key.
	// Building here (rather than letting gateway construct them internally)
	// keeps the wiring symmetric across transports — both ends mint
	// identical URLs from the same builder state — and avoids exposing
	// gateway internals to sshd.
	var bundleBuildURL, packBuildURL func(ctx context.Context, tenant, repo, hash, storageKey, expectedHash string) (string, error)
	if bMode != gateway.URIModeOff {
		bub := &gateway.URLBuilder{
			Store:          store,
			ProxiedKey:     signingKey,
			ProxiedBaseURL: *proxiedBaseURL,
			BundleTTL:      *proxiedBundleTTL,
			Mode:           bMode,
		}
		bundleBuildURL = func(ctx context.Context, tenant, repo, hash, key, expected string) (string, error) {
			u, _, err := bub.BuildBundleURL(ctx, tenant, repo, hash, key, expected)
			return u, err
		}
	}
	if pMode != gateway.URIModeOff {
		pub := &gateway.URLBuilder{
			Store:          store,
			ProxiedKey:     signingKey,
			ProxiedBaseURL: *proxiedBaseURL,
			PackTTL:        *proxiedPackTTL,
			Mode:           pMode,
		}
		packBuildURL = func(ctx context.Context, tenant, repo, hash, key, expected string) (string, error) {
			u, _, err := pub.BuildPackURL(ctx, tenant, repo, hash, key, expected)
			return u, err
		}
	}

	// Cancellable context wired to the signal handler so SSH and HTTP share
	// the same shutdown trigger.
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// M15 webhook delivery worker. Runs for the lifetime of serveCtx;
	// returns when ctx is cancelled at shutdown. Backed by the same
	// authdb as enqueue, so no extra DB handle is needed.
	go webhooks.StartWorker(serveCtx, webhookSvc, webhooks.DefaultWorkerConfig())

	// M22 OIDC expired-token sweep goroutine.
	if *oidcEnabled {
		if *addr == "" && ln == nil {
			logger.LogAttrs(serveCtx, slog.LevelWarn,
				"oidc enabled but no HTTP listener configured; POST /_oidc/token will not be served (sweep still runs)")
		}
		go func() {
			t := time.NewTicker(*oidcSweepInterval)
			defer t.Stop()
			for {
				select {
				case <-serveCtx.Done():
					return
				case <-t.C:
					n, err := authS.SweepExpiredOIDCTokens(serveCtx)
					if err != nil {
						logger.LogAttrs(serveCtx, slog.LevelWarn, "oidc sweep error", slog.String("err", err.Error()))
						continue
					}
					if n > 0 {
						logger.LogAttrs(serveCtx, slog.LevelInfo, "metric",
							slog.String("metric_name", "oidc_tokens_swept_total"), slog.Int64("value", n))
					}
				}
			}
		}()
	}

	// M24 web-UI session sweeper. Deletes expired sessions on a fixed tick;
	// runs for the lifetime of serveCtx.
	if *uiEnabled {
		go func() {
			t := time.NewTicker(10 * time.Minute)
			defer t.Stop()
			for {
				select {
				case <-serveCtx.Done():
					return
				case <-t.C:
					if n, err := authS.SweepExpiredSessions(serveCtx, time.Now()); err != nil {
						logger.Warn("session sweep failed", "err", err)
					} else if n > 0 {
						logger.Info("swept expired sessions", "count", n)
					}
				}
			}
		}()
	}

	// ---- HTTP listener ----
	var httpSrv *http.Server
	var uiSrv *http.Server
	// Buffered for 2 potential senders: the main HTTP listener and, when
	// --ui-addr is set, a separate web-UI listener.
	httpErrCh := make(chan error, 2)
	if *addr != "" || ln != nil {
		// LFS Locks store (M13.3) shares the authdb sqlite handle. Only
		// constructed when LFS is enabled — when --lfs=false the locks
		// routes return 503 (server.go's LocksStore=nil gate).
		var lfsLocksStore *locks.Store
		if *lfsEnabled {
			lfsLocksStore = locks.New(authS)
		}

		// M22 OIDC verifier + store adapters (nil when --oidc=false).
		var oidcVerifier gateway.OIDCVerifier
		var oidcStore gateway.OIDCExchangeStore
		if *oidcEnabled {
			oidcVerifier = oidcVerifierAdapter{oidc.NewVerifier()}
			oidcStore = oidcStoreAdapter{authS}
		}

		srv, err := gateway.NewServer(store, gateway.Options{
			MirrorDir:               *mirrorDir,
			Version:                 buildVersion,
			AuthStore:               authS,
			MaxBodyBytes:            *maxBody,
			BundleURIEnabled:        bundleBuildURL != nil,
			BundleURIBuildURL:       bundleBuildURL,
			BundleWarmCommits:       *warmCommits,
			BundleWarmAge:           *warmAge,
			PackURIEnabled:          packBuildURL != nil,
			PackURIBuildURL:         packBuildURL,
			ProxiedURLSigningKey:    signingKey,
			ProxiedBaseURL:          *proxiedBaseURL,
			LFSEnabled:              *lfsEnabled,
			LFSPresignTTL:           *lfsPresignTTL,
			LFSProxiedURLSigningKey: signingKey,
			LFSProxiedBaseURL:       *proxiedBaseURL,
			LFSLocksStore:           lfsLocksStore,
			Policy:                  policySvc,
			Webhooks:                webhookSvc,
			Hooks:                   hooksSvc,
			Limiter:                 rateLimiter,
			TrustProxyHeaders:       *trustProxyHeaders,
			OIDCEnabled:             *oidcEnabled,
			OIDCStore:               oidcStore,
			OIDCVerifier:            oidcVerifier,
		})
		if err != nil {
			fmt.Fprintf(stderr, "serve: NewServer: %v\n", err)
			return 1
		}
		defer srv.Close()

		listenAddr := *addr
		if ln != nil {
			listenAddr = ln.Addr().String()
		}

		// Web UI handler (M24): mounted on the same listener as git via a
		// dispatcher, or on its own --ui-addr listener.
		var uiHandler http.Handler
		if *uiEnabled {
			var oidcProvider *web.OIDCProvider
			if *oidcLogin {
				secret, serr := resolveOIDCClientSecret(*oidcSecretFile, os.Getenv)
				if serr != nil {
					fmt.Fprintf(stderr, "serve: %v\n", serr)
					return 1
				}
				md, derr := oidc.Discover(serveCtx, http.DefaultClient, *oidcIssuer)
				if derr != nil {
					fmt.Fprintf(stderr, "serve: oidc discovery for %s: %v\n", *oidcIssuer, derr)
					return 1
				}
				hmacKey := make([]byte, 32)
				if _, kerr := rand.Read(hmacKey); kerr != nil {
					fmt.Fprintf(stderr, "serve: oidc hmac key: %v\n", kerr)
					return 1
				}
				oidcProvider = &web.OIDCProvider{
					Issuer:      *oidcIssuer,
					ClientID:    *oidcClientID,
					Secret:      secret,
					AuthURL:     md.AuthorizationEndpoint,
					TokenURL:    md.TokenEndpoint,
					RedirectURL: *oidcRedirect,
					Scopes:      splitCSV(*oidcScopes),
					Label:       *oidcLabel,
					HMACKey:     hmacKey,
				}
				logger.Info("oidc browser login enabled", "issuer", *oidcIssuer)
			}
			browseSvc := gitbrowse.NewService(store, srv.MirrorManager(), *uiBrowseTimeout, logger)
			webDeps := web.Deps{
				Store:      newWebAdapter(authS),
				Logger:     logger,
				Limiter:    rateLimiter,
				UIDir:      *uiDir,
				SessionTTL: *uiSessionTTL,
				TrustProxy: *trustProxyHeaders,
				OIDC:       oidcProvider,
				Content:    browseSvc,
				Webhooks:   webhookSvc,
				Policy:     policySvc,
				Hooks:      hooksStore,
				RepoInit: func(ctx context.Context, tenant, repoName, actor string) error {
					// Mirrors `bucketvcs init` defaults (cmd/bucketvcs/init.go).
					_, err := repo.Create(ctx, store, tenant, repoName, repo.CreateOptions{
						DefaultBranch: "refs/heads/main",
						Actor:         actor,
					})
					return err
				},
				RenameCheck: func(ctx context.Context, tenant, newName string) error {
					// Mirrors `bucketvcs repo rename` pre-check 3
					// (cmd/bucketvcs/repo_rename.go): M21 rename does NOT migrate
					// storage keys, so refuse if any object already lives under the
					// destination prefix to avoid a confused read after rename.
					// keys.NewRepo(...).Prefix() == "tenants/<t>/repos/<new>/", the
					// SAME literal the CLI builds.
					rk, kerr := keys.NewRepo(tenant, newName)
					if kerr != nil {
						return fmt.Errorf("rename: storage key: %w", kerr)
					}
					destPrefix := rk.Prefix()
					page, lerr := store.List(ctx, destPrefix, &storage.ListOptions{MaxKeys: 1})
					if lerr != nil {
						return fmt.Errorf("rename: storage collision check: %w", lerr)
					}
					if page != nil && len(page.Objects) > 0 {
						return fmt.Errorf("rename: storage prefix %s is non-empty (first key: %s); refusing to rename",
							destPrefix, page.Objects[0].Key)
					}
					return nil
				},
			}
			// Quota pages only when LFS is on (see quotaSvc construction).
			// Assigned conditionally: a typed-nil *quota.Service stored in the
			// QuotaAdmin interface would defeat the web layer's nil checks.
			if quotaSvc != nil {
				webDeps.Quotas = quotaSvc
				webDeps.QuotaReconcile = func(ctx context.Context, tenant string, dryRun bool) (quota.Report, error) {
					return quotaSvc.Reconcile(ctx, store, tenant, dryRun)
				}
			}
			uiHandler = web.NewHandler(webDeps)
		}

		var mainHandler http.Handler = srv // gateway
		if uiHandler != nil && *uiAddr == "" {
			mainHandler = web.Dispatcher(srv, uiHandler) // shared listener: git vs UI
		}
		httpSrv = &http.Server{Addr: listenAddr, Handler: mainHandler}
		go func() {
			if ln != nil {
				httpErrCh <- httpSrv.Serve(ln)
			} else {
				httpErrCh <- httpSrv.ListenAndServe()
			}
		}()

		// Optional separate UI listener.
		if uiHandler != nil && *uiAddr != "" {
			uiSrv = &http.Server{Addr: *uiAddr, Handler: uiHandler}
			go func() { httpErrCh <- uiSrv.ListenAndServe() }()
		}
	}

	// ---- SSH listener ----
	var sshSrv *sshd.Server
	if *sshAddr != "" {
		hostKeyPath, err := resolveHostKey(*sshHostKey, realEnv())
		if err != nil {
			fmt.Fprintf(stderr, "serve: %v\n", err)
			return 2
		}
		// The state directory must exist for first-run host-key generation.
		if err := os.MkdirAll(filepath.Dir(hostKeyPath), 0o755); err != nil {
			fmt.Fprintf(stderr, "serve: mkdir for host key: %v\n", err)
			return 1
		}
		// SSH needs its own mirror.Manager rooted in a subdirectory so the
		// two managers do not collide on the process-wide flock that
		// mirror.NewManager acquires per rootDir.
		sshMirrorDir := filepath.Join(*mirrorDir, "ssh")
		sshMirror, err := mirror.NewManager(sshMirrorDir, store)
		if err != nil {
			fmt.Fprintf(stderr, "serve: ssh mirror manager: %v\n", err)
			return 1
		}
		defer sshMirror.Close()

		// SSH-side LFS: git-lfs-authenticate mints a bearer + Href back to
		// this gateway. The Href needs an external base URL (the SSH
		// session has no inbound Host header to fall back to). When
		// --lfs=true but --proxied-url-base is unset, the SSH command is
		// disabled (warn but do not fail-start; HTTPS LFS continues to
		// work via the inbound request host).
		if *lfsEnabled && *proxiedBaseURL == "" {
			fmt.Fprintln(stderr, "serve: --lfs is enabled but --proxied-url-base is unset; the SSH git-lfs-authenticate command will be disabled. HTTPS LFS continues to work via the inbound request host.")
		}
		var lfsIssuer lfs.TokenIssuer
		var lfsBaseURL string
		var lfsSSHTTL time.Duration
		if *lfsEnabled && *proxiedBaseURL != "" {
			lfsIssuer = authS
			lfsBaseURL = *proxiedBaseURL
			lfsSSHTTL = *lfsSSHTokenTTL
		}

		sshSrv, err = sshd.NewServer(sshd.Options{
			Addr:              *sshAddr,
			HostKeyPath:       hostKeyPath,
			Grace:             *sshGrace,
			AgentVersion:      buildVersion,
			Store:             authS,
			BVStore:           store,
			Mirror:            sshMirror,
			Logger:            logger,
			BundleURIEnabled:  bundleBuildURL != nil,
			BundleURIBuildURL: bundleBuildURL,
			BundleWarmCommits: *warmCommits,
			BundleWarmAge:     *warmAge,
			PackURIEnabled:    packBuildURL != nil,
			PackURIBuildURL:   packBuildURL,
			LFSTokenIssuer:    lfsIssuer,
			LFSBaseURL:        lfsBaseURL,
			LFSSSHTokenTTL:    lfsSSHTTL,
			Policy:            policySvc,
			Webhooks:          webhookSvc,
			Hooks:             hooksSvc,
			Limiter:           rateLimiter,
		})
		if err != nil {
			fmt.Fprintf(stderr, "serve: ssh new server: %v\n", err)
			return 1
		}
		if err := sshSrv.Listen(); err != nil {
			fmt.Fprintf(stderr, "serve: ssh listen: %v\n", err)
			return 1
		}
		go func() {
			if err := sshSrv.Serve(serveCtx); err != nil {
				fmt.Fprintf(stderr, "serve: ssh: %v\n", err)
				// Trigger global shutdown so HTTP also stops.
				cancel()
			}
		}()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	// Wait for: HTTP error, signal, or parent ctx cancellation.
	select {
	case err := <-httpErrCh:
		if !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(stderr, "serve: %v\n", err)
			cancel()
			if sshSrv != nil {
				_ = sshSrv.Close()
			}
			return 1
		}
		// HTTP shut down cleanly (e.g. triggered by Shutdown below); fall
		// through to also stop SSH.
	case <-serveCtx.Done():
		// Either parent ctx canceled (test harness) or SSH goroutine called cancel().
	case <-sigCh:
		cancel()
	}

	// ---- Graceful shutdown ----

	// SSH: Close() uses its own Grace deadline internally.
	if sshSrv != nil {
		if err := sshSrv.Close(); err != nil {
			fmt.Fprintf(stderr, "serve: ssh close: %v\n", err)
		}
	}

	// HTTP: use the configured shutdown timeout.
	if httpSrv != nil {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), *shutdownTimeout)
		defer shutdownCancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(stderr, "serve: http shutdown: %v\n", err)
			return 1
		}
		if uiSrv != nil {
			_ = uiSrv.Shutdown(shutdownCtx)
		}
		// Drain the httpErrCh in case we never received from it above.
		select {
		case <-httpErrCh:
		default:
		}
	}

	return 0
}

// rejectLegacyAuthFlags prints a migration message and returns 2 if any
// of the M3-era auth flags are present. It must run before flag.Parse so
// the error is meaningful (the new flag set does not define these).
func rejectLegacyAuthFlags(args []string, stderr io.Writer) int {
	for _, a := range args {
		if a == "--auth-mode" || a == "--auth-token" || a == "--auth-scope" ||
			a == "-auth-mode" || a == "-auth-token" || a == "-auth-scope" ||
			strings.HasPrefix(a, "--auth-mode=") ||
			strings.HasPrefix(a, "--auth-token=") ||
			strings.HasPrefix(a, "--auth-scope=") ||
			strings.HasPrefix(a, "-auth-mode=") ||
			strings.HasPrefix(a, "-auth-token=") ||
			strings.HasPrefix(a, "-auth-scope=") {
			fmt.Fprintln(stderr, "bucketvcs serve: --auth-mode/--auth-token/--auth-scope were removed in M4.")
			fmt.Fprintln(stderr, "Configure auth via `bucketvcs user`, `bucketvcs token`, and `bucketvcs repo`.")
			fmt.Fprintln(stderr, "See docs/migration-m3-to-m4.md (created in Task 27).")
			return 2
		}
	}
	return 0
}

func defaultMirrorDir() string {
	if x := os.Getenv("XDG_CACHE_HOME"); x != "" {
		return filepath.Join(x, defaultMirrorSubdir)
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".cache", defaultMirrorSubdir)
	}
	return filepath.Join(os.TempDir(), "bucketvcs-mirrors")
}

// splitCSV splits a comma-separated string, trimming whitespace and dropping
// empty entries. Returns nil for the empty input. Used by M20 hooks flags
// (--hooks-allow-network, --hooks-env).
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// oidcStoreAdapter adapts *sqlitestore.Store to gateway.OIDCExchangeStore,
// translating the flat MintOIDCToken args to sqlitestore.MintOIDCParams.
type oidcStoreAdapter struct{ s *sqlitestore.Store }

func (a oidcStoreAdapter) FindOIDCIssuerByURL(ctx context.Context, u string) (auth.OIDCIssuer, error) {
	return a.s.FindOIDCIssuerByURL(ctx, u)
}
func (a oidcStoreAdapter) ListOIDCRulesForIssuer(ctx context.Context, alias string) ([]auth.OIDCTrustRule, error) {
	return a.s.ListOIDCRulesForIssuer(ctx, alias)
}
func (a oidcStoreAdapter) MintOIDCToken(ctx context.Context, tenant, repo string, perm auth.Perm,
	scopes auth.TokenScope, ttl int64, label string) (string, error) {
	return a.s.MintOIDCToken(ctx, sqlitestore.MintOIDCParams{
		Tenant: tenant, Repo: repo, Perm: perm, Scopes: scopes, TTLSeconds: ttl, Label: label,
	})
}

// oidcVerifierAdapter adapts *oidc.Verifier to gateway.OIDCVerifier
// (named-type oidc.Claims -> map[string]any).
type oidcVerifierAdapter struct{ v *oidc.Verifier }

func (a oidcVerifierAdapter) Verify(ctx context.Context, raw, issuer string) (map[string]any, error) {
	c, err := a.v.Verify(ctx, raw, issuer)
	return map[string]any(c), err
}
