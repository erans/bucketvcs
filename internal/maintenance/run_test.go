package maintenance_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/maintenance"
	"github.com/bucketvcs/bucketvcs/internal/maintenance/mtest"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/tx"
)

func TestRun_HappyPathProducesOnePackWithFreshIndexes(t *testing.T) {
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
	if report.Outcome != "success" {
		t.Errorf("Outcome = %q, want success", report.Outcome)
	}
	if report.AfterPackCount != 1 {
		t.Errorf("AfterPackCount = %d, want 1", report.AfterPackCount)
	}
	if report.NewPackKey == "" {
		t.Errorf("NewPackKey empty")
	}
	if report.NewObjectMapKey == "" || report.NewCommitGraphKey == "" {
		t.Errorf("index keys empty: %+v", report)
	}

	// Verify post-Run manifest state.
	view, err := r.ReadRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var body manifest.Body
	if err := json.Unmarshal(view.Body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Packs) != 1 {
		t.Errorf("post-Run manifest has %d packs, want 1", len(body.Packs))
	}
	if body.Indexes.ObjectMap == nil || body.Indexes.CommitGraph == nil {
		t.Errorf("post-Run manifest missing indexes")
	}
}

func TestRun_NoOpWhenThresholdsNotTriggered(t *testing.T) {
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
	opts := maintenance.RunOptions{} // no Force, default thresholds; 1-pack repo won't trigger anything
	report, err := maintenance.Run(ctx, s, r, k, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Outcome != "noop" {
		t.Errorf("Outcome = %q, want noop", report.Outcome)
	}
	if report.AfterPackCount != report.BeforePackCount {
		t.Errorf("noop changed pack count: %d → %d", report.BeforePackCount, report.AfterPackCount)
	}
}

func TestRun_DryRunWritesNothing(t *testing.T) {
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
	preView, _ := r.ReadRoot(ctx)
	preVer := preView.Header.ManifestVersion

	opts := maintenance.RunOptions{Force: true, DryRun: true}
	report, err := maintenance.Run(ctx, s, r, k, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Outcome != "success" {
		t.Errorf("Outcome = %q, want success", report.Outcome)
	}
	if !report.DryRun {
		t.Errorf("Report.DryRun = false")
	}
	postView, _ := r.ReadRoot(ctx)
	if postView.Header.ManifestVersion != preVer {
		t.Errorf("dry-run mutated manifest: %d → %d", preVer, postView.Header.ManifestVersion)
	}
}

func TestRun_VersionBumpDuringPipelineRetriesCAS(t *testing.T) {
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

	// The hook fires during the first buildBody callback and before CAS,
	// performing a no-op commit on the same repo, which bumps the manifest
	// version without altering the body content. Maintenance's first CAS
	// attempt will see version mismatch after the hook fires, retry, and
	// succeed on the second pass because the body it constructs is
	// identical (the late commit added no packs to filter against P0Keys).
	hookFired := false
	opts := maintenance.RunOptions{
		Force: true,
		BetweenRepackAndCAS: func() {
			if hookFired {
				return // only fire once
			}
			hookFired = true
			r2, err := repo.Open(ctx, s, "acme", "site")
			if err != nil {
				t.Fatalf("hook: open repo: %v", err)
			}
			if _, err := r2.Commit(ctx,
				tx.Body{Type: "test_bump", Actor: "u_test"},
				func(prev *repo.RootView) ([]byte, error) {
					return prev.Body, nil
				}); err != nil {
				t.Fatalf("hook: bump commit: %v", err)
			}
		},
	}

	report, err := maintenance.Run(ctx, s, r, k, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Outcome != "success" {
		t.Errorf("Outcome = %q, want success", report.Outcome)
	}
	if report.CASAttempts < 2 {
		t.Errorf("CASAttempts = %d, want >= 2 (first attempt should hit version mismatch)", report.CASAttempts)
	}
	if report.AfterPackCount != 1 {
		t.Errorf("AfterPackCount = %d, want 1 (no concurrent push added a pack)", report.AfterPackCount)
	}
}
