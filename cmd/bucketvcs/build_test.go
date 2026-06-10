package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

func setupAuthDBForBuild(t *testing.T, tenant, repo string) string {
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

func TestBuild_TriggerAddListRemove(t *testing.T) {
	authDB := setupAuthDBForBuild(t, "acme", "site")
	ctx := context.Background()
	var stdout, stderr bytes.Buffer

	code := runBuild(ctx, []string{
		"trigger", "add",
		"--auth-db=" + authDB,
		"--tenant=acme", "--repo=site",
		"--name=cloudbuild-main", "--kind=cloudbuild",
		"--url=https://cloudbuild.example/x",
		"--ref-include=refs/heads/main",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("add exit=%d, stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "trigger_id=") {
		t.Errorf("add output missing trigger_id: %s", out)
	}
	if !strings.Contains(out, "secret=") {
		t.Errorf("add output (cloudbuild) missing secret: %s", out)
	}

	stdout.Reset()
	stderr.Reset()
	code = runBuild(ctx, []string{
		"trigger", "list",
		"--auth-db=" + authDB,
		"--tenant=acme", "--repo=site",
		"--format=json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("list exit=%d, stderr=%s", code, stderr.String())
	}
	jout := stdout.String()
	if !strings.Contains(jout, "cloudbuild-main") {
		t.Errorf("list json missing name: %s", jout)
	}
	if strings.Contains(jout, "\"secret\"") {
		t.Errorf("list json exposed secret: %s", jout)
	}
	lines := strings.Split(strings.TrimRight(jout, "\n"), "\n")
	if len(lines) != 1 {
		t.Errorf("expected 1 NDJSON line, got %d: %s", len(lines), jout)
	}

	// Find the id from text list to remove it.
	stdout.Reset()
	stderr.Reset()
	_ = runBuild(ctx, []string{
		"trigger", "list",
		"--auth-db=" + authDB, "--tenant=acme", "--repo=site",
	}, &stdout, &stderr)
	id := extractKV(t, stdout.String(), "id=")

	stdout.Reset()
	stderr.Reset()
	if code := runBuild(ctx, []string{
		"trigger", "remove",
		"--auth-db=" + authDB, "--id=" + id,
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("remove exit=%d, stderr=%s", code, stderr.String())
	}
}

func TestBuild_TriggerAddBadKind(t *testing.T) {
	authDB := setupAuthDBForBuild(t, "acme", "site")
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	code := runBuild(ctx, []string{
		"trigger", "add",
		"--auth-db=" + authDB,
		"--tenant=acme", "--repo=site",
		"--name=x", "--kind=bogus",
		"--url=https://x",
	}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("bad kind: exit=%d, want 2 (stderr=%s)", code, stderr.String())
	}
}

func TestBuild_TriggerAddCodeBuildMissingProject(t *testing.T) {
	authDB := setupAuthDBForBuild(t, "acme", "site")
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	code := runBuild(ctx, []string{
		"trigger", "add",
		"--auth-db=" + authDB,
		"--tenant=acme", "--repo=site",
		"--name=cb", "--kind=codebuild",
		"--aws-region=us-east-1",
	}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("codebuild missing project: exit=%d, want 2 (stderr=%s)", code, stderr.String())
	}
}

func TestBuild_Apply(t *testing.T) {
	authDB := setupAuthDBForBuild(t, "acme", "site")
	ctx := context.Background()
	doc := `triggers:
  - tenant: acme
    repo: site
    name: applied-cb
    kind: cloudbuild
    url: https://cloudbuild.example/applied
    ref_include:
      - refs/heads/main
`
	f := filepath.Join(t.TempDir(), "triggers.yaml")
	if err := os.WriteFile(f, []byte(doc), 0o600); err != nil {
		t.Fatalf("write doc: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := runBuild(ctx, []string{
		"apply", "--auth-db=" + authDB, "-f", f,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("apply exit=%d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "created=1") {
		t.Errorf("apply output missing created=1: %s", stdout.String())
	}
}

func TestBuild_TestFireAndDeliveryList(t *testing.T) {
	authDB := setupAuthDBForBuild(t, "acme", "site")
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	if code := runBuild(ctx, []string{
		"trigger", "add",
		"--auth-db=" + authDB,
		"--tenant=acme", "--repo=site",
		"--name=fire-cb", "--kind=cloudbuild",
		"--url=https://cloudbuild.example/x",
		"--ref-include=refs/heads/main",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("add exit=%d, stderr=%s", code, stderr.String())
	}
	id := extractKV(t, stdout.String(), "trigger_id=")

	stdout.Reset()
	stderr.Reset()
	if code := runBuild(ctx, []string{
		"test", "--auth-db=" + authDB, "--id=" + id,
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("test exit=%d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runBuild(ctx, []string{
		"delivery", "list", "--auth-db=" + authDB, "--trigger=" + id,
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("delivery list exit=%d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "status=pending") {
		t.Errorf("delivery list missing pending row: %s", stdout.String())
	}
}

func TestTriggerEdit(t *testing.T) {
	authDB := setupAuthDBForBuild(t, "acme", "site")
	ctx := context.Background()
	var stdout, stderr bytes.Buffer

	if code := runBuild(ctx, []string{
		"trigger", "add",
		"--auth-db=" + authDB,
		"--tenant=acme", "--repo=site",
		"--name=ci", "--kind=cloudbuild",
		"--url=https://cloudbuild.example/x",
		"--ref-include=refs/heads/main",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("add exit=%d, stderr=%s", code, stderr.String())
	}
	id := extractKV(t, stdout.String(), "trigger_id=")

	stdout.Reset()
	stderr.Reset()
	if code := runBuild(ctx, []string{
		"trigger", "edit",
		"--auth-db=" + authDB,
		"--id=" + id,
		"--name=ci2",
		"--ref-include=refs/heads/main",
		"--active=false",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("edit exit=%d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runBuild(ctx, []string{
		"trigger", "list",
		"--auth-db=" + authDB, "--tenant=acme", "--repo=site",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("list exit=%d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "ci2") {
		t.Errorf("list output missing renamed trigger ci2: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "active=false") {
		t.Errorf("list output shows trigger still active: %s", stdout.String())
	}
}

func TestTriggerEdit_NotFoundExit1(t *testing.T) {
	authDB := setupAuthDBForBuild(t, "acme", "app")
	var out, errb bytes.Buffer
	code := runBuild(context.Background(), []string{
		"trigger", "edit", "--auth-db=" + authDB, "--id=bvbt_nope", "--name=x",
	}, &out, &errb)
	if code != 1 {
		t.Fatalf("want exit 1 for not-found, got %d (err=%s)", code, errb.String())
	}
}

func TestTriggerRotateSecret(t *testing.T) {
	authDB := setupAuthDBForBuild(t, "acme", "app")
	var addOut, addErr bytes.Buffer
	if code := runBuild(context.Background(), []string{
		"trigger", "add", "--auth-db=" + authDB, "--tenant=acme", "--repo=app",
		"--name=ci", "--kind=generic", "--url=https://example.com/h",
	}, &addOut, &addErr); code != 0 {
		t.Fatalf("add: code=%d err=%s", code, addErr.String())
	}
	id := extractKV(t, addOut.String(), "trigger_id=")
	var out, errb bytes.Buffer
	if code := runBuild(context.Background(), []string{
		"trigger", "rotate-secret", "--auth-db=" + authDB, "--id=" + id,
	}, &out, &errb); code != 0 {
		t.Fatalf("rotate: code=%d err=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "secret=") {
		t.Errorf("rotate output missing secret=: %s", out.String())
	}
}

func TestTriggerRotateSecret_CodeBuildExit2(t *testing.T) {
	authDB := setupAuthDBForBuild(t, "acme", "app")
	var addOut, addErr bytes.Buffer
	if code := runBuild(context.Background(), []string{
		"trigger", "add", "--auth-db=" + authDB, "--tenant=acme", "--repo=app",
		"--name=cb", "--kind=codebuild", "--aws-region=us-east-1", "--aws-project=p",
	}, &addOut, &addErr); code != 0 {
		t.Fatalf("add codebuild: code=%d err=%s", code, addErr.String())
	}
	id := extractKV(t, addOut.String(), "trigger_id=")
	var out, errb bytes.Buffer
	code := runBuild(context.Background(), []string{
		"trigger", "rotate-secret", "--auth-db=" + authDB, "--id=" + id,
	}, &out, &errb)
	if code != 2 { // ErrInvalidInput → exit 2
		t.Fatalf("want exit 2 for codebuild rotate, got %d (err=%s)", code, errb.String())
	}
}

// extractKV pulls the whitespace-delimited value following key (e.g. "id=") out
// of the first line that contains it.
func extractKV(t *testing.T, out, key string) string {
	t.Helper()
	idx := strings.Index(out, key)
	if idx < 0 {
		t.Fatalf("no %q in output: %s", key, out)
	}
	tail := out[idx+len(key):]
	end := strings.IndexAny(tail, " \t\n\r")
	if end < 0 {
		end = len(tail)
	}
	return tail[:end]
}
