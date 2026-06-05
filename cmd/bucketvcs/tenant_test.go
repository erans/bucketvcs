package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/byob"
)

func seedKeyFile(t *testing.T) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "key.bin")
	os.WriteFile(f, make([]byte, 32), 0o600)
	return f
}

func seedCredsFile(t *testing.T, content string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "creds.json")
	os.WriteFile(f, []byte(content), 0o600)
	return f
}

func openFreshDB(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "auth.db")
	s, err := sqlitestore.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	return p
}

func TestTenantStorageBind_LocalfsRoundTrip(t *testing.T) {
	storeDir := t.TempDir()
	dbPath := openFreshDB(t)
	keyFile := seedKeyFile(t)
	credsFile := seedCredsFile(t, `{}`)

	var out, errb bytes.Buffer
	code := run(context.Background(), []string{
		"tenant", "storage", "bind",
		"--auth-db", dbPath,
		"--tenant", "acme",
		"--store", "localfs:" + storeDir,
		"--creds-file", credsFile,
		"--byob-encryption-key", keyFile,
	}, &out, &errb)
	if code != 0 {
		t.Fatalf("bind exit=%d\nstderr: %s\nstdout: %s", code, errb.String(), out.String())
	}
	if !strings.Contains(out.String(), "bound") {
		t.Errorf("expected 'bound' in output: %s", out.String())
	}
	// Verify binding in DB.
	s2, _ := sqlitestore.Open(dbPath)
	defer s2.Close()
	b, err := s2.GetStorageBinding(context.Background(), "acme")
	if err != nil {
		t.Fatalf("GetStorageBinding: %v", err)
	}
	if b.Provider != "localfs" {
		t.Fatalf("provider=%s", b.Provider)
	}
	// Verify creds are encrypted and round-trip correctly.
	plain, err := byob.Decrypt(make([]byte, 32), b.CredsJSON)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !json.Valid(plain) {
		t.Fatalf("creds not valid JSON")
	}
}

func TestTenantStorageList(t *testing.T) {
	storeDir := t.TempDir()
	dbPath := openFreshDB(t)
	keyFile := seedKeyFile(t)
	credsFile := seedCredsFile(t, `{}`)
	// Bind first.
	run(context.Background(), []string{"tenant", "storage", "bind",
		"--auth-db", dbPath, "--tenant", "acme",
		"--store", "localfs:" + storeDir,
		"--creds-file", credsFile, "--byob-encryption-key", keyFile,
	}, &bytes.Buffer{}, &bytes.Buffer{})

	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"tenant", "storage", "list", "--auth-db", dbPath}, &out, &errb)
	if code != 0 {
		t.Fatalf("list exit=%d: %s", code, errb.String())
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &row); err != nil {
		t.Fatalf("parse NDJSON: %v\n%s", err, out.String())
	}
	if row["tenant"] != "acme" {
		t.Fatalf("unexpected row: %v", row)
	}
	if _, ok := row["creds_json"]; ok {
		t.Fatal("creds_json must not appear in list output")
	}
}

func TestTenantStorageUnbind(t *testing.T) {
	storeDir := t.TempDir()
	dbPath := openFreshDB(t)
	keyFile := seedKeyFile(t)
	credsFile := seedCredsFile(t, `{}`)
	run(context.Background(), []string{"tenant", "storage", "bind",
		"--auth-db", dbPath, "--tenant", "acme",
		"--store", "localfs:" + storeDir,
		"--creds-file", credsFile, "--byob-encryption-key", keyFile,
	}, &bytes.Buffer{}, &bytes.Buffer{})

	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"tenant", "storage", "unbind",
		"--auth-db", dbPath, "--tenant", "acme",
	}, &out, &errb)
	if code != 0 {
		t.Fatalf("unbind exit=%d: %s", code, errb.String())
	}
	s2, _ := sqlitestore.Open(dbPath)
	defer s2.Close()
	if _, err := s2.GetStorageBinding(context.Background(), "acme"); err == nil {
		t.Fatal("binding should be gone after unbind")
	}
}

func TestTenantStorageVerify(t *testing.T) {
	storeDir := t.TempDir()
	dbPath := openFreshDB(t)
	keyFile := seedKeyFile(t)
	credsFile := seedCredsFile(t, `{}`)
	run(context.Background(), []string{"tenant", "storage", "bind",
		"--auth-db", dbPath, "--tenant", "acme",
		"--store", "localfs:" + storeDir,
		"--creds-file", credsFile, "--byob-encryption-key", keyFile,
	}, &bytes.Buffer{}, &bytes.Buffer{})

	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"tenant", "storage", "verify",
		"--auth-db", dbPath, "--tenant", "acme", "--byob-encryption-key", keyFile,
	}, &out, &errb)
	if code != 0 {
		t.Fatalf("verify exit=%d\n%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "verified ok") {
		t.Fatalf("expected 'verified ok': %s", out.String())
	}
}
