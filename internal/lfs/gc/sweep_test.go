package gc_test

import (
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/lfs/gc"
)

func TestApplyRetention_DeletesPastRetention(t *testing.T) {
	now := time.Unix(1700000000, 0)
	cands := []gc.MarkCandidate{
		{OID: "aa", FirstSeenUnreferencedAt: now.Add(-10 * 24 * time.Hour)}, // 10 days old → deletable
		{OID: "bb", FirstSeenUnreferencedAt: now.Add(-3 * time.Hour)},       // 3 hours → skipped
		{OID: "cc", FirstSeenUnreferencedAt: now.Add(-7 * 24 * time.Hour)},  // exactly 7 days → deletable (boundary)
	}
	deletable, skipped := gc.ApplyRetention(cands, now, 7*24*time.Hour)
	if len(deletable) != 2 {
		t.Errorf("deletable len=%d want 2 (got=%v)", len(deletable), deletable)
	}
	if len(skipped) != 1 || skipped[0].OID != "bb" {
		t.Errorf("skipped=%v want one bb", skipped)
	}
	// Deletable sorted by OID.
	if deletable[0].OID != "aa" || deletable[1].OID != "cc" {
		t.Errorf("deletable not sorted by OID: %v", deletable)
	}
}
