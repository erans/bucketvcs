package maintenance_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/maintenance"
	"github.com/bucketvcs/bucketvcs/internal/maintenance/mtest"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// TestRun_BundleRefresh_RunsWhenMissing: after a Force run on a fresh
// repo, the post-run manifest contains a full_default BundleEntry whose
// TipOID matches refs/heads/main.
func TestRun_BundleRefresh_RunsWhenMissing(t *testing.T) {
	mtest.GitAvailable(t)
	s := mtest.LocalfsStore(t)
	mtest.SeedRepoFromImport(t, s, "acme", "site")

	ctx := context.Background()
	r, err := repo.Open(ctx, s, "acme", "site")
	if err != nil {
		t.Fatal(err)
	}
	k, err := keys.NewRepo("acme", "site")
	if err != nil {
		t.Fatal(err)
	}

	opts := maintenance.RunOptions{Force: true}
	report, err := maintenance.Run(ctx, s, r, k, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.BundleResult == nil {
		t.Fatalf("BundleResult is nil; expected populated for forced run on fresh repo")
	}
	if !report.BundleResult.Generated {
		t.Fatalf("BundleResult.Generated=false (reason=%q, err=%q); want true",
			report.BundleResult.TriggerReason, report.BundleResult.ErrorMessage)
	}

	view, err := r.ReadRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var body manifest.Body
	if err := json.Unmarshal(view.Body, &body); err != nil {
		t.Fatal(err)
	}
	// Assert exactly one full_default entry, and its tip matches refs/heads/main.
	fullDefaults := 0
	var fullEntry manifest.BundleEntry
	for _, b := range body.Bundles {
		if b.Kind == "full_default" {
			fullDefaults++
			fullEntry = b
		}
	}
	if fullDefaults != 1 {
		t.Fatalf("got %d full_default bundles in post-run manifest, want exactly 1: %+v", fullDefaults, body.Bundles)
	}
	if fullEntry.TipOID != body.Refs["refs/heads/main"] {
		t.Errorf("bundle.TipOID = %q, want manifest ref tip %q", fullEntry.TipOID, body.Refs["refs/heads/main"])
	}
}

// TestRun_NoBundle_SkipsBundlePhase: NoBundle=true means report.BundleResult is nil.
func TestRun_NoBundle_SkipsBundlePhase(t *testing.T) {
	mtest.GitAvailable(t)
	s := mtest.LocalfsStore(t)
	mtest.SeedRepoFromImport(t, s, "acme", "site")

	ctx := context.Background()
	r, err := repo.Open(ctx, s, "acme", "site")
	if err != nil {
		t.Fatal(err)
	}
	k, err := keys.NewRepo("acme", "site")
	if err != nil {
		t.Fatal(err)
	}

	opts := maintenance.RunOptions{Force: true, NoBundle: true}
	report, err := maintenance.Run(ctx, s, r, k, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.BundleResult != nil {
		t.Fatalf("BundleResult = %+v; want nil under NoBundle=true", report.BundleResult)
	}
}

// TestRun_BundleRefresh_DryRun documents the current contract:
// DryRun=true makes the pipeline early-return after Materialize, BEFORE
// reaching the bundle phase. report.BundleResult is therefore nil and
// no bundle entries land in the manifest. Task 3.8 may revisit whether
// DryRun should preview the bundle-trigger decision; for Task 3.7 the
// scope is explicit no-op-under-DryRun.
func TestRun_BundleRefresh_DryRun(t *testing.T) {
	mtest.GitAvailable(t)
	s := mtest.LocalfsStore(t)
	mtest.SeedRepoFromImport(t, s, "acme", "site")

	ctx := context.Background()
	r, err := repo.Open(ctx, s, "acme", "site")
	if err != nil {
		t.Fatal(err)
	}
	k, err := keys.NewRepo("acme", "site")
	if err != nil {
		t.Fatal(err)
	}

	opts := maintenance.RunOptions{Force: true, DryRun: true}
	report, err := maintenance.Run(ctx, s, r, k, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.BundleResult != nil {
		t.Fatalf("BundleResult = %+v; want nil under DryRun=true (pipeline early-returns before bundle phase)", report.BundleResult)
	}

	view, err := r.ReadRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var body manifest.Body
	if err := json.Unmarshal(view.Body, &body); err != nil {
		t.Fatal(err)
	}
	for _, b := range body.Bundles {
		if b.Kind == "full_default" {
			t.Errorf("DryRun should not produce a full_default bundle entry; got %+v", b)
		}
	}
}

// TestRun_BundleOnly_SuccessOutcome: BundleOnly=true on a seeded repo
// generates a bundle, skips repack/compact (no new pack), and reports
// Outcome="success_bundle_only".
func TestRun_BundleOnly_SuccessOutcome(t *testing.T) {
	mtest.GitAvailable(t)
	s := mtest.LocalfsStore(t)
	mtest.SeedRepoFromImport(t, s, "acme", "site")

	ctx := context.Background()
	r, err := repo.Open(ctx, s, "acme", "site")
	if err != nil {
		t.Fatal(err)
	}
	k, err := keys.NewRepo("acme", "site")
	if err != nil {
		t.Fatal(err)
	}

	opts := maintenance.RunOptions{Force: true, BundleOnly: true}
	report, err := maintenance.Run(ctx, s, r, k, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Outcome != "success_bundle_only" {
		t.Fatalf("Outcome = %q, want success_bundle_only", report.Outcome)
	}
	if report.NewPackBytes != 0 {
		t.Errorf("NewPackBytes = %d under BundleOnly; want 0", report.NewPackBytes)
	}
	if report.NewPackKey != "" {
		t.Errorf("NewPackKey = %q under BundleOnly; want empty", report.NewPackKey)
	}
	if report.BundleResult == nil || !report.BundleResult.Generated {
		t.Fatalf("BundleResult = %+v; want Generated=true", report.BundleResult)
	}

	// Manifest should have exactly one full_default bundle and the
	// original pack list should be unchanged (no repack happened).
	view, err := r.ReadRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var body manifest.Body
	if err := json.Unmarshal(view.Body, &body); err != nil {
		t.Fatal(err)
	}
	fullDefaults := 0
	for _, b := range body.Bundles {
		if b.Kind == "full_default" {
			fullDefaults++
		}
	}
	if fullDefaults != 1 {
		t.Errorf("got %d full_default bundles, want exactly 1", fullDefaults)
	}
}

// TestRun_BundleOnly_NoRefs_NoopOutcome: BundleOnly=true on an empty repo
// (no packs / no refs) returns Outcome="noop_bundle_only".
func TestRun_BundleOnly_NoRefs_NoopOutcome(t *testing.T) {
	s := mtest.LocalfsStore(t)
	ctx := context.Background()
	if _, err := repo.Create(ctx, s, "acme", "empty", repo.CreateOptions{Actor: "u_test"}); err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	r, err := repo.Open(ctx, s, "acme", "empty")
	if err != nil {
		t.Fatal(err)
	}
	k, err := keys.NewRepo("acme", "empty")
	if err != nil {
		t.Fatal(err)
	}

	opts := maintenance.RunOptions{Force: true, BundleOnly: true}
	report, err := maintenance.Run(ctx, s, r, k, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Outcome != "noop_bundle_only" {
		t.Fatalf("Outcome = %q, want noop_bundle_only (empty repo)", report.Outcome)
	}
}

// TestRun_BundleOnly_DryRun_GeneratesArtifactButNotManifestEntry: under
// BundleOnly + DryRun the bundle phase runs (uploads artifact), but
// RunBundleCASMerge is skipped — so BundleResult.Generated=false and no
// manifest entry is written. Outcome is therefore noop_bundle_only.
func TestRun_BundleOnly_DryRun_GeneratesArtifactButNotManifestEntry(t *testing.T) {
	mtest.GitAvailable(t)
	s := mtest.LocalfsStore(t)
	mtest.SeedRepoFromImport(t, s, "acme", "site")

	ctx := context.Background()
	r, err := repo.Open(ctx, s, "acme", "site")
	if err != nil {
		t.Fatal(err)
	}
	k, err := keys.NewRepo("acme", "site")
	if err != nil {
		t.Fatal(err)
	}

	opts := maintenance.RunOptions{Force: true, BundleOnly: true, DryRun: true}
	report, err := maintenance.Run(ctx, s, r, k, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Outcome != "noop_bundle_only" {
		t.Fatalf("Outcome = %q, want noop_bundle_only (DryRun)", report.Outcome)
	}
	if report.BundleResult == nil {
		t.Fatalf("BundleResult is nil; want populated even under DryRun")
	}
	if report.BundleResult.Generated {
		t.Fatalf("BundleResult.Generated=true under DryRun; want false")
	}

	view, err := r.ReadRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var body manifest.Body
	if err := json.Unmarshal(view.Body, &body); err != nil {
		t.Fatal(err)
	}
	for _, b := range body.Bundles {
		if b.Kind == "full_default" {
			t.Errorf("DryRun BundleOnly should not write a full_default entry; got %+v", b)
		}
	}
}

// TestRun_BundleOnly_WithoutForce_BypassesNoopGate verifies the
// `opts.BundleOnly` term in the Phase-1 gate is load-bearing: with
// Force=false and no triggers fired, BundleOnly must still proceed
// to Phase 1 + the bundle phase. On a fresh repo (no existing bundle)
// the trigger evaluator returns Reason="missing" so a bundle is
// generated and Outcome is success_bundle_only.
func TestRun_BundleOnly_WithoutForce_BypassesNoopGate(t *testing.T) {
	mtest.GitAvailable(t)
	s := mtest.LocalfsStore(t)
	mtest.SeedRepoFromImport(t, s, "acme", "site")

	ctx := context.Background()
	r, err := repo.Open(ctx, s, "acme", "site")
	if err != nil {
		t.Fatal(err)
	}
	k, err := keys.NewRepo("acme", "site")
	if err != nil {
		t.Fatal(err)
	}

	// Force=false; no triggers will fire under the recently-imported
	// repo's pack thresholds. Only BundleOnly should keep the pipeline
	// going past the noop early-return.
	opts := maintenance.RunOptions{BundleOnly: true}
	report, err := maintenance.Run(ctx, s, r, k, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Outcome != "success_bundle_only" {
		t.Fatalf("Outcome = %q, want success_bundle_only (BundleOnly should bypass noop gate without Force)", report.Outcome)
	}
	if report.BundleResult == nil || !report.BundleResult.Generated {
		t.Fatalf("BundleResult = %+v; want Generated=true", report.BundleResult)
	}
	// Trigger reason should be "missing" (no prior bundle), NOT "force".
	if report.BundleResult.TriggerReason != "missing" {
		t.Errorf("TriggerReason = %q, want missing (organic trigger, not force)", report.BundleResult.TriggerReason)
	}
}

// bundleUploadFailingStore wraps an ObjectStore and injects a failure
// on PutIfAbsent calls targeting the bundles/<hash>.bundle key. All
// other operations pass through. Used to exercise the failed_bundle_only
// outcome branch. The intercepted-call counter guards against silent
// key-schema drift: if the production code's bundle key path changes
// and the predicate stops matching, the test asserts that at least one
// upload was intercepted rather than vacuously passing.
type bundleUploadFailingStore struct {
	storage.ObjectStore
	intercepted int
}

func (f *bundleUploadFailingStore) PutIfAbsent(ctx context.Context, key string, body io.Reader, opts *storage.PutOptions) (storage.ObjectVersion, error) {
	if strings.Contains(key, "/bundles/") && strings.HasSuffix(key, ".bundle") {
		f.intercepted++
		return storage.ObjectVersion{}, errors.New("injected bundle upload failure")
	}
	return f.ObjectStore.PutIfAbsent(ctx, key, body, opts)
}

// TestRun_BundleOnly_FailedOutcome injects a storage error on the
// bundle blob upload and verifies the pipeline reports
// Outcome="failed_bundle_only" with BundleResult.ErrorMessage set —
// distinguishing a real failure from a genuine no-op.
func TestRun_BundleOnly_FailedOutcome(t *testing.T) {
	mtest.GitAvailable(t)
	innerS := mtest.LocalfsStore(t)
	mtest.SeedRepoFromImport(t, innerS, "acme", "site")
	s := &bundleUploadFailingStore{ObjectStore: innerS}

	ctx := context.Background()
	r, err := repo.Open(ctx, s, "acme", "site")
	if err != nil {
		t.Fatal(err)
	}
	k, err := keys.NewRepo("acme", "site")
	if err != nil {
		t.Fatal(err)
	}

	opts := maintenance.RunOptions{Force: true, BundleOnly: true}
	report, err := maintenance.Run(ctx, s, r, k, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Outcome != "failed_bundle_only" {
		t.Fatalf("Outcome = %q, want failed_bundle_only", report.Outcome)
	}
	if report.BundleResult == nil || report.BundleResult.ErrorMessage == "" {
		t.Fatalf("BundleResult = %+v; want non-empty ErrorMessage", report.BundleResult)
	}
	if report.BundleResult.Generated {
		t.Errorf("Generated = true under injected failure; want false")
	}
	if s.intercepted == 0 {
		t.Errorf("bundle upload predicate never matched; key schema may have drifted")
	}
}
