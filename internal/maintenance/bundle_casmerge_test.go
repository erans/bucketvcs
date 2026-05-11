package maintenance_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/maintenance"
	"github.com/bucketvcs/bucketvcs/internal/maintenance/mtest"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

func TestMergeBundleEntry_ReplacesExistingFullDefault(t *testing.T) {
	base := manifest.Body{
		DefaultBranch: "refs/heads/main",
		Refs:          map[string]string{"refs/heads/main": "1111111111111111111111111111111111111111"},
		Packs: []manifest.PackEntry{{
			PackID: "p", PackKey: "k", IdxKey: "i", SizeBytes: 1, ObjectCount: 1,
		}},
		Bundles: []manifest.BundleEntry{{
			Kind: "full_default", ID: "old", BundleKey: "bk", SidecarKey: "sk", BundleHash: "sha256-old",
			Ref: "refs/heads/main", TipOID: "0000000000000000000000000000000000000000",
			CoversManifestVersion: 1, ByteSize: 10, GeneratedAt: "2026-05-09T00:00:00Z",
		}},
	}
	fresh := manifest.BundleEntry{
		Kind: "full_default", ID: "new", BundleKey: "bk2", SidecarKey: "sk2", BundleHash: "sha256-new",
		Ref: "refs/heads/main", TipOID: "1111111111111111111111111111111111111111",
		CoversManifestVersion: 2, ByteSize: 20, GeneratedAt: "2026-05-10T00:00:00Z",
	}

	got, err := maintenance.MergeBundleEntry(base, fresh)
	if err != nil {
		t.Fatalf("MergeBundleEntry: %v", err)
	}
	if len(got.Bundles) != 1 {
		t.Fatalf("Bundles len = %d, want 1", len(got.Bundles))
	}
	if got.Bundles[0].ID != "new" {
		t.Fatalf("merged Bundles[0].ID = %q, want new", got.Bundles[0].ID)
	}
	if len(got.Packs) != 1 || got.Packs[0].PackID != "p" {
		t.Fatalf("packs disturbed: %+v", got.Packs)
	}
	if got.DefaultBranch != "refs/heads/main" {
		t.Fatalf("default branch dropped: %q", got.DefaultBranch)
	}
}

func TestMergeBundleEntry_AddWhenAbsent(t *testing.T) {
	base := manifest.Body{
		Refs:    map[string]string{"refs/heads/main": "abc"},
		Bundles: nil,
	}
	fresh := manifest.BundleEntry{
		Kind: "full_default", ID: "new", Ref: "refs/heads/main",
		TipOID: "abc", CoversManifestVersion: 1, ByteSize: 1, GeneratedAt: "2026-05-10T00:00:00Z",
	}
	got, err := maintenance.MergeBundleEntry(base, fresh)
	if err != nil {
		t.Fatalf("MergeBundleEntry: %v", err)
	}
	if len(got.Bundles) != 1 || got.Bundles[0].ID != "new" {
		t.Fatalf("Bundles = %+v, want 1 entry", got.Bundles)
	}
}

func TestMergeBundleEntry_RejectsNonFullDefault(t *testing.T) {
	base := manifest.Body{Refs: map[string]string{"refs/heads/main": "abc"}}
	fresh := manifest.BundleEntry{Kind: "rolling_increment", ID: "new"}
	if _, err := maintenance.MergeBundleEntry(base, fresh); err == nil {
		t.Fatal("expected error for non-full_default Kind")
	}
}

// TestMergeBundleEntry_PreservesOtherKinds pins the forward-compat
// contract: a non-full_default existing entry (e.g. a future
// rolling_increment) must survive a merge that replaces only the
// full_default entry.
func TestMergeBundleEntry_PreservesOtherKinds(t *testing.T) {
	base := manifest.Body{
		Refs: map[string]string{"refs/heads/main": "1111111111111111111111111111111111111111"},
		Bundles: []manifest.BundleEntry{
			{
				Kind: "rolling_increment", ID: "roll1", BundleKey: "rk1", SidecarKey: "rsk1",
				BundleHash: "sha256-r1", Ref: "refs/heads/main",
				TipOID:                "1111111111111111111111111111111111111111",
				CoversManifestVersion: 1, ByteSize: 5, GeneratedAt: "2026-05-09T00:00:00Z",
			},
			{
				Kind: "full_default", ID: "old", BundleKey: "bk", SidecarKey: "sk",
				BundleHash: "sha256-old", Ref: "refs/heads/main",
				TipOID:                "0000000000000000000000000000000000000000",
				CoversManifestVersion: 1, ByteSize: 10, GeneratedAt: "2026-05-09T00:00:00Z",
			},
		},
	}
	fresh := manifest.BundleEntry{
		Kind: "full_default", ID: "new", BundleKey: "bk2", SidecarKey: "sk2",
		BundleHash: "sha256-new", Ref: "refs/heads/main",
		TipOID:                "1111111111111111111111111111111111111111",
		CoversManifestVersion: 2, ByteSize: 20, GeneratedAt: "2026-05-10T00:00:00Z",
	}
	got, err := maintenance.MergeBundleEntry(base, fresh)
	if err != nil {
		t.Fatalf("MergeBundleEntry: %v", err)
	}
	if len(got.Bundles) != 2 {
		t.Fatalf("Bundles len = %d, want 2 (rolling_increment + new full_default)", len(got.Bundles))
	}
	// Find each kind and assert which entry survived/replaced.
	var roll, full *manifest.BundleEntry
	for i := range got.Bundles {
		switch got.Bundles[i].Kind {
		case "rolling_increment":
			roll = &got.Bundles[i]
		case "full_default":
			full = &got.Bundles[i]
		}
	}
	if roll == nil || roll.ID != "roll1" {
		t.Errorf("rolling_increment entry dropped or mutated: %+v", roll)
	}
	if full == nil || full.ID != "new" {
		t.Errorf("full_default entry not replaced: %+v", full)
	}
}

// TestRunBundleCASMerge_End2End uses the mtest fixture to import a
// real repo into a localfs-backed store, then exercises BOTH branches
// of MergeBundleEntry through the CAS round-trip (json marshal →
// manifest write → re-read):
//   1. Add-when-absent: seeded manifest has Bundles=nil, first call
//      lands a stub full_default entry.
//   2. Replace-existing: a second call with a different fresh entry
//      replaces the prior one (still exactly one full_default after).
func TestRunBundleCASMerge_End2End(t *testing.T) {
	mtest.GitAvailable(t)
	s := mtest.LocalfsStore(t)
	mtest.SeedRepoFromImport(t, s, "acme", "site")

	ctx := context.Background()
	r, err := repo.Open(ctx, s, "acme", "site")
	if err != nil {
		t.Fatal(err)
	}

	first := manifest.BundleEntry{
		ID: "first", Kind: "full_default", BundleKey: "bk1", SidecarKey: "sk1",
		BundleHash: "sha256-first", Ref: "refs/heads/main",
		TipOID:                "0000000000000000000000000000000000000000",
		CoversManifestVersion: 1, ByteSize: 10, GeneratedAt: "2026-05-10T00:00:00Z",
	}
	if err := maintenance.RunBundleCASMerge(ctx, r, first, "u_test", 0); err != nil {
		t.Fatalf("first RunBundleCASMerge: %v", err)
	}

	view1, err := r.ReadRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var m1 manifest.Body
	if err := json.Unmarshal(view1.Body, &m1); err != nil {
		t.Fatal(err)
	}
	if len(m1.Bundles) != 1 || m1.Bundles[0].ID != "first" {
		t.Fatalf("post-first manifest bundles = %+v", m1.Bundles)
	}

	// Replace branch: a second CAS-merge with a different fresh entry
	// must drop the prior full_default and leave exactly the new one.
	second := manifest.BundleEntry{
		ID: "second", Kind: "full_default", BundleKey: "bk2", SidecarKey: "sk2",
		BundleHash: "sha256-second", Ref: "refs/heads/main",
		TipOID:                "1111111111111111111111111111111111111111",
		CoversManifestVersion: 2, ByteSize: 20, GeneratedAt: "2026-05-11T00:00:00Z",
	}
	if err := maintenance.RunBundleCASMerge(ctx, r, second, "u_test", 0); err != nil {
		t.Fatalf("second RunBundleCASMerge: %v", err)
	}

	view2, err := r.ReadRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var m2 manifest.Body
	if err := json.Unmarshal(view2.Body, &m2); err != nil {
		t.Fatal(err)
	}
	if len(m2.Bundles) != 1 || m2.Bundles[0].ID != "second" {
		t.Fatalf("post-second manifest bundles = %+v (expected exactly one with ID=second)", m2.Bundles)
	}
	if m2.Bundles[0].BundleHash != "sha256-second" || m2.Bundles[0].CoversManifestVersion != 2 {
		t.Fatalf("post-second Bundle entry not the freshly merged one: %+v", m2.Bundles[0])
	}
}
