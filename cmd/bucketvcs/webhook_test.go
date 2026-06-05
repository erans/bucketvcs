package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

func setupAuthDBForWebhook(t *testing.T, tenant, repo string) string {
	t.Helper()
	tmp := t.TempDir()
	path := filepath.Join(tmp, "auth.db")
	store, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("open authdb: %v", err)
	}
	defer store.Close()
	if _, err := store.DB().ExecContext(context.Background(),
		`INSERT INTO repos (tenant, name, public_read, created_at)
		 VALUES (?, ?, 0, strftime('%s','now'))`,
		tenant, repo,
	); err != nil {
		t.Fatalf("seed repo row: %v", err)
	}
	return path
}

func TestWebhook_EndpointAddListRemove(t *testing.T) {
	authDB := setupAuthDBForWebhook(t, "acme", "site")
	ctx := context.Background()
	var stdout, stderr bytes.Buffer

	code := runWebhook(ctx, []string{
		"endpoint", "add",
		"--auth-db=" + authDB,
		"--tenant=acme",
		"--repo=site",
		"--url=https://hooks.example.com/x",
		"--events=push,lfs.upload",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("add exit=%d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "endpoint_id=") {
		t.Errorf("add output missing endpoint_id: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "secret=") {
		t.Errorf("add output missing secret: %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = runWebhook(ctx, []string{
		"endpoint", "list",
		"--auth-db=" + authDB,
		"--tenant=acme",
		"--repo=site",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("list exit=%d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "url=https://hooks.example.com/x") {
		t.Errorf("list output missing url: %s", stdout.String())
	}
	if strings.Contains(stdout.String(), "secret=") {
		t.Errorf("list output exposed secret: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "secret_preview=") {
		t.Errorf("list output missing secret_preview: %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = runWebhook(ctx, []string{
		"endpoint", "list",
		"--auth-db=" + authDB,
		"--tenant=acme",
		"--repo=site",
		"--format=json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("list json exit=%d, stderr=%s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Errorf("expected 1 NDJSON line, got %d: %s", len(lines), stdout.String())
	}
}

func TestWebhook_RemoveByID(t *testing.T) {
	authDB := setupAuthDBForWebhook(t, "acme", "site")
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	_ = runWebhook(ctx, []string{
		"endpoint", "add",
		"--auth-db=" + authDB,
		"--tenant=acme", "--repo=site",
		"--url=https://x", "--events=all",
	}, &stdout, &stderr)
	if code := runWebhook(ctx, []string{
		"endpoint", "remove",
		"--auth-db=" + authDB, "--id=1",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("remove exit=%d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	_ = runWebhook(ctx, []string{
		"endpoint", "list",
		"--auth-db=" + authDB, "--tenant=acme", "--repo=site",
	}, &stdout, &stderr)
	if !strings.Contains(stdout.String(), "no endpoints") {
		t.Errorf("after remove: expected 'no endpoints', got %s", stdout.String())
	}
}

func TestWebhook_UsageErrors(t *testing.T) {
	authDB := setupAuthDBForWebhook(t, "acme", "site")
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	cases := [][]string{
		{"endpoint", "add"},
		{"endpoint", "add",
			"--auth-db=" + authDB,
			"--tenant=acme", "--repo=site",
			"--url=ftp://bad",
			"--events=push"},
		{"endpoint", "add",
			"--auth-db=" + authDB,
			"--tenant=acme", "--repo=site",
			"--url=https://x",
			"--events=bogus"},
	}
	for _, args := range cases {
		stdout.Reset()
		stderr.Reset()
		code := runWebhook(ctx, args, &stdout, &stderr)
		if code != 2 {
			t.Errorf("args=%v: exit=%d, want 2", args, code)
		}
	}
}

func TestWebhook_EndpointRotateSecret(t *testing.T) {
	authDB := setupAuthDBForWebhook(t, "acme", "site")
	ctx := context.Background()
	var stdout, stderr bytes.Buffer

	code := runWebhook(ctx, []string{
		"endpoint", "add",
		"--auth-db=" + authDB,
		"--tenant=acme", "--repo=site",
		"--url=https://hook.example.com",
		"--events=push",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("add: rc=%d, stderr=%s", code, stderr.String())
	}
	originalOutput := stdout.String()
	if !strings.Contains(originalOutput, "secret=") {
		t.Fatalf("add output missing secret: %s", originalOutput)
	}

	stdout.Reset()
	stderr.Reset()
	code = runWebhook(ctx, []string{
		"endpoint", "rotate-secret",
		"--auth-db=" + authDB,
		"--id=1",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("rotate-secret: rc=%d, stderr=%s", code, stderr.String())
	}
	rotateOutput := stdout.String()
	if !strings.Contains(rotateOutput, "endpoint_id=1") {
		t.Errorf("rotate output missing endpoint_id=1: %s", rotateOutput)
	}
	if !strings.Contains(rotateOutput, "rotated") {
		t.Errorf("rotate output missing 'rotated': %s", rotateOutput)
	}
	if !strings.Contains(rotateOutput, "secret=") {
		t.Errorf("rotate output missing secret=: %s", rotateOutput)
	}

	originalSecret := extractSecret(t, originalOutput)
	newSecret := extractSecret(t, rotateOutput)
	if originalSecret == newSecret {
		t.Errorf("rotate-secret returned same secret as add: %q", newSecret)
	}
}

func TestWebhook_RotateSecretNotFound(t *testing.T) {
	authDB := setupAuthDBForWebhook(t, "acme", "site")
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	code := runWebhook(ctx, []string{
		"endpoint", "rotate-secret",
		"--auth-db=" + authDB,
		"--id=99999",
	}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("rotate-secret non-existent: rc=%d, want 2", code)
	}
}

func extractSecret(t *testing.T, out string) string {
	t.Helper()
	idx := strings.Index(out, "secret=")
	if idx < 0 {
		t.Fatalf("no secret in output: %s", out)
	}
	tail := out[idx+len("secret="):]
	end := strings.IndexAny(tail, " \t\n\r")
	if end < 0 {
		end = len(tail)
	}
	return tail[:end]
}

// TestWebhook_AddWarnsOnDeniedIP asserts that registering an endpoint whose
// URL names a literal IP in the default egress deny set still succeeds (the
// CLI cannot know serve's --webhook-allow-cidr config), but prints a warning
// on stderr.
func TestWebhook_AddWarnsOnDeniedIP(t *testing.T) {
	authDB := setupAuthDBForWebhook(t, "acme", "site")
	ctx := context.Background()
	var stdout, stderr bytes.Buffer

	code := runWebhook(ctx, []string{
		"endpoint", "add",
		"--auth-db=" + authDB,
		"--tenant=acme",
		"--repo=site",
		"--url=http://127.0.0.1:9/hook",
		"--events=push",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("add exit=%d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "default egress deny set") {
		t.Errorf("expected egress warning on stderr, got: %q", stderr.String())
	}
}

// TestWebhook_AddNoWarnOnPublicURL asserts that a public-host URL registers
// without an egress warning (a hostname can resolve anywhere, so the CLI only
// warns on literal denied IPs).
func TestWebhook_AddNoWarnOnPublicURL(t *testing.T) {
	authDB := setupAuthDBForWebhook(t, "acme", "site")
	ctx := context.Background()
	var stdout, stderr bytes.Buffer

	code := runWebhook(ctx, []string{
		"endpoint", "add",
		"--auth-db=" + authDB,
		"--tenant=acme",
		"--repo=site",
		"--url=https://hooks.example.com/hook",
		"--events=push",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("add exit=%d, stderr=%s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "default egress deny set") {
		t.Errorf("unexpected egress warning for public URL: %q", stderr.String())
	}
}
