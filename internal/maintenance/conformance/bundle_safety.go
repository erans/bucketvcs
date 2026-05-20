// Package conformance — bundle_safety.go provides RunPropertyBundleSafety,
// the M11 §bundle-uri property-test factory. It verifies:
//
//	(a) Every advertised bundle has TipOID reachable from the current
//	    default-branch tip at the moment of advertise.
//	(b) Bundle files dropped from the manifest become M8 GC orphan
//	    candidates and are reclaimed after retention (deferred sub-cases).
//
// For M11 only the `solo` sub-test is active; the three concurrent
// sub-cases ship as t.Skip stubs consistent with M10's deferred property
// tests.
package conformance

import (
	"context"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/maintenance"
	"github.com/bucketvcs/bucketvcs/internal/maintenance/mtest"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/refstore"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// RunPropertyBundleSafety verifies M11 bundle-uri safety invariants against
// any caller-provided ObjectStore factory. Sub-tests:
//
//	solo                    — BundleOnly run on a seeded repo produces exactly
//	                          one full_default bundle entry.
//	push_during_bundle      — deferred: requires concurrent-push test harness.
//	bundle_during_compaction — deferred: requires concurrent-test harness.
//	sweep_after_retire      — deferred: requires GC + maintenance interleaving.
//
// Each sub-test calls factory(t) for a fresh store. Tests skip when
// `git` is not on PATH.
func RunPropertyBundleSafety(t *testing.T, factory Factory) {
	t.Run("solo", func(t *testing.T) {
		s, cleanup := factory(t)
		defer cleanup()
		runBundleSolo(t, s)
	})
	t.Run("push_during_bundle", func(t *testing.T) {
		t.Skip("requires concurrent-push test harness (deferred to M11.x follow-up)")
	})
	t.Run("bundle_during_compaction", func(t *testing.T) {
		t.Skip("requires concurrent-test harness (deferred to M11.x)")
	})
	t.Run("sweep_after_retire", func(t *testing.T) {
		t.Skip("requires GC + maintenance interleaving harness (deferred to M11.x)")
	})
}

func runBundleSolo(t *testing.T, s storage.ObjectStore) {
	mtest.GitAvailable(t)
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
	rep, err := maintenance.Run(ctx, s, r, k, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.BundleResult == nil || !rep.BundleResult.Generated {
		t.Fatalf("BundleResult = %+v; want Generated=true", rep.BundleResult)
	}

	// Invariant (a): the manifest must contain exactly one full_default bundle
	// and TipOID must be non-empty (reachability guaranteed by bundle generation
	// itself, which runs git-bundle create against the live pack).
	view, err := r.ReadRoot(ctx)
	if err != nil {
		t.Fatalf("ReadRoot: %v", err)
	}
	body, err := manifest.UnmarshalBody(view.Body)
	if err != nil {
		t.Fatalf("UnmarshalBody: %v", err)
	}
	rs, err := refstore.New(ctx, s, k, &body)
	if err != nil {
		t.Fatalf("refstore.New: %v", err)
	}
	if len(body.Bundles) != 1 {
		t.Fatalf("len(body.Bundles) = %d, want 1", len(body.Bundles))
	}
	if body.Bundles[0].Kind != "full_default" {
		t.Errorf("Bundles[0].Kind = %q, want full_default", body.Bundles[0].Kind)
	}
	// Invariant (a): TipOID must equal the current default-branch tip.
	// (GenerateBundleArtifact validates 40-hex format at write time; this
	// assertion verifies the cross-adapter advertise-time invariant the M11
	// bundle-uri spec calls out.)
	wantTip, ok, err := rs.Lookup(ctx, body.DefaultBranch)
	if err != nil {
		t.Fatalf("rs.Lookup default branch: %v", err)
	}
	if !ok || wantTip == "" {
		t.Fatalf("default branch %q not found in refs (or has empty OID)", body.DefaultBranch)
	}
	if body.Bundles[0].TipOID != wantTip {
		t.Errorf("Bundles[0].TipOID = %q, want %q (default branch tip)", body.Bundles[0].TipOID, wantTip)
	}
	assertReachable(t, s, r)
}
