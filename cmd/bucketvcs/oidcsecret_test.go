package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveOIDCHMACKey_EnvProvisioned is the A6 regression: a stable
// operator-provisioned key (env or file) must be honored so OIDC logins
// verify across instances and restarts.
func TestResolveOIDCHMACKey_EnvProvisioned(t *testing.T) {
	env := func(string) string { return "0123456789abcdef0123456789abcdef" }
	key, provisioned, err := resolveOIDCHMACKey("", env)
	if err != nil {
		t.Fatalf("env key: %v", err)
	}
	if !provisioned {
		t.Fatal("env key should provision")
	}
	if string(key) != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("key mismatch: %q", key)
	}
}

func TestResolveOIDCHMACKey_FileBeatsUnsetEnv(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hmac.key")
	if err := os.WriteFile(p, []byte("  file-key-0123456789abcdef  \n"), 0600); err != nil {
		t.Fatal(err)
	}
	env := func(string) string { return "" }
	key, provisioned, err := resolveOIDCHMACKey(p, env)
	if err != nil {
		t.Fatalf("file key: %v", err)
	}
	if !provisioned || string(key) != "file-key-0123456789abcdef" {
		t.Fatalf("file key not honored/trimmed: provisioned=%v key=%q", provisioned, key)
	}
}

func TestResolveOIDCHMACKey_AbsentFallsBackToEphemeral(t *testing.T) {
	env := func(string) string { return "" }
	key, provisioned, err := resolveOIDCHMACKey("", env)
	if err != nil {
		t.Fatalf("absent key: %v", err)
	}
	if provisioned || key != nil {
		t.Fatal("absent key should request ephemeral fallback")
	}
}

func TestResolveOIDCHMACKey_ShortKeyFailsClosed(t *testing.T) {
	env := func(string) string { return "short" }
	if _, _, err := resolveOIDCHMACKey("", env); err == nil {
		t.Fatal("short key should fail closed, not run multi-node on guessable key")
	}
}

func TestResolveOIDCHMACKey_MissingFileFailsClosed(t *testing.T) {
	env := func(string) string { return "" }
	if _, _, err := resolveOIDCHMACKey(filepath.Join(t.TempDir(), "nope"), env); err == nil {
		t.Fatal("missing key file should fail closed")
	}
}
