package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestServeCommand_StartsAndStops(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary + local listener")
	}
	_ = userCmdEnv(t) // tmp HOME so the default auth-db lands in a clean place
	storeDir := t.TempDir()
	mirrorDir := t.TempDir()

	// Allocate an ephemeral port up front to avoid hard-coded port conflicts
	// on developer machines and CI.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

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
		// Pass the pre-bound listener so runServeWithListener uses Serve(ln)
		// rather than ListenAndServe, which would open a second socket on the
		// same address and race with our Addr.String() read.
		done <- runServeWithListener(ctx, args, &stdout, &stderr, ln)
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
	_ = userCmdEnv(t)
	var stdout, stderr bytes.Buffer
	code := runServe(context.Background(), []string{"--mirror-dir", t.TempDir()}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("expected non-zero exit on missing --store, got 0")
	}
}

func TestServe_RejectsLegacyAuthModeFlag(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rc := runServe(context.Background(), []string{"--auth-mode", "all"}, stdout, stderr)
	if rc != 2 {
		t.Fatalf("rc = %d, want 2", rc)
	}
	if !strings.Contains(stderr.String(), "M4") {
		t.Fatalf("stderr should explain M4 removal: %q", stderr.String())
	}
}

func TestServe_RejectsLegacyAuthModeFlag_Equals(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rc := runServe(context.Background(), []string{"--auth-mode=all"}, stdout, stderr)
	if rc != 2 {
		t.Fatalf("rc = %d, want 2", rc)
	}
	if !strings.Contains(stderr.String(), "M4") {
		t.Fatalf("stderr should explain M4 removal: %q", stderr.String())
	}
}

func TestServe_RejectsLegacyAuthTokenFlag(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rc := runServe(context.Background(), []string{"--auth-token=secret"}, stdout, stderr)
	if rc != 2 {
		t.Fatalf("rc = %d, want 2", rc)
	}
	if !strings.Contains(stderr.String(), "M4") {
		t.Fatalf("stderr should explain M4 removal: %q", stderr.String())
	}
}

func TestServe_RejectsLegacyAuthScopeFlag(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rc := runServe(context.Background(), []string{"--auth-scope", "all"}, stdout, stderr)
	if rc != 2 {
		t.Fatalf("rc = %d, want 2", rc)
	}
	if !strings.Contains(stderr.String(), "M4") {
		t.Fatalf("stderr should explain M4 removal: %q", stderr.String())
	}
}

func TestServe_RequiresAtLeastOneListener(t *testing.T) {
	_ = userCmdEnv(t)
	storeDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	// Pass --store so we reach the at-least-one-listener check; pass neither
	// --addr nor --ssh-addr.
	code := runServe(context.Background(),
		[]string{"--store", "localfs:" + storeDir, "--mirror-dir", t.TempDir()},
		&stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit code 2 when neither --addr nor --ssh-addr given, got %d (stderr=%q)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "at least one") {
		t.Fatalf("stderr should mention 'at least one': %q", stderr.String())
	}
}

// keep io import alive (used in earlier test files in this pkg)
var _ = io.Discard
