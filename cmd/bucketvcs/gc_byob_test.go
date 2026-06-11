package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/byob"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// makeByobKey creates a 32-byte random key file and returns its path.
func makeByobKey(t *testing.T) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	// openByobStore TrimSpaces the key file; a random first/last byte that
	// happens to be whitespace (~5% of runs) would shrink/shift the key and
	// flake the test. Pin both ends outside the ASCII whitespace range.
	key[0] |= 0x80
	key[len(key)-1] |= 0x80
	path := filepath.Join(t.TempDir(), "byob.key")
	if err := os.WriteFile(path, key, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return path
}

// seedByobBinding creates an authdb, registers a localfs binding for tenant,
// and returns (authdb path, key file path).
func seedByobBinding(t *testing.T, tenant, storeURL string) (dbPath, keyPath string) {
	t.Helper()

	dbPath = filepath.Join(t.TempDir(), "auth.db")
	keyPath = makeByobKey(t)

	rawKey, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read key: %v", err)
	}

	// credsJSON is empty for localfs — it ignores creds.
	credsPlain := []byte("{}")
	encCreds, err := byob.Encrypt(rawKey[:32], credsPlain)
	if err != nil {
		t.Fatalf("encrypt creds: %v", err)
	}

	s, err := sqlitestore.Open(dbPath)
	if err != nil {
		t.Fatalf("open authdb: %v", err)
	}
	defer s.Close()

	now := time.Now().Unix()
	if err := s.UpsertStorageBinding(context.Background(), sqlitestore.StorageBinding{
		Tenant:     tenant,
		StoreURL:   storeURL,
		CredsJSON:  encCreds,
		Provider:   "localfs",
		CreatedAt:  now,
		UpdatedAt:  now,
		VerifiedAt: now,
	}); err != nil {
		t.Fatalf("UpsertStorageBinding: %v", err)
	}
	return dbPath, keyPath
}

// TestGCBYOB_StoreOpenedFromBinding verifies that runGC can omit --store when
// --repo + --auth-db + --byob-encryption-key are all provided and a valid BYOB
// binding exists for the tenant. The GC run should succeed (exit 0).
func TestGCBYOB_StoreOpenedFromBinding(t *testing.T) {
	storeDir := t.TempDir()
	storeURL := "localfs:" + storeDir

	// Seed a repo in the store.
	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()
	if _, err := repo.Create(ctx, store, "byobtenant", "myrepo", repo.CreateOptions{Actor: "u_test"}); err != nil {
		t.Fatalf("Create repo: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	// Seed a BYOB binding pointing at the same localfs store.
	dbPath, keyPath := seedByobBinding(t, "byobtenant", storeURL)

	var stdout, stderr bytes.Buffer
	code := runGC(ctx, []string{
		"--repo", "byobtenant/myrepo",
		"--auth-db", dbPath,
		"--byob-encryption-key", keyPath,
		"--retention", "1s",
	}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("BYOB GC exit=%d, want 0\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "byobtenant/myrepo") {
		t.Errorf("expected stdout to mention byobtenant/myrepo; got: %s", stdout.String())
	}
}

// TestGCBYOB_MissingStoreFallsBackToStoreRequired verifies that when --repo is
// given without --auth-db / --byob-encryption-key, the original --store
// required error is returned (exit 2).
func TestGCBYOB_MissingStoreFallsBackToStoreRequired(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runGC(context.Background(), []string{
		"--repo", "sometenants/somerepo",
	}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("exit=%d, want 2 (--store required)\nstderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--store is required") {
		t.Errorf("expected --store error message; got: %s", stderr.String())
	}
}

// TestGCBYOB_NoBindingFallsBackToStore verifies that when the tenant has no
// BYOB binding in the authdb, GC falls back to the --store flag and uses it
// normally (exit 0 when --store points at a valid store with the repo).
func TestGCBYOB_NoBindingFallsBackToStore(t *testing.T) {
	storeDir := t.TempDir()
	storeURL := "localfs:" + storeDir

	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()
	if _, err := repo.Create(ctx, store, "notbound", "myrepo", repo.CreateOptions{Actor: "u_test"}); err != nil {
		t.Fatalf("Create repo: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	// Create authdb with NO binding for "notbound".
	dbPath := filepath.Join(t.TempDir(), "auth.db")
	s, err := sqlitestore.Open(dbPath)
	if err != nil {
		t.Fatalf("open authdb: %v", err)
	}
	s.Close()

	keyPath := makeByobKey(t)

	var stdout, stderr bytes.Buffer
	code := runGC(ctx, []string{
		"--repo", "notbound/myrepo",
		"--store", storeURL,
		"--auth-db", dbPath,
		"--byob-encryption-key", keyPath,
		"--retention", "1s",
	}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("fallback-to-store exit=%d, want 0\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
}
