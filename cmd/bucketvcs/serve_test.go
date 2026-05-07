package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestServeCommand_StartsAndStops(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary + local listener")
	}
	storeDir := t.TempDir()
	mirrorDir := t.TempDir()
	addr := "127.0.0.1:38719"

	args := []string{
		"--addr", addr,
		"--store", "localfs:" + storeDir,
		"--mirror-dir", mirrorDir,
		"--shutdown-timeout", "1s",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		done <- runServe(ctx, args, &stdout, &stderr)
	}()

	// Wait until /healthz responds.
	deadline := time.Now().Add(5 * time.Second)
	ok := false
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				ok = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ok {
		t.Fatalf("server never came up at %s", addr)
	}

	cancel()
	select {
	case code := <-done:
		// runServe should return 0 on graceful shutdown.
		if code != 0 {
			t.Fatalf("runServe exit code: %d, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("runServe did not return after cancel")
	}
}

func TestServeCommand_RejectsMissingStore(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runServe(context.Background(), []string{"--mirror-dir", t.TempDir()}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("expected non-zero exit on missing --store, got 0")
	}
}

func TestServeCommand_RejectsAuthScopeWithoutToken(t *testing.T) {
	storeDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runServe(context.Background(), []string{
		"--store", "localfs:" + storeDir,
		"--mirror-dir", t.TempDir(),
		"--auth-scope", "all",
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("expected non-zero exit on --auth-scope without token, got 0")
	}
}

// keep io import alive (used in earlier test files in this pkg)
var _ = io.Discard
