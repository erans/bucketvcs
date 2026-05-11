package maintenance_test

import (
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/maintenance"
)

func TestThresholds_Defaults(t *testing.T) {
	d := maintenance.DefaultThresholds()
	if d.RecentPackCount != 1000 {
		t.Errorf("RecentPackCount default = %d, want 1000", d.RecentPackCount)
	}
	if d.TotalPackCount != 10000 {
		t.Errorf("TotalPackCount default = %d, want 10000", d.TotalPackCount)
	}
	if d.ManifestPackBytes != 8<<20 {
		t.Errorf("ManifestPackBytes default = %d, want %d", d.ManifestPackBytes, 8<<20)
	}
}

func TestRunOptions_NormalizeApplyDefaults(t *testing.T) {
	o := maintenance.RunOptions{}
	o.Normalize()
	if o.CASRetry != maintenance.DefaultCASRetry {
		t.Errorf("CASRetry default = %d, want %d", o.CASRetry, maintenance.DefaultCASRetry)
	}
	if o.RecentWindow != maintenance.DefaultRecentWindow {
		t.Errorf("RecentWindow default = %v, want %v", o.RecentWindow, maintenance.DefaultRecentWindow)
	}
	if o.Logger == nil {
		t.Errorf("Logger default = nil, want slog.Default()")
	}
	if o.Now == nil {
		t.Errorf("Now default = nil, want time.Now")
	}
}

func TestRunOptions_NormalizePreservesCallerValues(t *testing.T) {
	o := maintenance.RunOptions{
		CASRetry:     12,
		RecentWindow: 7 * time.Hour,
		Actor:        "u_test",
	}
	o.Normalize()
	if o.CASRetry != 12 {
		t.Errorf("CASRetry = %d, want 12 (caller value preserved)", o.CASRetry)
	}
	if o.RecentWindow != 7*time.Hour {
		t.Errorf("RecentWindow = %v, want 7h (caller value preserved)", o.RecentWindow)
	}
}

func TestRunOptions_ValidateRejectsSubHourWindow(t *testing.T) {
	o := maintenance.RunOptions{RecentWindow: 30 * time.Minute}
	o.Normalize()
	// Normalize should NOT bump a caller-set sub-hour window up; it bumps zero.
	// 30m is a non-zero caller value, so Normalize preserves it, and Validate rejects.
	if err := o.Validate(); err == nil {
		t.Fatal("Validate accepted sub-1h RecentWindow; want error")
	}
}

// TestRunOptions_NormalizeBumpsZeroCASRetry confirms the
// Normalize/Validate contract: callers may pass CASRetry=0 (or any
// non-positive value); Normalize bumps it to DefaultCASRetry and
// Validate accepts the result.
func TestRunOptions_NormalizeBumpsZeroCASRetry(t *testing.T) {
	o := maintenance.RunOptions{CASRetry: 0}
	o.Normalize()
	if o.CASRetry != maintenance.DefaultCASRetry {
		t.Fatalf("after Normalize CASRetry = %d, want %d", o.CASRetry, maintenance.DefaultCASRetry)
	}
	if err := o.Validate(); err != nil {
		t.Fatalf("Validate after Normalize: %v", err)
	}
}

func TestThresholds_ReachabilityDefaults(t *testing.T) {
	d := maintenance.DefaultThresholds()
	if d.ReachabilityDeltaCommits != 1000 {
		t.Errorf("ReachabilityDeltaCommits = %d, want 1000", d.ReachabilityDeltaCommits)
	}
	if d.ReachabilityDeltaPushes != 100 {
		t.Errorf("ReachabilityDeltaPushes = %d, want 100", d.ReachabilityDeltaPushes)
	}
	if d.ReachabilityDeltaBytes != 64*1024*1024 {
		t.Errorf("ReachabilityDeltaBytes = %d, want 64MiB", d.ReachabilityDeltaBytes)
	}
}
