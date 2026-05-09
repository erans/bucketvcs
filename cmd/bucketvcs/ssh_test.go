package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/sshd"
)

// newTestLogger returns a logger that discards all output, suitable for use in tests.
func newTestLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRunSSHFingerprint_PrintsExpectedFormat(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "host_key")

	// Generate a host key via the same path the gateway uses.
	logger := newTestLogger(t)
	signer, err := sshd.LoadOrGenerateHostKey(keyPath, logger)
	if err != nil {
		t.Fatal(err)
	}
	expectedFP := sshd.SHA256Fingerprint(signer.PublicKey())

	// Override env so resolveHostKey uses our temp path via BUCKETVCS_SSH_HOST_KEY.
	t.Setenv("BUCKETVCS_SSH_HOST_KEY", keyPath)
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "")

	var stdout, stderr bytes.Buffer
	rc := runSSHFingerprint(context.Background(), nil, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rc = %d, stderr=%q", rc, stderr.String())
	}
	out := stdout.String()
	if !strings.HasPrefix(out, expectedFP+" ") {
		t.Fatalf("output does not start with expected fp %q: %q", expectedFP, out)
	}
	if !strings.Contains(out, "bucketvcs host key") {
		t.Fatalf("missing label in output: %q", out)
	}
}

func TestRunSSHFingerprint_MissingFile(t *testing.T) {
	t.Setenv("BUCKETVCS_SSH_HOST_KEY", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "")
	var stderr bytes.Buffer
	rc := runSSHFingerprint(context.Background(), nil, &bytes.Buffer{}, &stderr)
	if rc == 0 {
		t.Fatal("expected nonzero exit for missing file")
	}
	if !strings.Contains(stderr.String(), "read") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunSSH_UnknownSubcommand(t *testing.T) {
	var stderr bytes.Buffer
	rc := runSSH(context.Background(), []string{"bogus"}, &bytes.Buffer{}, &stderr)
	if rc != 2 {
		t.Fatalf("rc = %d, want 2", rc)
	}
	if !strings.Contains(stderr.String(), "unknown subcommand") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunSSH_NoSubcommand(t *testing.T) {
	var stderr bytes.Buffer
	rc := runSSH(context.Background(), nil, &bytes.Buffer{}, &stderr)
	if rc != 2 {
		t.Fatalf("rc = %d, want 2", rc)
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
