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

func runServe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runServeWithListener(ctx, args, stdout, stderr, nil)
}

// runServeWithListener is the real implementation. When ln is non-nil the
// server uses Serve(ln) instead of ListenAndServe, which lets tests inject
// an ephemeral listener without touching production flag parsing.
func runServeWithListener(ctx context.Context, args []string, stdout, stderr io.Writer, ln net.Listener) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", ":8080", "Listen address (host:port)")
	storeURL := fs.String("store", "", `Store URL (e.g. "localfs:/var/lib/bucketvcs")`)
	mirrorDir := fs.String("mirror-dir", "", "Mirror cache directory (default $XDG_CACHE_HOME/bucketvcs/mirrors)")
	authToken := fs.String("auth-token", "", "HTTP Basic auth password (user=bucketvcs); also via BUCKETVCS_AUTH_TOKEN env")
	authScope := fs.String("auth-scope", "", `"write-only" (default if --auth-token set) or "all"`)
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
	token := *authToken
	if token == "" {
		token = os.Getenv("BUCKETVCS_AUTH_TOKEN")
	}
	mode := gateway.AuthAnonymous
	if token != "" {
		switch strings.ToLower(*authScope) {
		case "", "write-only":
			mode = gateway.AuthWriteOnly
		case "all":
			mode = gateway.AuthAll
		default:
			fmt.Fprintf(stderr, "serve: invalid --auth-scope %q\n", *authScope)
			return 2
		}
	} else if *authScope != "" && strings.ToLower(*authScope) != "anonymous" {
		fmt.Fprintf(stderr, "serve: --auth-scope=%q without --auth-token\n", *authScope)
		return 2
	}

	store, err := openStore(*storeURL)
	if err != nil {
		fmt.Fprintf(stderr, "serve: open store: %v\n", err)
		return 1
	}
	defer closeStore(store)

	srv, err := gateway.NewServer(store, gateway.Options{
		MirrorDir:    *mirrorDir,
		Version:      buildVersion,
		AuthMode:     mode,
		AuthToken:    token,
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
