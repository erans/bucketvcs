package main

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestGC_CLI_LFSFlag_LFSOnlyOnEmptyRepo(t *testing.T) {
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
		"--lfs",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("--lfs exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "LFS GC") {
		t.Errorf("expected text output to mention 'LFS GC'; got: %s", out)
	}
	// Git GC section should NOT be present when --lfs alone.
	if strings.Contains(out, "manifest v") {
		t.Errorf("--lfs alone should suppress Git GC report; got: %s", out)
	}
}

func TestGC_CLI_LFSAndIncludeGitObjects_BothSectionsPresent(t *testing.T) {
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
		"--lfs",
		"--include-git-objects",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("--lfs --include-git-objects exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "LFS GC") {
		t.Errorf("expected 'LFS GC' section in combined output; got: %s", out)
	}
	if !strings.Contains(out, "manifest v") {
		t.Errorf("expected Git GC section ('manifest v') in combined output; got: %s", out)
	}
}

func TestGC_CLI_IncludeGitObjectsWithoutLFS_RejectsUsage(t *testing.T) {
	dir := t.TempDir()
	store, _ := localfs.Open(dir)
	ctx := context.Background()
	_, _ = repo.Create(ctx, store, "acme", "site", repo.CreateOptions{Actor: "u_test"})
	store.Close()

	var stdout, stderr bytes.Buffer
	code := runGC(ctx, []string{
		"--store", "localfs:" + dir,
		"--repo", "acme/site",
		"--include-git-objects",
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("--include-git-objects without --lfs exit = %d, want 2; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--include-git-objects requires --lfs") {
		t.Errorf("expected usage error on stderr; got: %s", stderr.String())
	}
}

func TestGC_CLI_LFSFlag_JSON_HasLFSField(t *testing.T) {
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
		"--lfs",
		"--format", "json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("--lfs json exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, `"lfs":`) {
		t.Errorf("expected JSON output to contain top-level \"lfs\" field; got: %s", out)
	}
	if !strings.Contains(out, `"repo_id":"acme/site"`) {
		t.Errorf("expected repo_id in JSON output; got: %s", out)
	}
	// --lfs alone: top-level git fields should be absent.
	if strings.Contains(out, `"manifest_version":`) {
		t.Errorf("--lfs alone should not emit top-level git fields; got: %s", out)
	}
}

func TestGC_CLI_LFSFlag_MarkOnly_NoSweepInReport(t *testing.T) {
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
		"--lfs",
		"--mark-only",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("--lfs --mark-only exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "LFS GC") {
		t.Errorf("expected 'LFS GC' section; got: %s", out)
	}
	if strings.Contains(out, "lfs-sweep-") {
		t.Errorf("--mark-only should not emit a sweep block; got: %s", out)
	}
}

func TestGC_CLI_LFSFlag_SweepOnlyWithoutPriorMark_ExitsZero(t *testing.T) {
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
		"--lfs",
		"--sweep-only",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("--lfs --sweep-only on empty repo exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	// No prior mark → graceful no-op with a clear "no prior mark"
	// hint so the output isn't a header-only line easily misread as
	// a clean sweep.
	out := stdout.String()
	if !strings.Contains(out, "LFS GC") {
		t.Errorf("expected 'LFS GC' header; got: %s", out)
	}
	if !strings.Contains(out, "no prior mark on disk") {
		t.Errorf("expected 'no prior mark on disk' hint; got: %s", out)
	}
}

func TestGC_CLI_HelpMentionsLFSFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runGC(context.Background(), []string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("--help exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "--lfs") {
		t.Errorf("help missing --lfs flag; got: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "--include-git-objects") {
		t.Errorf("help missing --include-git-objects flag; got: %s", stdout.String())
	}
}

func TestGC_CLI_LFSAndIncludeGitObjects_JSON_BothShapes(t *testing.T) {
	// Pins the load-bearing JSON backward-compat invariant: when both
	// phases run, the top-level Git fields (manifest_version, mark_id,
	// deleted, ...) MUST stay in their original M8 positions so existing
	// jq pipelines keep working; the new "lfs" key sits alongside them.
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
		"--lfs",
		"--include-git-objects",
		"--format", "json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("--lfs --include-git-objects json exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	// Top-level git fields must remain present (M8 jq compatibility).
	for _, key := range []string{`"manifest_version":`, `"mark_id":`, `"deleted":`, `"repo_id":"acme/site"`} {
		if !strings.Contains(out, key) {
			t.Errorf("expected JSON to contain %s in combined-mode output; got: %s", key, out)
		}
	}
	// New "lfs" key sits alongside the flat git fields, not under "git".
	if !strings.Contains(out, `"lfs":`) {
		t.Errorf("expected JSON to contain top-level \"lfs\" key in combined-mode output; got: %s", out)
	}
	if strings.Contains(out, `"git":`) {
		t.Errorf("combined-mode JSON should keep git fields flat at top level (no \"git\" nesting); got: %s", out)
	}
}

func TestGC_CLI_LFS_AllRepos_TouchesEachRepo(t *testing.T) {
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
		"--lfs",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("--lfs --all-repos exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	// LFS section must appear for both repos.
	if strings.Count(out, "LFS GC") != 2 {
		t.Errorf("expected 'LFS GC' header twice (one per repo); got count=%d output: %s",
			strings.Count(out, "LFS GC"), out)
	}
	if !strings.Contains(out, "acme/site") || !strings.Contains(out, "acme/blog") {
		t.Errorf("output missing one of the repos: %s", out)
	}
}

func TestGC_CLI_LFS_JSON_DurationsInSeconds(t *testing.T) {
	// Pins the unit invariant: lfs.mark_duration_seconds /
	// lfs.sweep_duration_seconds must be in seconds (float), not raw
	// nanoseconds. The lfsgc.RunReport struct serializes time.Duration
	// as int64-ns by default — emitting that side by side with the
	// git "_seconds" fields would create a 1e9 unit mismatch in
	// combined mode.
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
		"--lfs",
		"--format", "json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("--lfs json exit = %d, want 0; stderr=%s", code, stderr.String())
	}

	var parsed map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v; got: %s", err, stdout.String())
	}
	lfsRaw, ok := parsed["lfs"].(map[string]any)
	if !ok {
		t.Fatalf("expected 'lfs' object in JSON output; got: %s", stdout.String())
	}
	// Must use the *_seconds keys (parity with git side).
	for _, k := range []string{"mark_duration_seconds", "sweep_duration_seconds"} {
		v, present := lfsRaw[k]
		if !present {
			t.Errorf("lfs JSON missing %q (parity with git fields)", k)
			continue
		}
		// Must serialize as float seconds, not as the int64-ns raw
		// time.Duration default. json.Unmarshal decodes JSON numbers
		// into float64; the unit assertion is that the value is well
		// under 1e6 for a sub-second run (raw ns would be ≥ 1e6 for
		// even microsecond-scale runs).
		f, isFloat := v.(float64)
		if !isFloat {
			t.Errorf("lfs.%s = %T (%v), want float64 seconds", k, v, v)
			continue
		}
		if f > 60 {
			t.Errorf("lfs.%s = %v looks like nanoseconds, not seconds (suspect default time.Duration marshaling)", k, f)
		}
	}
	// Inner repo_id must not duplicate the top-level repo_id key.
	if _, dup := lfsRaw["repo_id"]; dup {
		t.Errorf("lfs sub-document must not carry its own repo_id (it's already at top level); got lfs: %v", lfsRaw)
	}
	// New explicit count fields exist.
	for _, k := range []string{"candidates_count", "deleted_count", "deleted_bytes", "skipped_retention", "errors_count"} {
		if _, ok := lfsRaw[k]; !ok {
			t.Errorf("lfs JSON missing %q", k)
		}
	}
}

func TestGC_CLI_LFS_MarkThenSweepOnly_RoundTrip(t *testing.T) {
	// Round-trip: --lfs --mark-only persists a mark; a follow-up
	// --lfs --sweep-only loads it and surfaces its ID in the report.
	// Also exercises the retention-override warning path (only the
	// mismatched-retention variant fires the warning).
	dir := t.TempDir()
	store, _ := localfs.Open(dir)
	ctx := context.Background()
	if _, err := repo.Create(ctx, store, "acme", "site", repo.CreateOptions{Actor: "u_test"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	store.Close()

	// Phase 1: --lfs --mark-only persists a mark on disk.
	var stdout1, stderr1 bytes.Buffer
	if code := runGC(ctx, []string{
		"--store", "localfs:" + dir, "--repo", "acme/site",
		"--retention", "1s", "--lfs", "--mark-only",
	}, &stdout1, &stderr1); code != 0 {
		t.Fatalf("mark-only exit=%d; stderr=%s", code, stderr1.String())
	}

	// Phase 2: --lfs --sweep-only loads the mark; the sweep block
	// must surface the mark ID it operated against (which means
	// report.MarkRecord was populated from disk, not left empty).
	var stdout2, stderr2 bytes.Buffer
	if code := runGC(ctx, []string{
		"--store", "localfs:" + dir, "--repo", "acme/site",
		"--retention", "1s", "--lfs", "--sweep-only",
	}, &stdout2, &stderr2); code != 0 {
		t.Fatalf("sweep-only exit=%d; stderr=%s", code, stderr2.String())
	}
	out := stdout2.String()
	if !strings.Contains(out, "mark    lfs-") {
		t.Errorf("sweep-only output did not surface mark ID; got: %s", out)
	}
	// Matching retention: no warning.
	if strings.Contains(stderr2.String(), "retention_overridden_by_mark") {
		t.Errorf("retention_overridden_by_mark fired with matching retention; stderr=%s", stderr2.String())
	}

	// Phase 3: --lfs --sweep-only with a mismatched --retention triggers
	// the warning. The sweep itself uses the mark's pinned retention.
	var stdout3, stderr3 bytes.Buffer
	if code := runGC(ctx, []string{
		"--store", "localfs:" + dir, "--repo", "acme/site",
		"--retention", "48h", "--lfs", "--sweep-only",
	}, &stdout3, &stderr3); code != 0 {
		t.Fatalf("sweep-only mismatch exit=%d; stderr=%s", code, stderr3.String())
	}
	if !strings.Contains(stderr3.String(), "lfs_gc.sweep_only.retention_overridden_by_mark") {
		t.Errorf("retention_overridden_by_mark did not fire on mismatched retention; stderr=%s", stderr3.String())
	}
}

func TestGC_CLI_LFS_AllRepos_IncludeGitObjects_BothPhasesPerRepo(t *testing.T) {
	// In --all-repos --lfs --include-git-objects mode, every repo must
	// see both phases run. This complements the single-repo combined
	// test by covering the loop's fall-through structure: when both
	// phases are independent (disjoint storage prefixes), neither
	// should skip the other across repos.
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
		"--lfs",
		"--include-git-objects",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("--all-repos --lfs --include-git-objects exit=%d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	// Two LFS sections (one per repo).
	if got := strings.Count(out, "LFS GC"); got != 2 {
		t.Errorf("expected 2 LFS GC sections; got %d; output: %s", got, out)
	}
	// Two Git sections (one per repo) — "manifest v" is the Git header
	// fingerprint.
	if got := strings.Count(out, "manifest v"); got != 2 {
		t.Errorf("expected 2 Git GC sections ('manifest v'); got %d; output: %s", got, out)
	}
	if !strings.Contains(out, "acme/site") || !strings.Contains(out, "acme/blog") {
		t.Errorf("output missing one of the repos: %s", out)
	}
}

func TestGC_CLI_LFS_DryRun_TextHasDryRunHeader(t *testing.T) {
	// Pins the dry-run visibility invariant: without a sweep block
	// (e.g. --lfs --mark-only --dry-run), the LFS text section must
	// still indicate dry-run mode in the header / mark line.
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
		"--lfs",
		"--mark-only",
		"--dry-run",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("--lfs --mark-only --dry-run exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "DRY RUN — LFS GC") {
		t.Errorf("expected 'DRY RUN — LFS GC' header for --lfs --mark-only --dry-run; got: %s", out)
	}
	if !strings.Contains(out, "(not persisted)") {
		t.Errorf("expected '(not persisted)' annotation on mark line in dry-run mode; got: %s", out)
	}
}
