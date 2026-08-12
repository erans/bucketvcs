package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/byob"
)

// Wave-2 U-8 regression coverage: `tenant storage bind` must encrypt creds
// with the tenant bound as AAD; GC/doctor/verify readers and the resolver
// must decrypt with tenant AAD and keep the nil-AAD legacy fallback.

// readBindingRow opens the authdb at dbPath and returns the binding for
// tenant.
func readBindingRow(t *testing.T, dbPath, tenant string) sqlitestore.StorageBinding {
	t.Helper()
	s, err := sqlitestore.Open(dbPath)
	if err != nil {
		t.Fatalf("open authdb: %v", err)
	}
	defer s.Close()
	b, err := s.GetStorageBinding(context.Background(), tenant)
	if err != nil {
		t.Fatalf("GetStorageBinding: %v", err)
	}
	return *b
}

func TestTenantBind_CredsBoundToTenantAAD(t *testing.T) {
	storeDir := t.TempDir()
	dbPath := openFreshDB(t)
	keyFile := seedKeyFile(t)
	credsFile := seedCredsFile(t, `{"s3_secret":"hunter2"}`)

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

	// seedKeyFile writes a 32-byte zero key.
	key := make([]byte, 32)
	b := readBindingRow(t, dbPath, "acme")

	// Tenant-AAD decrypt must round-trip the creds (green pre/post via the
	// legacy fallback — the canary half).
	got, err := byob.DecryptForTenant(key, b.CredsJSON, "acme")
	if err != nil {
		t.Fatalf("DecryptForTenant(acme): %v", err)
	}
	if !json.Valid(got) || !strings.Contains(string(got), "hunter2") {
		t.Fatalf("round-trip creds mismatch: %q", got)
	}

	// The row must NOT decrypt without the tenant AAD (nil-AAD path). Pre-fix
	// bind used Encrypt (nil AAD), so this succeeds — U-8 dead crypto.
	if _, err := byob.Decrypt(key, b.CredsJSON); err == nil {
		t.Fatalf("creds row decrypts under nil AAD — tenant binding (AAD) was not wired into bind (U-8)")
	}

	// And must NOT decrypt under a different tenant's AAD. Pre-fix the
	// nil-AAD step in DecryptForTenant falls open to "other-tenant".
	if _, err := byob.DecryptForTenant(key, b.CredsJSON, "other-tenant"); err == nil {
		t.Fatalf("creds row decrypts under the wrong tenant AAD — tenant binding not wired into bind (U-8)")
	}
}

func TestTenantStorageVerify_LegacyNilAADRowStillOpens(t *testing.T) {
	// Canary for the adjudicated constraint "do NOT drop the legacy fallback":
	// a row encrypted pre-U-8 (nil AAD) must keep passing `tenant storage
	// verify` and DecryptForTenant.
	dbPath := openFreshDB(t)
	keyFile := seedKeyFile(t)
	key := make([]byte, 32)

	legacy, err := byob.Encrypt(key, []byte(`{"legacy":true}`))
	if err != nil {
		t.Fatalf("Encrypt legacy row: %v", err)
	}
	s, err := sqlitestore.Open(dbPath)
	if err != nil {
		t.Fatalf("open authdb: %v", err)
	}
	now := time.Now().Unix()
	if err := s.UpsertStorageBinding(context.Background(), sqlitestore.StorageBinding{
		Tenant:     "acme",
		StoreURL:   "localfs:" + t.TempDir(),
		CredsJSON:  legacy,
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

	b := readBindingRow(t, dbPath, "acme")
	got, err := byob.DecryptForTenant(key, b.CredsJSON, "acme")
	if err != nil {
		t.Fatalf("legacy nil-AAD row must still open via the DecryptForTenant fallback: %v", err)
	}
	if !strings.Contains(string(got), "legacy") {
		t.Fatalf("legacy round-trip mismatch: %q", got)
	}

	var out, errb bytes.Buffer
	code := run(context.Background(), []string{
		"tenant", "storage", "verify",
		"--auth-db", dbPath,
		"--tenant", "acme",
		"--byob-encryption-key", keyFile,
	}, &out, &errb)
	if code != 0 {
		t.Fatalf("verify on legacy nil-AAD row must still succeed: exit=%d\nstderr: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "verified ok") {
		t.Errorf("expected 'verified ok': %s", out.String())
	}
}
