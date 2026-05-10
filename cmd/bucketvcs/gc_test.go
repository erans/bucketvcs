package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestGC_CLI_HelpExitsZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runGC(context.Background(), []string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("--help exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "--retention") {
		t.Errorf("help missing --retention; got: %s", stdout.String())
	}
}

func TestGC_CLI_RepoXorAllReposRequired(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runGC(context.Background(), []string{"--store", "localfs:" + t.TempDir()}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("missing --repo / --all-repos exit = %d, want 2; stderr=%s", code, stderr.String())
	}
}

func TestGC_CLI_SingleRepo_HappyPath(t *testing.T) {
	dir := t.TempDir()
	store, _ := localfs.Open(dir)
	ctx := context.Background()
	if _, err := repo.Create(ctx, store, "acme", "site", repo.CreateOptions{Actor: "u_test"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Release the lock so runGC can open the same directory.
	if err := store.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runGC(ctx, []string{
		"--store", "localfs:" + dir,
		"--repo", "acme/site",
		"--retention", "1s",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("happy path exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "acme/site") {
		t.Errorf("expected stdout to mention acme/site; got: %s", stdout.String())
	}
}

func TestGC_CLI_RetentionWarningBelow24h(t *testing.T) {
	dir := t.TempDir()
	store, _ := localfs.Open(dir)
	ctx := context.Background()
	_, _ = repo.Create(ctx, store, "acme", "site", repo.CreateOptions{Actor: "u_test"})
	// Release the lock so runGC can open the same directory.
	_ = store.Close()

	var stdout, stderr bytes.Buffer
	_ = runGC(ctx, []string{
		"--store", "localfs:" + dir, "--repo", "acme/site",
		"--retention", "1s",
	}, &stdout, &stderr)
	if !strings.Contains(stderr.String(), "below 24h") {
		t.Errorf("expected retention warning on stderr; got: %s", stderr.String())
	}
}

func TestGC_CLI_DryRun_TextOutputShowsSweepBlock(t *testing.T) {
	dir := t.TempDir()
	store, _ := localfs.Open(dir)
	ctx := context.Background()
	if _, err := repo.Create(ctx, store, "acme", "site", repo.CreateOptions{Actor: "u_test"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	store.Close()

	var stdout, stderr bytes.Buffer
	code := runGC(ctx, []string{
		"--store", "localfs:" + dir,
		"--repo", "acme/site",
		"--retention", "1ms",
		"--dry-run",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("dry-run exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "sweep") {
		t.Errorf("dry-run text output missing 'sweep' block; got: %s", out)
	}
}

func TestGC_CLI_AllRepos_TouchesEachRepo(t *testing.T) {
	dir := t.TempDir()
	store, _ := localfs.Open(dir)
	ctx := context.Background()
	if _, err := repo.Create(ctx, store, "acme", "site", repo.CreateOptions{Actor: "u_test"}); err != nil {
		t.Fatalf("Create site: %v", err)
	}
	if _, err := repo.Create(ctx, store, "acme", "blog", repo.CreateOptions{Actor: "u_test"}); err != nil {
		t.Fatalf("Create blog: %v", err)
	}
	store.Close()

	var stdout, stderr bytes.Buffer
	code := runGC(ctx, []string{
		"--store", "localfs:" + dir,
		"--all-repos",
		"--retention", "1s",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "acme/site") || !strings.Contains(out, "acme/blog") {
		t.Errorf("output missing one of the repos: %s", out)
	}
}

func TestGC_CLI_DryRun_NoDelete(t *testing.T) {
	dir := t.TempDir()
	store, _ := localfs.Open(dir)
	ctx := context.Background()
	_, _ = repo.Create(ctx, store, "acme", "site", repo.CreateOptions{Actor: "u_test"})

	// Drop an orphan pack to be a sweep candidate.
	if _, err := store.PutIfAbsent(ctx, "tenants/acme/repos/site/packs/canonical/orphan.pack", strings.NewReader(""), nil); err != nil {
		t.Fatalf("seed orphan: %v", err)
	}
	store.Close()

	var stdout, stderr bytes.Buffer
	code := runGC(ctx, []string{
		"--store", "localfs:" + dir,
		"--repo", "acme/site",
		"--retention", "1ms",
		"--dry-run",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("dry-run exit = %d, want 0; stderr=%s", code, stderr.String())
	}

	// Re-open the store to verify the orphan still exists.
	store2, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store2.Close()
	if _, err := store2.Head(ctx, "tenants/acme/repos/site/packs/canonical/orphan.pack"); err != nil {
		t.Errorf("dry-run deleted orphan: %v", err)
	}
}
