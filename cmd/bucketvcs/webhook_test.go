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
	if _, err := store.DB().Exec(
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
