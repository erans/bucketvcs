package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/gateway"
	"github.com/bucketvcs/bucketvcs/internal/mirror"
	"github.com/bucketvcs/bucketvcs/internal/sshd"
)

const defaultMirrorSubdir = "bucketvcs/mirrors"
const buildVersion = "0.1-dev" // matches gateway agent= advertisement

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
	addr := fs.String("addr", "", "HTTP listen address (host:port); leave empty to disable HTTP (default 127.0.0.1:8080 when --ssh-addr is also absent)")
	storeURL := fs.String("store", "", `Store URL (e.g. "localfs:/var/lib/bucketvcs")`)
	mirrorDir := fs.String("mirror-dir", "", "Mirror cache directory (default $XDG_CACHE_HOME/bucketvcs/mirrors)")
	authDB := fs.String("auth-db", "", "Path to auth.db (default: $XDG_STATE_HOME/bucketvcs/bucketvcs.db)")
	maxBody := fs.Int64("max-body-bytes", 1<<30, "Per-request body limit in bytes")
	shutdownTimeout := fs.Duration("shutdown-timeout", 30*time.Second, "Graceful shutdown deadline")
	// SSH flags.
	sshAddr := fs.String("ssh-addr", "", "SSH listen address, e.g. 127.0.0.1:2222 (empty disables SSH)")
	sshHostKey := fs.String("ssh-host-key", "", "Path to SSH host key (default $XDG_STATE_HOME/bucketvcs/ssh_host_ed25519_key)")
	sshGrace := fs.Duration("ssh-grace", 10*time.Second, "Graceful shutdown deadline for in-flight SSH sessions")

	if err := fs.Parse(args); err != nil {
		return 2
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
	if *mirrorDir == "" {
		*mirrorDir = defaultMirrorDir()
	}

	store, err := openStore(*storeURL)
	if err != nil {
		fmt.Fprintf(stderr, "serve: open store: %v\n", err)
		return 1
	}
	defer closeStore(store)

	authS, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "serve: auth-db: %v\n", err)
		return 1
	}
	defer authS.Close()

	logger := slog.Default()

	// Cancellable context wired to the signal handler so SSH and HTTP share
	// the same shutdown trigger.
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// ---- HTTP listener ----
	var httpSrv *http.Server
	httpErrCh := make(chan error, 1)
	if *addr != "" || ln != nil {
		srv, err := gateway.NewServer(store, gateway.Options{
			MirrorDir:    *mirrorDir,
			Version:      buildVersion,
			AuthStore:    authS,
			MaxBodyBytes: *maxBody,
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
		httpSrv = &http.Server{Addr: listenAddr, Handler: srv}
		go func() {
			if ln != nil {
				httpErrCh <- httpSrv.Serve(ln)
			} else {
				httpErrCh <- httpSrv.ListenAndServe()
			}
		}()
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

		sshSrv, err = sshd.NewServer(sshd.Options{
			Addr:         *sshAddr,
			HostKeyPath:  hostKeyPath,
			Grace:        *sshGrace,
			AgentVersion: buildVersion,
			Store:        authS,
			BVStore:      store,
			Mirror:       sshMirror,
			Logger:       logger,
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
