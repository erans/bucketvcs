package v2proto

import (
	"errors"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

func TestEvaluateFreshness_Current(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	bundle := manifest.BundleEntry{
		TipOID:                "tip-current",
		CoversManifestVersion: 5,
		GeneratedAt:           now.Add(-1 * time.Hour).Format(time.RFC3339),
	}
	res := EvaluateFreshness(FreshnessInputs{
		Bundle:        &bundle,
		CurrentTip:    "tip-current",
		IsAncestor:    func(a, d string, max int) bool { return false },
		WalkBack:      func(from, target string, max int) (int, error) { return 0, nil },
		WarmCommits:   100,
		WarmAge:       24 * time.Hour,
		Now:           now,
	})
	if res.State != FreshnessCurrent {
		t.Errorf("got %s, want current", res.State)
	}
}

func TestEvaluateFreshness_Warm(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	bundle := manifest.BundleEntry{
		TipOID:      "old-tip",
		GeneratedAt: now.Add(-1 * time.Hour).Format(time.RFC3339),
	}
	res := EvaluateFreshness(FreshnessInputs{
		Bundle:     &bundle,
		CurrentTip: "new-tip",
		IsAncestor: func(a, d string, max int) bool { return a == "old-tip" && d == "new-tip" },
		WalkBack:   func(from, target string, max int) (int, error) { return 5, nil },
		WarmCommits: 100, WarmAge: 24 * time.Hour, Now: now,
	})
	if res.State != FreshnessWarm {
		t.Errorf("got %s, want warm", res.State)
	}
	if res.CommitsBehind != 5 {
		t.Errorf("CommitsBehind = %d, want 5", res.CommitsBehind)
	}
}

func TestEvaluateFreshness_StaleByAge(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	bundle := manifest.BundleEntry{
		TipOID:      "old-tip",
		GeneratedAt: now.Add(-25 * time.Hour).Format(time.RFC3339),
	}
	res := EvaluateFreshness(FreshnessInputs{
		Bundle:     &bundle,
		CurrentTip: "new-tip",
		IsAncestor: func(a, d string, max int) bool { return true },
		WalkBack:   func(from, target string, max int) (int, error) { return 5, nil },
		WarmCommits: 100, WarmAge: 24 * time.Hour, Now: now,
	})
	if res.State != FreshnessStale {
		t.Errorf("got %s, want stale", res.State)
	}
}

func TestEvaluateFreshness_StaleByForcePush(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	bundle := manifest.BundleEntry{
		TipOID:      "old-tip",
		GeneratedAt: now.Add(-1 * time.Hour).Format(time.RFC3339),
	}
	res := EvaluateFreshness(FreshnessInputs{
		Bundle:     &bundle,
		CurrentTip: "divergent-tip",
		IsAncestor: func(a, d string, max int) bool { return false }, // not an ancestor
		WalkBack:   func(from, target string, max int) (int, error) { return -1, nil },
		WarmCommits: 100, WarmAge: 24 * time.Hour, Now: now,
	})
	if res.State != FreshnessStale {
		t.Errorf("got %s, want stale (force-push case)", res.State)
	}
}

func TestEvaluateFreshness_Retired(t *testing.T) {
	res := EvaluateFreshness(FreshnessInputs{
		Bundle: nil, CurrentTip: "anything",
		WarmCommits: 100, WarmAge: 24 * time.Hour, Now: time.Now(),
	})
	if res.State != FreshnessRetired {
		t.Errorf("got %s, want retired", res.State)
	}
}

func TestEvaluateFreshness_WarmThresholdsMisconfigured(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	bundle := manifest.BundleEntry{
		TipOID:      "old-tip",
		GeneratedAt: now.Add(-1 * time.Hour).Format(time.RFC3339),
	}
	cases := []struct {
		name        string
		warmCommits int
		warmAge     time.Duration
	}{
		{"both zero", 0, 0},
		{"WarmCommits zero", 0, 24 * time.Hour},
		{"WarmAge zero", 100, 0},
		{"WarmCommits negative", -1, 24 * time.Hour},
		{"WarmAge negative", 100, -time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := EvaluateFreshness(FreshnessInputs{
				Bundle:      &bundle,
				CurrentTip:  "new-tip",
				IsAncestor:  func(a, d string, max int) bool { return true },
				WalkBack:    func(from, target string, max int) (int, error) { return 0, nil },
				WarmCommits: tc.warmCommits,
				WarmAge:     tc.warmAge,
				Now:         now,
			})
			if res.State != FreshnessStale {
				t.Errorf("got %s, want stale", res.State)
			}
			if res.Reason != "warm_thresholds_misconfigured" {
				t.Errorf("got reason %q, want warm_thresholds_misconfigured", res.Reason)
			}
		})
	}
}

// "current" should still pass without consulting the thresholds.
func TestEvaluateFreshness_CurrentBypassesMisconfiguration(t *testing.T) {
	bundle := manifest.BundleEntry{TipOID: "tip"}
	res := EvaluateFreshness(FreshnessInputs{
		Bundle:      &bundle,
		CurrentTip:  "tip",
		WarmCommits: 0,
		WarmAge:     0,
		Now:         time.Now(),
	})
	if res.State != FreshnessCurrent {
		t.Errorf("got %s, want current (thresholds shouldn't matter when tip matches)", res.State)
	}
}

func TestEvaluateFreshness_StaleByUnparseableGeneratedAt(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	bundle := manifest.BundleEntry{
		TipOID:      "old-tip",
		GeneratedAt: "not-a-timestamp",
	}
	res := EvaluateFreshness(FreshnessInputs{
		Bundle:      &bundle,
		CurrentTip:  "new-tip",
		IsAncestor:  func(a, d string, max int) bool { return true },
		WalkBack:    func(from, target string, max int) (int, error) { return 0, nil },
		WarmCommits: 100, WarmAge: 24 * time.Hour, Now: now,
	})
	if res.State != FreshnessStale {
		t.Errorf("got %s, want stale (unparseable GeneratedAt)", res.State)
	}
	if res.Reason != "age_unparseable" {
		t.Errorf("got reason %q, want age_unparseable", res.Reason)
	}
}

func TestEvaluateFreshness_StaleByCommitsExceeded(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	bundle := manifest.BundleEntry{
		TipOID:      "old-tip",
		GeneratedAt: now.Add(-1 * time.Hour).Format(time.RFC3339),
	}
	res := EvaluateFreshness(FreshnessInputs{
		Bundle:      &bundle,
		CurrentTip:  "new-tip",
		IsAncestor:  func(a, d string, max int) bool { return true },
		WalkBack:    func(from, target string, max int) (int, error) { return 250, nil }, // > WarmCommits
		WarmCommits: 100, WarmAge: 24 * time.Hour, Now: now,
	})
	if res.State != FreshnessStale {
		t.Errorf("got %s, want stale", res.State)
	}
	if res.Reason != "commits_exceeded" {
		t.Errorf("got reason %q, want commits_exceeded", res.Reason)
	}
	if res.CommitsBehind != 250 {
		t.Errorf("got CommitsBehind=%d, want 250", res.CommitsBehind)
	}
}

func TestEvaluateFreshness_StaleByWalkBackError(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	bundle := manifest.BundleEntry{
		TipOID:      "old-tip",
		GeneratedAt: now.Add(-1 * time.Hour).Format(time.RFC3339),
	}
	res := EvaluateFreshness(FreshnessInputs{
		Bundle:      &bundle,
		CurrentTip:  "new-tip",
		IsAncestor:  func(a, d string, max int) bool { return true },
		WalkBack:    func(from, target string, max int) (int, error) { return 0, errors.New("index decode failure") },
		WarmCommits: 100, WarmAge: 24 * time.Hour, Now: now,
	})
	if res.State != FreshnessStale {
		t.Errorf("got %s, want stale (walkback_error)", res.State)
	}
	if res.Reason != "walkback_error" {
		t.Errorf("got reason %q, want walkback_error", res.Reason)
	}
	if res.CommitsBehind != -1 {
		t.Errorf("got CommitsBehind=%d, want -1 on error", res.CommitsBehind)
	}
}

func TestEvaluateFreshness_StaleByWalkBackNotReachable(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	bundle := manifest.BundleEntry{
		TipOID:      "old-tip",
		GeneratedAt: now.Add(-1 * time.Hour).Format(time.RFC3339),
	}
	res := EvaluateFreshness(FreshnessInputs{
		Bundle:      &bundle,
		CurrentTip:  "new-tip",
		IsAncestor:  func(a, d string, max int) bool { return true },
		WalkBack:    func(from, target string, max int) (int, error) { return -1, nil }, // sentinel: not reachable
		WarmCommits: 100, WarmAge: 24 * time.Hour, Now: now,
	})
	if res.State != FreshnessStale {
		t.Errorf("got %s, want stale (walkback_not_reachable)", res.State)
	}
	if res.Reason != "walkback_not_reachable" {
		t.Errorf("got reason %q, want walkback_not_reachable", res.Reason)
	}
	if res.CommitsBehind != -1 {
		t.Errorf("got CommitsBehind=%d, want -1 on not-reachable sentinel", res.CommitsBehind)
	}
}
