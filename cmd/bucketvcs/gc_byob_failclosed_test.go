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

// Wave-2 U-4 regression coverage: gc/maintenance must abort (non-zero exit,
// operator store untouched) when a BYOB binding exists but resolution fails
// (bad key / decrypt failure / open failure), rather than silently falling
// back to --store and GC-ing the operator's same-named repo.

// seedUndecryptableBinding creates an authdb with a binding for tenant whose
// creds are encrypted under a DIFFERENT random key than the one in the
// returned key file — so every decrypt attempt fails with a GCM auth error.
func seedUndecryptableBinding(t *testing.T, tenant, storeURL string) (dbPath, keyPath string) {
	t.Helper()
	dbPath = filepath.Join(t.TempDir(), "auth.db")
	otherKey := make([]byte, 32)
	if _, err := rand.Read(otherKey); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	encCreds, err := byob.Encrypt(otherKey, []byte("{}"))
	if err != nil {
		t.Fatalf("encrypt creds under other key: %v", err)
	}
	s, err := sqlitestore.Open(dbPath)
	if err != nil {
		t.Fatalf("open authdb: %v", err)
	}
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
	if err := s.Close(); err != nil {
		t.Fatalf("close authdb: %v", err)
	}
	keyPath = makeByobKey(t) // random 32-byte key ≠ otherKey (2^-256 collision is ignorable)
	return dbPath, keyPath
}

// snapshotStoreDir maps relpath -> size for every regular file under dir so
// tests can assert the operator store was byte-for-byte untouched.
func snapshotStoreDir(t *testing.T, dir string) map[string]int64 {
	t.Helper()
	snap := map[string]int64{}
	if err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		snap[rel] = info.Size()
		return nil
	}); err != nil {
		t.Fatalf("walk store dir: %v", err)
	}
	return snap
}

func TestGCBYOB_DecryptFailure_AbortsAndLeavesOperatorUntouched(t *testing.T) {
	opDir := t.TempDir()
	opStore, err := localfs.Open(opDir)
	if err != nil {
		t.Fatalf("open operator store: %v", err)
	}
	ctx := context.Background()
	if _, err := repo.Create(ctx, opStore, "byobtenant", "myrepo", repo.CreateOptions{Actor: "u_test"}); err != nil {
		t.Fatalf("Create repo: %v", err)
	}
	if err := opStore.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	// Binding exists for byobtenant but creds don't decrypt under the CLI
	// key. Pre-fix runGC silently fell back to --store and GC'd the
	// operator's same-named repo (exit 0 + GC mark/sweep writes to operator
	// store) — U-4.
	dbPath, keyPath := seedUndecryptableBinding(t, "byobtenant", "localfs:"+filepath.Join(t.TempDir(), "tenant-store"))

	before := snapshotStoreDir(t, opDir)

	var stdout, stderr bytes.Buffer
	code := runGC(ctx, []string{
		"--store", "localfs:" + opDir,
		"--repo", "byobtenant/myrepo",
		"--auth-db", dbPath,
		"--byob-encryption-key", keyPath,
		"--retention", "1s",
	}, &stdout, &stderr)

	if code == 0 {
		t.Fatalf("decrypt-failing binding must abort: exit=0 (pre-fix silently GC'd the operator store — U-4)\nstdout=%s\nstderr=%s",
			stdout.String(), stderr.String())
	}
	if code != 1 {
		t.Errorf("exit=%d, want 1 (operational error, not usage)", code)
	}
	if !strings.Contains(stderr.String(), "byobtenant") || !strings.Contains(stderr.String(), "byob") {
		t.Errorf("stderr must name the tenant and the BYOB cause; got: %s", stderr.String())
	}
	after := snapshotStoreDir(t, opDir)
	if len(before) != len(after) {
		t.Fatalf("operator store mutated: %d files before, %d after", len(before), len(after))
	}
	for rel, size := range before {
		if after[rel] != size {
			t.Errorf("operator store file changed: %s size %d -> %d", rel, size, after[rel])
		}
	}
}

func TestGCBYOB_ShortKey_AbortsWhenBindingPresent(t *testing.T) {
	opDir := t.TempDir()
	opStore, err := localfs.Open(opDir)
	if err != nil {
		t.Fatalf("open operator store: %v", err)
	}
	ctx := context.Background()
	if _, err := repo.Create(ctx, opStore, "byobtenant", "myrepo", repo.CreateOptions{Actor: "u_test"}); err != nil {
		t.Fatalf("Create repo: %v", err)
	}
	if err := opStore.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	// Well-formed binding (decryptable) but the CLI key file is too short —
	// an operational failure, not an absent binding.
	dbPath, _ := seedByobBinding(t, "byobtenant", "localfs:"+t.TempDir())
	shortKey := filepath.Join(t.TempDir(), "short.key")
	if err := os.WriteFile(shortKey, []byte("tooshort"), 0o600); err != nil {
		t.Fatalf("write short key: %v", err)
	}

	before := snapshotStoreDir(t, opDir)

	var stdout, stderr bytes.Buffer
	code := runGC(ctx, []string{
		"--store", "localfs:" + opDir,
		"--repo", "byobtenant/myrepo",
		"--auth-db", dbPath,
		"--byob-encryption-key", shortKey,
		"--retention", "1s",
	}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("short key with a present binding must abort: exit=%d, want 1\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	after := snapshotStoreDir(t, opDir)
	if len(before) != len(after) {
		t.Fatalf("operator store mutated on short-key abort: %d files before, %d after", len(before), len(after))
	}
	for rel, size := range before {
		if after[rel] != size {
			t.Errorf("operator store file changed: %s size %d -> %d", rel, size, after[rel])
		}
	}
}

func TestGCBYOB_OmittedKeyWithBinding_Aborts_U4Review(t *testing.T) {
	opDir := t.TempDir()
	opStore, err := localfs.Open(opDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Create(context.Background(), opStore, "byobtenant", "myrepo", repo.CreateOptions{Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	_ = opStore.Close()
	dbPath, _ := seedByobBinding(t, "byobtenant", "localfs:"+t.TempDir())
	before := snapshotStoreDir(t, opDir)

	var stdout, stderr bytes.Buffer
	code := runGC(context.Background(), []string{
		"--store", "localfs:" + opDir,
		"--repo", "byobtenant/myrepo",
		"--auth-db", dbPath,
		"--retention", "1s",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("binding with omitted key exit=%d, want 1; stderr=%s", code, stderr.String())
	}
	after := snapshotStoreDir(t, opDir)
	if len(before) != len(after) {
		t.Fatalf("operator store mutated: before=%d after=%d", len(before), len(after))
	}
	for rel, size := range before {
		if after[rel] != size {
			t.Fatalf("operator store file %s changed", rel)
		}
	}
}

func TestMaintenanceBYOB_OmittedKeyWithBinding_Aborts_U4Review(t *testing.T) {
	dbPath, _ := seedByobBinding(t, "byobtenant", "localfs:"+t.TempDir())
	var stdout, stderr bytes.Buffer
	code := runMaintenance(context.Background(), []string{
		"--store", "localfs:" + t.TempDir(),
		"--repo", "byobtenant/myrepo",
		"--auth-db", dbPath,
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("binding with omitted key exit=%d, want 1; stderr=%s", code, stderr.String())
	}
}

func TestMaintenanceBYOB_DecryptFailure_AbortsAndLeavesOperatorUntouched(t *testing.T) {
	opDir := t.TempDir()
	opStore, err := localfs.Open(opDir)
	if err != nil {
		t.Fatalf("open operator store: %v", err)
	}
	ctx := context.Background()
	if _, err := repo.Create(ctx, opStore, "byobtenant", "myrepo", repo.CreateOptions{Actor: "u_test"}); err != nil {
		t.Fatalf("Create repo: %v", err)
	}
	if err := opStore.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	dbPath, keyPath := seedUndecryptableBinding(t, "byobtenant", "localfs:"+filepath.Join(t.TempDir(), "tenant-store"))

	before := snapshotStoreDir(t, opDir)

	var stdout, stderr bytes.Buffer
	code := runMaintenance(ctx, []string{
		"--store", "localfs:" + opDir,
		"--repo", "byobtenant/myrepo",
		"--auth-db", dbPath,
		"--byob-encryption-key", keyPath,
	}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("decrypt-failing binding must abort maintenance: exit=%d, want 1\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	after := snapshotStoreDir(t, opDir)
	if len(before) != len(after) {
		t.Fatalf("operator store mutated: %d files before, %d after", len(before), len(after))
	}
	for rel, size := range before {
		if after[rel] != size {
			t.Errorf("operator store file changed: %s size %d -> %d", rel, size, after[rel])
		}
	}
}
