package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/gateway"
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
	addr := fs.String("addr", "127.0.0.1:8080", "Listen address (host:port); defaults to loopback to avoid unintended network exposure")
	storeURL := fs.String("store", "", `Store URL (e.g. "localfs:/var/lib/bucketvcs")`)
	mirrorDir := fs.String("mirror-dir", "", "Mirror cache directory (default $XDG_CACHE_HOME/bucketvcs/mirrors)")
	authDB := fs.String("auth-db", "", "Path to auth.db (default: $XDG_STATE_HOME/bucketvcs/bucketvcs.db)")
	maxBody := fs.Int64("max-body-bytes", 1<<30, "Per-request body limit in bytes")
	shutdownTimeout := fs.Duration("shutdown-timeout", 30*time.Second, "Graceful shutdown deadline")
	if err := fs.Parse(args); err != nil {
		return 2
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
	_ = store // localfs has no Close

	authS, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "serve: auth-db: %v\n", err)
		return 1
	}
	defer authS.Close()

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

	httpSrv := &http.Server{Addr: *addr, Handler: srv}
	errCh := make(chan error, 1)
	go func() {
		if ln != nil {
			errCh <- httpSrv.Serve(ln)
		} else {
			errCh <- httpSrv.ListenAndServe()
		}
	}()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return 0
		}
		fmt.Fprintf(stderr, "serve: %v\n", err)
		return 1
	case <-ctx.Done():
	case <-sigCh:
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), *shutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		fmt.Fprintf(stderr, "serve: shutdown: %v\n", err)
		return 1
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
