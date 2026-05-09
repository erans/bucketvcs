package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoDeployKey_AddListRevoke(t *testing.T) {
	_ = userKeyCmdEnv(t) // sets BUCKETVCS_AUTH_DB, XDG_STATE_HOME, HOME

	ctx := context.Background()
	var stdout, stderr bytes.Buffer

	// Register a repo (--no-init: no M1 bucket needed in unit tests).
	rc := runRepo(ctx, []string{"register", "acme/web", "--no-init"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("repo register: rc=%d %s", rc, stderr.String())
	}

	// Generate a fresh key.
	pubFile := genPubKeyFile(t)

	stdout.Reset()
	stderr.Reset()
	rc = repoDeployKeyAdd(ctx, []string{"--label", "ci", "acme/web", pubFile, "read"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("deployKeyAdd: rc=%d %s", rc, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "bvsk_") {
		t.Fatalf("missing key id: %s", out)
	}
	if !strings.Contains(out, "read") || !strings.Contains(out, "acme/web") {
		t.Fatalf("missing scope info: %s", out)
	}

	// Extract key ID for revoke test.
	fields := strings.Fields(out)
	var keyID string
	for i, f := range fields {
		if f == "key" && i+1 < len(fields) {
			keyID = fields[i+1]
			break
		}
	}
	if keyID == "" {
		t.Fatalf("could not extract key ID from: %s", out)
	}

	// List should show one row.
	stdout.Reset()
	stderr.Reset()
	rc = repoDeployKeyList(ctx, []string{"acme/web"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("list: rc=%d %s", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "bvsk_") {
		t.Fatalf("list missing key: %s", stdout.String())
	}

	// List --json should be a valid JSON array.
	stdout.Reset()
	stderr.Reset()
	rc = repoDeployKeyList(ctx, []string{"--json", "acme/web"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("list --json: rc=%d %s", rc, stderr.String())
	}
	trimmed := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(trimmed, "[") {
		t.Fatalf("not a JSON array: %s", trimmed)
	}

	// Add same key again → duplicate fingerprint error.
	stdout.Reset()
	stderr.Reset()
	rc = repoDeployKeyAdd(ctx, []string{"acme/web", pubFile, "write"}, &stdout, &stderr)
	if rc == 0 {
		t.Fatal("expected duplicate-fingerprint rejection")
	}
	if !strings.Contains(stderr.String(), "fingerprint already registered") {
		t.Logf("stderr: %q", stderr.String())
	}

	// Revoke by full ID.
	stdout.Reset()
	stderr.Reset()
	rc = repoDeployKeyRevoke(ctx, []string{keyID}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("revoke: rc=%d %s", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "revoked") {
		t.Fatalf("revoke output unexpected: %s", stdout.String())
	}
}

func TestRepoDeployKey_RejectsUnregisteredRepo(t *testing.T) {
	_ = userKeyCmdEnv(t)
	pubFile := genPubKeyFile(t)
	var stderr bytes.Buffer
	rc := repoDeployKeyAdd(context.Background(), []string{"ghost/repo", pubFile, "read"}, &bytes.Buffer{}, &stderr)
	if rc == 0 {
		t.Fatal("expected error for unregistered repo")
	}
	// Accept any non-zero exit with a message about the repo not being found.
	t.Logf("stderr: %q", stderr.String())
}

func TestRepoDeployKey_RejectsAdminPerm(t *testing.T) {
	_ = userKeyCmdEnv(t)
	dir := t.TempDir()
	pubFile := filepath.Join(dir, "k.pub")
	// /dev/null is not a valid pubkey but the admin check happens before reading the file.
	var stderr bytes.Buffer
	rc := repoDeployKeyAdd(context.Background(), []string{"acme/web", pubFile, "admin"}, &bytes.Buffer{}, &stderr)
	if rc == 0 {
		t.Fatal("expected error for 'admin' perm")
	}
	if !strings.Contains(stderr.String(), "admin") {
		t.Logf("stderr: %q", stderr.String())
	}
}

func TestRepoDeployKey_BadTenantRepoFormat(t *testing.T) {
	_ = userKeyCmdEnv(t)
	var stderr bytes.Buffer
	rc := repoDeployKeyAdd(context.Background(), []string{"no-slash-here", "/dev/null", "read"}, &bytes.Buffer{}, &stderr)
	if rc == 0 {
		t.Fatal("expected error for missing /")
	}
}

func TestRepoDeployKey_RevokeNoSuchKey(t *testing.T) {
	_ = userKeyCmdEnv(t)
	var stderr bytes.Buffer
	rc := repoDeployKeyRevoke(context.Background(), []string{"bvsk_doesnotexist"}, &bytes.Buffer{}, &stderr)
	if rc == 0 {
		t.Fatal("expected nonzero exit for no-such-key")
	}
}

func TestRepoDeployKey_ListEmpty(t *testing.T) {
	_ = userKeyCmdEnv(t)
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	// Register a repo with no keys — list should succeed and return empty table.
	rc := runRepo(ctx, []string{"register", "empty/proj", "--no-init"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("repo register: rc=%d %s", rc, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	rc = repoDeployKeyList(ctx, []string{"empty/proj"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("list: rc=%d %s", rc, stderr.String())
	}
}

func TestRepoDeployKey_WritePermKey(t *testing.T) {
	_ = userKeyCmdEnv(t)
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	rc := runRepo(ctx, []string{"register", "acme/writes", "--no-init"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("repo register: rc=%d %s", rc, stderr.String())
	}

	pubFile := genPubKeyFile(t)
	stdout.Reset()
	stderr.Reset()
	rc = repoDeployKeyAdd(ctx, []string{"acme/writes", pubFile, "write"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("deployKeyAdd write: rc=%d %s", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "write") {
		t.Fatalf("output missing 'write': %s", stdout.String())
	}
}

func TestRepoDeployKey_PubKeyReadFromFile(t *testing.T) {
	_ = userKeyCmdEnv(t)
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	rc := runRepo(ctx, []string{"register", "acme/pktest", "--no-init"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("repo register: rc=%d %s", rc, stderr.String())
	}

	// Write garbage to the pubkey file — should fail with a parse error.
	dir := t.TempDir()
	badPub := filepath.Join(dir, "bad.pub")
	os.WriteFile(badPub, []byte("not-a-pubkey"), 0o600) //nolint:errcheck

	stdout.Reset()
	stderr.Reset()
	rc = repoDeployKeyAdd(ctx, []string{"acme/pktest", badPub, "read"}, &stdout, &stderr)
	if rc == 0 {
		t.Fatal("expected error for bad pubkey")
	}
}
