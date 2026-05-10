package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

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

	// Sleep so any candidates age past the 1s retention floor.
	time.Sleep(1100 * time.Millisecond)

	var stdout, stderr bytes.Buffer
	code := runGC(ctx, []string{
		"--store", "localfs:" + dir,
		"--repo", "acme/site",
		"--retention", "1s",
		"--dry-run",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("dry-run exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "sweep") {
		t.Errorf("dry-run text output missing 'sweep' block; got: %s", out)
	}
	if !strings.Contains(out, "mark") {
		t.Errorf("dry-run text output missing 'mark' block; got: %s", out)
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

func TestGC_CLI_DryRun_MarkBlockShowsMarkID(t *testing.T) {
	dir := t.TempDir()
	store, _ := localfs.Open(dir)
	ctx := context.Background()
	if _, err := repo.Create(ctx, store, "acme", "site", repo.CreateOptions{Actor: "u_test"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	store.Close()

	// Sleep so any candidates age past the 1s retention floor.
	time.Sleep(1100 * time.Millisecond)

	var stdout, stderr bytes.Buffer
	code := runGC(ctx, []string{
		"--store", "localfs:" + dir,
		"--repo", "acme/site",
		"--retention", "1s",
		"--dry-run",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("dry-run exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	// Mark block should print a real mark ID (mk_<ulid>), not an empty placeholder.
	if !strings.Contains(out, "mk_") {
		t.Errorf("dry-run mark block missing mk_<id> token; got: %s", out)
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

	// Phase 1: mark-only run writes firstSeenUnreachableAt = now to disk.
	var stdout1, stderr1 bytes.Buffer
	if code := runGC(ctx, []string{
		"--store", "localfs:" + dir,
		"--repo", "acme/site",
		"--retention", "1s",
		"--mark-only",
	}, &stdout1, &stderr1); code != 0 {
		t.Fatalf("mark-only exit = %d; stderr=%s", code, stderr1.String())
	}

	// Sleep so the orphan pack ages past the 1s retention floor.
	// The sweep will see now - firstSeenUnreachableAt ≈ 1.1s > 1s → would-delete.
	time.Sleep(1100 * time.Millisecond)

	// Phase 2: sweep-only dry-run against the mark written in phase 1.
	var stdout, stderr bytes.Buffer
	code := runGC(ctx, []string{
		"--store", "localfs:" + dir,
		"--repo", "acme/site",
		"--retention", "1s",
		"--sweep-only",
		"--dry-run",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("dry-run exit = %d, want 0; stderr=%s", code, stderr.String())
	}

	// Verify the would-delete branch fired: packs=1 in the sweep block, not packs=0.
	out := stdout.String()
	if !strings.Contains(out, "packs=1") {
		t.Errorf("dry-run did not exercise would-delete path; expected packs=1 in sweep block, got: %s", out)
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

func TestGC_CLI_DryRun_TextHasDryRunMarkers(t *testing.T) {
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
		"--retention", "1s",
		"--dry-run",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("dry-run exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "DRY RUN") {
		t.Errorf("expected 'DRY RUN' marker in text output; got: %s", out)
	}
	if !strings.Contains(out, "would-delete") {
		t.Errorf("expected 'would-delete' label in dry-run text output; got: %s", out)
	}
}

func TestGC_CLI_JSON_DeletedSlicesAreEmptyArrays(t *testing.T) {
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
		"--retention", "1h",
		"--mark-only",
		"--format", "json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("mark-only json exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	// Empty slices must serialize as [] not null — jq pipelines rely on this.
	if strings.Contains(out, `"tx_records":null`) || strings.Contains(out, `"canonical_packs":null`) || strings.Contains(out, `"indexes":null`) {
		t.Fatalf("deleted slices serialized as null instead of []; got: %s", out)
	}
	if !strings.Contains(out, `"tx_records":[]`) || !strings.Contains(out, `"canonical_packs":[]`) || !strings.Contains(out, `"indexes":[]`) {
		t.Fatalf("expected empty arrays for all three deleted categories; got: %s", out)
	}
}
