package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
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

// --- M11 Phase 8 Task 8.3: bundle/pack URI mode flag validation ---
//
// These tests exercise the flag.Parse and post-Parse validation paths in
// runServeWithListener. They never bind a listener, so no temp store or
// mirror dir is needed — validation failures happen before any setup.

func TestRunServe_BundleURIMode_RequiresSigningKey(t *testing.T) {
	_ = userCmdEnv(t)
	var stdout, stderr bytes.Buffer
	rc := runServe(context.Background(), []string{
		"--addr", "127.0.0.1:0",
		"--store", "localfs:" + t.TempDir(),
		"--bundle-uri-mode", "auto",
	}, &stdout, &stderr)
	if rc == 0 {
		t.Fatalf("rc = 0; want non-zero (stderr=%q)", stderr.String())
	}
	if !strings.Contains(stderr.String(), "signing-key") {
		t.Fatalf("stderr should mention 'signing-key': %q", stderr.String())
	}
}

func TestRunServe_PackURIMode_RequiresSigningKey(t *testing.T) {
	_ = userCmdEnv(t)
	var stdout, stderr bytes.Buffer
	rc := runServe(context.Background(), []string{
		"--addr", "127.0.0.1:0",
		"--store", "localfs:" + t.TempDir(),
		"--pack-uri-mode", "proxied",
	}, &stdout, &stderr)
	if rc == 0 {
		t.Fatalf("rc = 0; want non-zero (stderr=%q)", stderr.String())
	}
	if !strings.Contains(stderr.String(), "signing-key") {
		t.Fatalf("stderr should mention 'signing-key': %q", stderr.String())
	}
}

func TestRunServe_BundleURIMode_Off_NoSigningKeyNeeded(t *testing.T) {
	_ = userCmdEnv(t)
	// Pass off/off; runServe binds an ephemeral port (--addr :0) and is
	// torn down via the context timeout below. We assert there is no
	// signing-key validation error in stderr (the modes don't require
	// one) and the call returns rc=0 after graceful shutdown.
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	rc := runServe(ctx, []string{
		"--addr", "127.0.0.1:0",
		"--store", "localfs:" + t.TempDir(),
		"--mirror-dir", t.TempDir(),
		"--bundle-uri-mode", "off",
		"--pack-uri-mode", "off",
		"--shutdown-timeout", "10ms",
	}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rc = %d; want 0 (stderr=%q)", rc, stderr.String())
	}
	if strings.Contains(stderr.String(), "signing-key") {
		t.Fatalf("stderr should not mention signing-key when modes are off: %q", stderr.String())
	}
}

func TestRunServe_BundleURIMode_InvalidValue(t *testing.T) {
	_ = userCmdEnv(t)
	var stdout, stderr bytes.Buffer
	rc := runServe(context.Background(), []string{
		"--addr", "127.0.0.1:0",
		"--store", "localfs:" + t.TempDir(),
		"--bundle-uri-mode", "garbage",
	}, &stdout, &stderr)
	if rc != 2 {
		t.Fatalf("rc = %d; want 2 (stderr=%q)", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "auto|direct|proxied|off") {
		t.Fatalf("stderr should list valid modes: %q", stderr.String())
	}
}

func TestRunServe_PackURIMode_InvalidValue(t *testing.T) {
	_ = userCmdEnv(t)
	var stdout, stderr bytes.Buffer
	rc := runServe(context.Background(), []string{
		"--addr", "127.0.0.1:0",
		"--store", "localfs:" + t.TempDir(),
		"--pack-uri-mode", "garbage",
	}, &stdout, &stderr)
	if rc != 2 {
		t.Fatalf("rc = %d; want 2 (stderr=%q)", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "auto|direct|proxied|off") {
		t.Fatalf("stderr should enumerate valid modes: %s", stderr.String())
	}
}

func TestRunServe_SigningKeyFile_TooShort(t *testing.T) {
	_ = userCmdEnv(t)
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key")
	if err := os.WriteFile(keyPath, []byte("tooshort"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	rc := runServe(context.Background(), []string{
		"--addr", "127.0.0.1:0",
		"--store", "localfs:" + t.TempDir(),
		"--bundle-uri-mode", "proxied",
		"--proxied-url-signing-key", keyPath,
		"--proxied-url-base", "https://gw.example",
	}, &stdout, &stderr)
	if rc != 2 {
		t.Fatalf("rc = %d; want 2 (stderr=%q)", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "8 bytes") && !strings.Contains(stderr.String(), "too short") {
		t.Fatalf("stderr should mention byte count / too short: %q", stderr.String())
	}
}

func TestRunServe_SigningKeyFile_NotReadable(t *testing.T) {
	_ = userCmdEnv(t)
	missing := filepath.Join(t.TempDir(), "no-such-file")
	var stdout, stderr bytes.Buffer
	rc := runServe(context.Background(), []string{
		"--addr", "127.0.0.1:0",
		"--store", "localfs:" + t.TempDir(),
		"--bundle-uri-mode", "auto",
		"--proxied-url-signing-key", missing,
		"--proxied-url-base", "https://gw.example",
	}, &stdout, &stderr)
	if rc != 1 {
		t.Fatalf("rc = %d; want 1 (stderr=%q)", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "signing-key") {
		t.Fatalf("stderr should mention 'signing-key' read error: %q", stderr.String())
	}
}

func TestRunServe_ProxiedBaseURLRequired(t *testing.T) {
	_ = userCmdEnv(t)
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key")
	if err := os.WriteFile(keyPath, []byte("0123456789abcdef0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	rc := runServe(context.Background(), []string{
		"--addr", "127.0.0.1:0",
		"--store", "localfs:" + t.TempDir(),
		"--bundle-uri-mode", "auto",
		"--proxied-url-signing-key", keyPath,
	}, &stdout, &stderr)
	if rc != 2 {
		t.Fatalf("rc = %d; want 2 (stderr=%q)", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "proxied-url-base") {
		t.Fatalf("stderr should mention 'proxied-url-base': %q", stderr.String())
	}
}
