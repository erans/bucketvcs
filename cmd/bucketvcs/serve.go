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

// runServe is the M3-era serve entry point. M4 Task 23 will rewire it with
// a proper --auth-db flag and remove the deprecated --auth-token /
// --auth-scope flags. For now this is a minimal scaffold that:
//   - keeps cmd/bucketvcs/ buildable (so user/token/repo subcommand tests run)
//   - opens a default sqlitestore for AuthStore (gateway requires it)
//   - accepts but rejects/warns about the deprecated --auth-token /
//     --auth-scope flags so existing serve_test.go behaviour is preserved
func runServe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runServeWithListener(ctx, args, stdout, stderr, nil)
}

func runServeWithListener(ctx context.Context, args []string, stdout, stderr io.Writer, ln net.Listener) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "127.0.0.1:8080", "Listen address (host:port); defaults to loopback to avoid unintended network exposure")
	storeURL := fs.String("store", "", `Store URL (e.g. "localfs:/var/lib/bucketvcs")`)
	mirrorDir := fs.String("mirror-dir", "", "Mirror cache directory (default $XDG_CACHE_HOME/bucketvcs/mirrors)")
	authToken := fs.String("auth-token", "", "DEPRECATED: ignored in M4; configure tokens via `bucketvcs user`/`bucketvcs token`")
	authScope := fs.String("auth-scope", "", "DEPRECATED: ignored in M4; per-repo permissions via `bucketvcs repo grant`")
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
	// Preserve the M3 contract that --auth-scope=all without a token is
	// a usage error. T23 will replace this with --auth-db wiring.
	tok := *authToken
	if tok == "" {
		tok = os.Getenv("BUCKETVCS_AUTH_TOKEN")
	}
	if tok == "" && *authScope != "" && strings.ToLower(*authScope) != "anonymous" {
		fmt.Fprintf(stderr, "serve: --auth-scope=%q without --auth-token\n", *authScope)
		return 2
	}
	if *authToken != "" || *authScope != "" {
		fmt.Fprintln(stderr, "serve: --auth-token/--auth-scope are deprecated in M4; use `bucketvcs user`/`bucketvcs token` (T23 will wire --auth-db)")
	}

	store, err := openStore(*storeURL)
	if err != nil {
		fmt.Fprintf(stderr, "serve: open store: %v\n", err)
		return 1
	}
	_ = store // localfs has no Close

	authS, _, err := openAuthDB("")
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

func defaultMirrorDir() string {
	if x := os.Getenv("XDG_CACHE_HOME"); x != "" {
		return filepath.Join(x, defaultMirrorSubdir)
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".cache", defaultMirrorSubdir)
	}
	return filepath.Join(os.TempDir(), "bucketvcs-mirrors")
}
