package v2proto

import (
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

// helper: build a minimal single-pack slice with a checksum.
func onePack(checksum string) []manifest.PackEntry {
	return []manifest.PackEntry{
		{PackID: "pack-001", PackChecksum: checksum},
	}
}

// helper: build two packs.
func twoPacks() []manifest.PackEntry {
	return []manifest.PackEntry{
		{PackID: "pack-001", PackChecksum: "aabbccdd"},
		{PackID: "pack-002", PackChecksum: "11223344"},
	}
}

// TestFullPackRequested_FullClone_True: empty Haves, 1 pack with checksum,
// wants exactly equal RefTips → true.
func TestFullPackRequested_FullClone_True(t *testing.T) {
	tips := []string{"aabb000000000000000000000000000000000001", "bbcc000000000000000000000000000000000002"}
	result := EvaluateFullPackRequested(FullPackRequestedInputs{
		Wants:   []string{"aabb000000000000000000000000000000000001", "bbcc000000000000000000000000000000000002"},
		Haves:   nil,
		Packs:   onePack("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"),
		RefTips: tips,
	})
	if !result {
		t.Error("expected true for full-clone with single pack and matching wants/reftips")
	}
}

// TestFullPackRequested_HavesPresent_False: same as #1 but Haves non-empty → false.
func TestFullPackRequested_HavesPresent_False(t *testing.T) {
	tips := []string{"aabb000000000000000000000000000000000001"}
	result := EvaluateFullPackRequested(FullPackRequestedInputs{
		Wants:   tips,
		Haves:   []string{"ccdd000000000000000000000000000000000003"},
		Packs:   onePack("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"),
		RefTips: tips,
	})
	if result {
		t.Error("expected false when Haves is non-empty (incremental fetch, not full clone)")
	}
}

// TestFullPackRequested_MultiPack_False: 2 packs in body.Packs → false.
func TestFullPackRequested_MultiPack_False(t *testing.T) {
	tips := []string{"aabb000000000000000000000000000000000001"}
	result := EvaluateFullPackRequested(FullPackRequestedInputs{
		Wants:   tips,
		Haves:   nil,
		Packs:   twoPacks(),
		RefTips: tips,
	})
	if result {
		t.Error("expected false when there are 2 packs (single-pack invariant not met)")
	}
}

// TestFullPackRequested_NoPacks_False: 0 packs → false.
func TestFullPackRequested_NoPacks_False(t *testing.T) {
	tips := []string{"aabb000000000000000000000000000000000001"}
	result := EvaluateFullPackRequested(FullPackRequestedInputs{
		Wants:   tips,
		Haves:   nil,
		Packs:   nil,
		RefTips: tips,
	})
	if result {
		t.Error("expected false when there are 0 packs")
	}
}

// TestFullPackRequested_MissingChecksum_False: 1 pack but PackChecksum=="" → false.
func TestFullPackRequested_MissingChecksum_False(t *testing.T) {
	tips := []string{"aabb000000000000000000000000000000000001"}
	result := EvaluateFullPackRequested(FullPackRequestedInputs{
		Wants:   tips,
		Haves:   nil,
		Packs:   onePack(""), // empty checksum = legacy pack, not M11-backfilled
		RefTips: tips,
	})
	if result {
		t.Error("expected false when pack has no checksum (legacy pack, cannot serve packfile-uri)")
	}
}

// TestFullPackRequested_EmptyRefTips_False: non-empty Wants but no advertised refs → false.
// Symmetric to TestFullPackRequested_EmptyWants_False; covers the bare-empty-repo path.
func TestFullPackRequested_EmptyRefTips_False(t *testing.T) {
	result := EvaluateFullPackRequested(FullPackRequestedInputs{
		Wants:   []string{"aabb000000000000000000000000000000000001"},
		Haves:   nil,
		Packs:   onePack("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"),
		RefTips: nil,
	})
	if result {
		t.Error("expected false when RefTips is empty (nothing advertised)")
	}
}

// TestFullPackRequested_PartialWants_False: wants is a subset of RefTips (not equal) → false.
func TestFullPackRequested_PartialWants_False(t *testing.T) {
	tips := []string{
		"aabb000000000000000000000000000000000001",
		"bbcc000000000000000000000000000000000002",
	}
	result := EvaluateFullPackRequested(FullPackRequestedInputs{
		Wants:   tips[:1], // only first tip
		Haves:   nil,
		Packs:   onePack("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"),
		RefTips: tips,
	})
	if result {
		t.Error("expected false when wants is a strict subset of RefTips")
	}
}

// TestFullPackRequested_ExtraWants_False: wants contains a hash not in RefTips → false.
func TestFullPackRequested_ExtraWants_False(t *testing.T) {
	tips := []string{"aabb000000000000000000000000000000000001"}
	result := EvaluateFullPackRequested(FullPackRequestedInputs{
		Wants: []string{
			"aabb000000000000000000000000000000000001",
			"ffff000000000000000000000000000000000099", // not in RefTips
		},
		Haves:   nil,
		Packs:   onePack("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"),
		RefTips: tips,
	})
	if result {
		t.Error("expected false when wants contains OID not in RefTips")
	}
}

// TestFullPackRequested_EmptyWants_False: wants empty → false (degenerate).
func TestFullPackRequested_EmptyWants_False(t *testing.T) {
	tips := []string{"aabb000000000000000000000000000000000001"}
	result := EvaluateFullPackRequested(FullPackRequestedInputs{
		Wants:   nil,
		Haves:   nil,
		Packs:   onePack("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"),
		RefTips: tips,
	})
	if result {
		t.Error("expected false when wants is empty (degenerate request)")
	}
}

// TestFullPackRequested_DuplicateWants: wants has duplicates that dedupe to RefTips → true
// (test set semantics, not list equality).
func TestFullPackRequested_DuplicateWants(t *testing.T) {
	tip := "aabb000000000000000000000000000000000001"
	result := EvaluateFullPackRequested(FullPackRequestedInputs{
		Wants:   []string{tip, tip, tip}, // duplicates
		Haves:   nil,
		Packs:   onePack("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"),
		RefTips: []string{tip},
	})
	if !result {
		t.Error("expected true: duplicate wants should deduplicate to the same set as RefTips")
	}
}

// TestFullPackRequested_CaseInsensitive: wants/RefTips supplied with mixed case but
// represent same OIDs → true (defensive normalization).
func TestFullPackRequested_CaseInsensitive(t *testing.T) {
	result := EvaluateFullPackRequested(FullPackRequestedInputs{
		Wants:   []string{"AABB000000000000000000000000000000000001"},
		Haves:   nil,
		Packs:   onePack("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"),
		RefTips: []string{"aabb000000000000000000000000000000000001"},
	})
	if !result {
		t.Error("expected true: mixed-case OIDs for same commit should be treated equal")
	}
}
