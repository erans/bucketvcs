package maintenance

import (
	"context"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/reachability"
	"github.com/bucketvcs/bucketvcs/internal/reachability/rtest"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

func TestEvaluateBundleTriggers_Missing(t *testing.T) {
	m := manifest.Body{
		Refs:    map[string]string{"refs/heads/main": "1111111111111111111111111111111111111111"},
		Bundles: nil,
	}
	th := Thresholds{BundleCommits: 100, BundleAge: 24 * time.Hour}
	res, err := EvaluateBundleTriggers(context.Background(), m, th, "refs/heads/main", time.Now(), nil)
	if err != nil {
		t.Fatalf("EvaluateBundleTriggers: %v", err)
	}
	if !res.Triggered || res.Reason != "missing" {
		t.Fatalf("got triggered=%v reason=%q, want true/missing", res.Triggered, res.Reason)
	}
}

func TestEvaluateBundleTriggers_Age(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	m := manifest.Body{
		Refs: map[string]string{"refs/heads/main": "1111111111111111111111111111111111111111"},
		Bundles: []manifest.BundleEntry{{
			Kind:        "full_default",
			Ref:         "refs/heads/main",
			TipOID:      "1111111111111111111111111111111111111111",
			GeneratedAt: now.Add(-25 * time.Hour).Format(time.RFC3339),
		}},
	}
	th := Thresholds{BundleCommits: 100, BundleAge: 24 * time.Hour}
	res, err := EvaluateBundleTriggers(context.Background(), m, th, "refs/heads/main", now, nil)
	if err != nil {
		t.Fatalf("EvaluateBundleTriggers: %v", err)
	}
	if !res.Triggered || res.Reason != "age" {
		t.Fatalf("got triggered=%v reason=%q, want true/age", res.Triggered, res.Reason)
	}
}

func TestEvaluateBundleTriggers_NoTrigger(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	m := manifest.Body{
		Refs: map[string]string{"refs/heads/main": "1111111111111111111111111111111111111111"},
		Bundles: []manifest.BundleEntry{{
			Kind:        "full_default",
			Ref:         "refs/heads/main",
			TipOID:      "1111111111111111111111111111111111111111",
			GeneratedAt: now.Add(-1 * time.Hour).Format(time.RFC3339),
		}},
	}
	th := Thresholds{BundleCommits: 100, BundleAge: 24 * time.Hour}
	res, err := EvaluateBundleTriggers(context.Background(), m, th, "refs/heads/main", now, nil)
	if err != nil {
		t.Fatalf("EvaluateBundleTriggers: %v", err)
	}
	if res.Triggered {
		t.Fatalf("got triggered=true; want false (tip unchanged, age within threshold)")
	}
}

// TestEvaluateBundleTriggers_UnparseableGeneratedAt verifies that a
// malformed or unknown-format GeneratedAt forces a refresh under the
// "age" reason rather than silently disabling the age policy.
func TestEvaluateBundleTriggers_UnparseableGeneratedAt(t *testing.T) {
	m := manifest.Body{
		Refs: map[string]string{"refs/heads/main": "1111111111111111111111111111111111111111"},
		Bundles: []manifest.BundleEntry{{
			Kind:        "full_default",
			Ref:         "refs/heads/main",
			TipOID:      "1111111111111111111111111111111111111111",
			GeneratedAt: "not-a-timestamp",
		}},
	}
	th := Thresholds{BundleCommits: 100, BundleAge: 24 * time.Hour}
	res, err := EvaluateBundleTriggers(context.Background(), m, th, "refs/heads/main", time.Now(), nil)
	if err != nil {
		t.Fatalf("EvaluateBundleTriggers: %v", err)
	}
	if !res.Triggered || res.Reason != "age" {
		t.Fatalf("got triggered=%v reason=%q, want true/age", res.Triggered, res.Reason)
	}
}

// TestEvaluateBundleTriggers_Commits_BeyondBound exercises the
// commit-distance trigger via a real reachability set. The fixture's
// chain is A→B→C→D; we plant a bundle pinned at A while the ref points
// at D, with BundleCommits=2 so the walk exhausts its bound (the path
// A is 3 commits behind D, exceeding the 2-commit budget). Trigger
// fires with Reason="commits" and CommitsBehind=-1 (true distance
// unknown).
func TestEvaluateBundleTriggers_Commits_BeyondBound(t *testing.T) {
	fx := rtest.NewBaseWithDeltaRepo(t)
	rset, err := reachability.Load(context.Background(), fx.Store, fx.Keys, fx.Body)
	if err != nil {
		t.Fatalf("reachability.Load: %v", err)
	}
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	m := manifest.Body{
		Refs: map[string]string{"refs/heads/main": fx.D.String()},
		Bundles: []manifest.BundleEntry{{
			Kind:        "full_default",
			Ref:         "refs/heads/main",
			TipOID:      fx.A.String(),
			GeneratedAt: now.Add(-1 * time.Hour).Format(time.RFC3339),
		}},
	}
	th := Thresholds{BundleCommits: 2, BundleAge: 24 * time.Hour}
	res, err := EvaluateBundleTriggers(context.Background(), m, th, "refs/heads/main", now, rset)
	if err != nil {
		t.Fatalf("EvaluateBundleTriggers: %v", err)
	}
	if !res.Triggered || res.Reason != "commits" {
		t.Fatalf("got triggered=%v reason=%q, want true/commits", res.Triggered, res.Reason)
	}
	if res.CommitsBehind != -1 {
		t.Errorf("CommitsBehind = %d, want -1 (walk exhausted bound)", res.CommitsBehind)
	}
}

// TestEvaluateBundleTriggers_Commits_WithinBound exercises the
// no-trigger branch when the walk finds the existing tip within the
// commit budget. Same fixture (A→B→C→D); bundle pinned at B while ref
// points at D with BundleCommits=10. Walk depth=2 (D→C→B), result is
// no_trigger with CommitsBehind=2.
func TestEvaluateBundleTriggers_Commits_WithinBound(t *testing.T) {
	fx := rtest.NewBaseWithDeltaRepo(t)
	rset, err := reachability.Load(context.Background(), fx.Store, fx.Keys, fx.Body)
	if err != nil {
		t.Fatalf("reachability.Load: %v", err)
	}
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	m := manifest.Body{
		Refs: map[string]string{"refs/heads/main": fx.D.String()},
		Bundles: []manifest.BundleEntry{{
			Kind:        "full_default",
			Ref:         "refs/heads/main",
			TipOID:      fx.B.String(),
			GeneratedAt: now.Add(-1 * time.Hour).Format(time.RFC3339),
		}},
	}
	th := Thresholds{BundleCommits: 10, BundleAge: 24 * time.Hour}
	res, err := EvaluateBundleTriggers(context.Background(), m, th, "refs/heads/main", now, rset)
	if err != nil {
		t.Fatalf("EvaluateBundleTriggers: %v", err)
	}
	if res.Triggered {
		t.Fatalf("got triggered=true; want false (walk found B within bound)")
	}
	if res.CommitsBehind != 2 {
		t.Errorf("CommitsBehind = %d, want 2 (D→C→B)", res.CommitsBehind)
	}
}

// TestEvaluateBundleTriggers_Commits_ExactBoundary exercises the
// `n == BundleCommits` branch — bundle at B, ref at D, BundleCommits=2.
// The walk traverses D→C→B at depth 2 and returns 2; the evaluator
// reports CommitsBehind=2 with Triggered=true.
func TestEvaluateBundleTriggers_Commits_ExactBoundary(t *testing.T) {
	fx := rtest.NewBaseWithDeltaRepo(t)
	rset, err := reachability.Load(context.Background(), fx.Store, fx.Keys, fx.Body)
	if err != nil {
		t.Fatalf("reachability.Load: %v", err)
	}
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	m := manifest.Body{
		Refs: map[string]string{"refs/heads/main": fx.D.String()},
		Bundles: []manifest.BundleEntry{{
			Kind:        "full_default",
			Ref:         "refs/heads/main",
			TipOID:      fx.B.String(),
			GeneratedAt: now.Add(-1 * time.Hour).Format(time.RFC3339),
		}},
	}
	th := Thresholds{BundleCommits: 2, BundleAge: 24 * time.Hour}
	res, err := EvaluateBundleTriggers(context.Background(), m, th, "refs/heads/main", now, rset)
	if err != nil {
		t.Fatalf("EvaluateBundleTriggers: %v", err)
	}
	if !res.Triggered || res.Reason != "commits" {
		t.Fatalf("got triggered=%v reason=%q, want true/commits", res.Triggered, res.Reason)
	}
	if res.CommitsBehind != 2 {
		t.Errorf("CommitsBehind = %d, want 2 (exact boundary)", res.CommitsBehind)
	}
}

// TestEvaluateBundleTriggers_RefNotInManifest verifies that calling
// with a ref absent from m.Refs returns an explicit error rather than
// silently falling through. The caller (runBundlePhase) is expected
// to resolve the ref via ResolveDefaultBranch before invoking this.
func TestEvaluateBundleTriggers_RefNotInManifest(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	m := manifest.Body{
		Refs: map[string]string{"refs/heads/main": "1111111111111111111111111111111111111111"},
		Bundles: []manifest.BundleEntry{{
			Kind:        "full_default",
			Ref:         "refs/heads/deleted",
			TipOID:      "2222222222222222222222222222222222222222",
			GeneratedAt: now.Add(-1 * time.Hour).Format(time.RFC3339),
		}},
	}
	th := Thresholds{BundleCommits: 100, BundleAge: 24 * time.Hour}
	_, err := EvaluateBundleTriggers(context.Background(), m, th, "refs/heads/deleted", now, nil)
	if err == nil {
		t.Fatalf("expected error when ref is absent from manifest refs")
	}
}

// TestEvaluateBundleTriggers_Commits_NilRset_TipDivergent confirms
// that with rset=nil and a divergent tip, the evaluator falls through
// to no_trigger / CommitsBehind=-1 (the commits-trigger path is
// skipped). The caller is responsible for treating nil rset as
// best-effort.
func TestEvaluateBundleTriggers_Commits_NilRset_TipDivergent(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	m := manifest.Body{
		Refs: map[string]string{"refs/heads/main": "2222222222222222222222222222222222222222"},
		Bundles: []manifest.BundleEntry{{
			Kind:        "full_default",
			Ref:         "refs/heads/main",
			TipOID:      "1111111111111111111111111111111111111111",
			GeneratedAt: now.Add(-1 * time.Hour).Format(time.RFC3339),
		}},
	}
	th := Thresholds{BundleCommits: 100, BundleAge: 24 * time.Hour}
	res, err := EvaluateBundleTriggers(context.Background(), m, th, "refs/heads/main", now, nil)
	if err != nil {
		t.Fatalf("EvaluateBundleTriggers: %v", err)
	}
	if res.Triggered {
		t.Fatalf("got triggered=true; want false (nil rset, commits-check skipped)")
	}
	if res.CommitsBehind != -1 {
		t.Errorf("CommitsBehind = %d, want -1", res.CommitsBehind)
	}
}
