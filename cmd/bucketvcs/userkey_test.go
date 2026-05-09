package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// userKeyCmdEnv sets up an isolated temp dir for auth DB and returns the db path.
func userKeyCmdEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "auth.db")
	t.Setenv("BUCKETVCS_AUTH_DB", dbPath)
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", dir)
	return dbPath
}

// genPubKeyFile generates an ed25519 keypair, writes the public key in
// authorized_keys format to a temp file, and returns the file path.
func genPubKeyFile(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		t.Fatalf("NewPublicKey: %v", err)
	}
	dir := t.TempDir()
	pubFile := filepath.Join(dir, "id_ed25519.pub")
	if err := os.WriteFile(pubFile, ssh.MarshalAuthorizedKey(sshPub), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return pubFile
}

func TestUserKey_AddAndList(t *testing.T) {
	_ = userKeyCmdEnv(t)
	ctx := context.Background()

	// Bootstrap a user.
	var stdout, stderr bytes.Buffer
	rc := userAdd(ctx, []string{"alice"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("userAdd: rc=%d stderr=%s", rc, stderr.String())
	}

	// Generate a fresh keypair, write the public-key file.
	pubFile := genPubKeyFile(t)

	stdout.Reset()
	stderr.Reset()
	rc = userKeyAdd(ctx, []string{"--label", "laptop", "alice", pubFile}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("userKeyAdd: rc=%d stderr=%s", rc, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "added key bvsk_") {
		t.Fatalf("stdout missing 'added key bvsk_': %s", out)
	}
	if !strings.Contains(out, "SHA256:") {
		t.Fatalf("stdout missing fingerprint: %s", out)
	}
	if !strings.Contains(out, "ssh-ed25519") {
		t.Fatalf("stdout missing key type: %s", out)
	}

	// Duplicate should fail.
	stdout.Reset()
	stderr.Reset()
	rc = userKeyAdd(ctx, []string{"alice", pubFile}, &stdout, &stderr)
	if rc == 0 {
		t.Fatalf("expected duplicate-fingerprint failure")
	}

	// List should show one row with the key id.
	stdout.Reset()
	stderr.Reset()
	rc = userKeyList(ctx, []string{"alice"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("userKeyList: rc=%d stderr=%s", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "bvsk_") {
		t.Fatalf("list missing key: %s", stdout.String())
	}

	// List --json should be a valid JSON array.
	stdout.Reset()
	stderr.Reset()
	rc = userKeyList(ctx, []string{"--json", "alice"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("userKeyList --json: rc=%d stderr=%s", rc, stderr.String())
	}
	trimmed := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(trimmed, "[") {
		t.Fatalf("not a JSON array: %s", trimmed)
	}
}

func TestUserKey_Revoke_FullID(t *testing.T) {
	_ = userKeyCmdEnv(t)
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	if rc := userAdd(ctx, []string{"bob"}, &stdout, &stderr); rc != 0 {
		t.Fatalf("userAdd: %s", stderr.String())
	}

	pubFile := genPubKeyFile(t)
	stdout.Reset()
	stderr.Reset()
	rc := userKeyAdd(ctx, []string{"bob", pubFile}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("userKeyAdd: rc=%d stderr=%s", rc, stderr.String())
	}

	// Extract the key ID from "added key bvsk_<id> (..."
	addOut := stdout.String()
	fields := strings.Fields(addOut)
	// "added key bvsk_... ..."
	var keyID string
	for i, f := range fields {
		if f == "key" && i+1 < len(fields) {
			keyID = fields[i+1]
			break
		}
	}
	if keyID == "" {
		t.Fatalf("could not extract key ID from: %s", addOut)
	}

	// Revoke using full ID.
	stdout.Reset()
	stderr.Reset()
	rc = userKeyRevoke(ctx, []string{keyID}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("userKeyRevoke: rc=%d stderr=%s", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "revoked") {
		t.Fatalf("revoke output unexpected: %s", stdout.String())
	}
}

func TestUserKey_Revoke_Prefix(t *testing.T) {
	_ = userKeyCmdEnv(t)
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	if rc := userAdd(ctx, []string{"carol"}, &stdout, &stderr); rc != 0 {
		t.Fatalf("userAdd: %s", stderr.String())
	}

	pubFile := genPubKeyFile(t)
	stdout.Reset()
	stderr.Reset()
	if rc := userKeyAdd(ctx, []string{"carol", pubFile}, &stdout, &stderr); rc != 0 {
		t.Fatalf("userKeyAdd: %s", stderr.String())
	}

	addOut := stdout.String()
	fields := strings.Fields(addOut)
	var keyID string
	for i, f := range fields {
		if f == "key" && i+1 < len(fields) {
			keyID = fields[i+1]
			break
		}
	}
	if len(keyID) < 10 {
		t.Fatalf("keyID too short: %q", keyID)
	}
	// Use a unique prefix (first 12 chars).
	prefix := keyID[:12]

	stdout.Reset()
	stderr.Reset()
	rc := userKeyRevoke(ctx, []string{prefix}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("userKeyRevoke prefix: rc=%d stderr=%s", rc, stderr.String())
	}
}

func TestUserKey_NoSuchUser(t *testing.T) {
	_ = userKeyCmdEnv(t)
	ctx := context.Background()

	pubFile := genPubKeyFile(t)
	var stderr bytes.Buffer
	rc := userKeyAdd(ctx, []string{"ghost", pubFile}, &bytes.Buffer{}, &stderr)
	if rc == 0 {
		t.Fatal("expected nonzero exit for no-such-user")
	}
	if !strings.Contains(stderr.String(), "no such user") {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
}

func TestUserKey_RevokeNoSuchKey(t *testing.T) {
	_ = userKeyCmdEnv(t)
	ctx := context.Background()

	var stderr bytes.Buffer
	rc := userKeyRevoke(ctx, []string{"bvsk_doesnotexist"}, &bytes.Buffer{}, &stderr)
	if rc == 0 {
		t.Fatal("expected nonzero exit for no-such-key")
	}
}

func TestUserKey_ListNoSuchUser(t *testing.T) {
	_ = userKeyCmdEnv(t)
	ctx := context.Background()

	var stderr bytes.Buffer
	rc := userKeyList(ctx, []string{"nobody"}, &bytes.Buffer{}, &stderr)
	if rc == 0 {
		t.Fatal("expected nonzero exit for no-such-user")
	}
}
