package sshd

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

func TestLoadOrGenerateHostKey_Generates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "host_key")
	logger := newTestLogger()

	signer, err := LoadOrGenerateHostKey(path, logger)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if signer == nil {
		t.Fatal("nil signer")
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", st.Mode().Perm())
	}

	if signer.PublicKey().Type() != "ssh-ed25519" {
		t.Fatalf("type = %q, want ssh-ed25519", signer.PublicKey().Type())
	}
}

func TestLoadOrGenerateHostKey_LoadsExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "host_key")
	logger := newTestLogger()

	first, err := LoadOrGenerateHostKey(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrGenerateHostKey(path, logger)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(first.PublicKey().Marshal(), second.PublicKey().Marshal()) {
		t.Fatal("host key changed across loads")
	}
}

func TestLoadOrGenerateHostKey_LooseMode_LoadsWithWarning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "host_key")
	// Generate first via the normal path.
	if _, err := LoadOrGenerateHostKey(path, newTestLogger()); err != nil {
		t.Fatal(err)
	}
	// Loosen mode.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	// Capture log output via a custom handler.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	signer, err := LoadOrGenerateHostKey(path, logger)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if signer == nil {
		t.Fatal("nil signer")
	}
	if !bytes.Contains(buf.Bytes(), []byte("permissive")) {
		t.Fatalf("expected permissive-mode warning in log; got %q", buf.String())
	}
}

func TestLoadOrGenerateHostKey_BadPath_ReturnsError(t *testing.T) {
	// Use a directory as the path — unreadable as a file.
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := LoadOrGenerateHostKey(path, newTestLogger())
	if err == nil {
		t.Fatal("expected error for directory path")
	}
}
