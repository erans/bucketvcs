package maintenance

import (
	"encoding/json"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

func TestBuildMergedBody_NoConcurrentChange(t *testing.T) {
	prev := manifest.Body{
		DefaultBranch: "main",
		Refs:          map[string]string{"refs/heads/main": "deadbeef"},
		Packs: []manifest.PackEntry{
			{PackID: "old1", PackKey: "K1", IdxKey: "I1"},
			{PackID: "old2", PackKey: "K2", IdxKey: "I2"},
		},
		Indexes: manifest.Indexes{
			ObjectMap:   &manifest.IndexRef{Key: "old-bvom", Hash: "h1"},
			CommitGraph: &manifest.IndexRef{Key: "old-bvcg", Hash: "h2"},
		},
	}
	in := mergeInput{
		P0Keys: []string{"K1", "K2"},
		NewPack: manifest.PackEntry{
			PackID: "new1", PackKey: "Knew", IdxKey: "Inew",
			SizeBytes: 100, ObjectCount: 5,
		},
		NewObjectMap:   manifest.IndexRef{Key: "new-bvom", Hash: "h3"},
		NewCommitGraph: manifest.IndexRef{Key: "new-bvcg", Hash: "h4"},
	}
	got := buildMergedBody(prev, in)
	if len(got.Packs) != 1 || got.Packs[0].PackID != "new1" {
		t.Fatalf("Packs = %+v, want exactly [new1]", got.Packs)
	}
	if got.Indexes.ObjectMap == nil || got.Indexes.ObjectMap.Key != "new-bvom" {
		t.Errorf("ObjectMap not updated: %+v", got.Indexes.ObjectMap)
	}
	if got.Refs["refs/heads/main"] != "deadbeef" {
		t.Errorf("Refs not preserved")
	}
	if got.DefaultBranch != "main" {
		t.Errorf("DefaultBranch not preserved")
	}
}

func TestBuildMergedBody_KeepsLatePushPacks(t *testing.T) {
	prev := manifest.Body{
		DefaultBranch: "main",
		Refs:          map[string]string{"refs/heads/main": "newtip"},
		Packs: []manifest.PackEntry{
			{PackID: "old1", PackKey: "K1", IdxKey: "I1"},
			{PackID: "old2", PackKey: "K2", IdxKey: "I2"},
			{PackID: "late", PackKey: "Klate", IdxKey: "Ilate"},
		},
	}
	in := mergeInput{
		P0Keys:  []string{"K1", "K2"},
		NewPack: manifest.PackEntry{PackID: "new1", PackKey: "Knew", IdxKey: "Inew"},
	}
	got := buildMergedBody(prev, in)
	if len(got.Packs) != 2 {
		t.Fatalf("got %d packs, want 2", len(got.Packs))
	}
	if got.Packs[0].PackID != "new1" {
		t.Errorf("Packs[0] = %s, want new1", got.Packs[0].PackID)
	}
	if got.Packs[1].PackID != "late" {
		t.Errorf("Packs[1] = %s, want late", got.Packs[1].PackID)
	}
}

func TestBuildMergedBody_RoundTrips(t *testing.T) {
	in := mergeInput{
		P0Keys:         []string{"K"},
		NewPack:        manifest.PackEntry{PackID: "n", PackKey: "Knew", IdxKey: "Inew"},
		NewObjectMap:   manifest.IndexRef{Key: "BV", Hash: "H"},
		NewCommitGraph: manifest.IndexRef{Key: "CG", Hash: "H2"},
	}
	prev := manifest.Body{
		DefaultBranch: "main",
		Refs:          map[string]string{"r": "o"},
		Packs:         []manifest.PackEntry{{PackKey: "K"}},
	}
	body := buildMergedBody(prev, in)
	bytes, err := manifest.MarshalBody(body)
	if err != nil {
		t.Fatal(err)
	}
	var rt manifest.Body
	if err := json.Unmarshal(bytes, &rt); err != nil {
		t.Fatal(err)
	}
	if rt.Packs[0].PackID != "n" {
		t.Errorf("round trip lost new pack")
	}
}

func TestTrimConsumedByHash_PreservesConcurrentTail(t *testing.T) {
	consumed := map[string]struct{}{"d1": {}, "d2": {}, "d3": {}, "d4": {}, "d5": {}}
	r := &manifest.ReachabilityRef{
		BaseManifest: "v00000003",
		Deltas: []manifest.IndexRef{
			{Hash: "d1"}, {Hash: "d2"}, {Hash: "d3"}, {Hash: "d4"}, {Hash: "d5"},
			{Hash: "d6"}, {Hash: "d7"},
		},
	}
	got := trimConsumedByHash(r, consumed, "v00000010", true)
	if got.BaseManifest != "v00000010" {
		t.Errorf("BaseManifest = %q, want v00000010", got.BaseManifest)
	}
	if len(got.Deltas) != 2 || got.Deltas[0].Hash != "d6" || got.Deltas[1].Hash != "d7" {
		t.Errorf("Deltas = %+v, want [d6 d7]", got.Deltas)
	}
}

func TestTrimConsumedByHash_TwoCompactionsRace(t *testing.T) {
	// Our consumed set was {d1, d2, d3} but another compaction already
	// emptied them; prev.Deltas is now just [d_new] introduced by a push
	// after the other compaction landed.
	consumed := map[string]struct{}{"d1": {}, "d2": {}, "d3": {}}
	prev := &manifest.ReachabilityRef{
		BaseManifest: "v10",
		Deltas:       []manifest.IndexRef{{Hash: "d_new"}},
	}
	got := trimConsumedByHash(prev, consumed, "v11", true)
	// d_new is not in consumed; preserve it.
	if len(got.Deltas) != 1 || got.Deltas[0].Hash != "d_new" {
		t.Fatalf("got %+v, expected [d_new]", got)
	}
}

func TestTrimConsumedByHash_AllConsumed(t *testing.T) {
	consumed := map[string]struct{}{"d1": {}, "d2": {}}
	r := &manifest.ReachabilityRef{Deltas: []manifest.IndexRef{{Hash: "d1"}, {Hash: "d2"}}}
	got := trimConsumedByHash(r, consumed, "v1", true)
	if len(got.Deltas) != 0 {
		t.Errorf("len = %d, want 0", len(got.Deltas))
	}
}

func TestTrimConsumedByHash_NilReachability(t *testing.T) {
	got := trimConsumedByHash(nil, map[string]struct{}{"d1": {}}, "v2", true)
	if got == nil {
		t.Fatalf("expected non-nil result for nil input")
	}
	if len(got.Deltas) != 0 {
		t.Errorf("len = %d, want 0", len(got.Deltas))
	}
}

func TestTrimConsumedByHash_EmptyConsumed(t *testing.T) {
	r := &manifest.ReachabilityRef{
		Deltas: []manifest.IndexRef{{Hash: "d1"}, {Hash: "d2"}},
	}
	got := trimConsumedByHash(r, map[string]struct{}{}, "v5", false)
	if len(got.Deltas) != 2 {
		t.Errorf("len = %d, want 2 (empty consumed = no trim)", len(got.Deltas))
	}
}

func TestTrimConsumedByHash_NoConsumption_PreservesPrev(t *testing.T) {
	// When consumedHashes is empty and prev is non-nil, the function must
	// preserve prev verbatim — in particular it must NOT advance BaseManifest
	// to the caller-supplied baseVersion, because no actual compaction occurred.
	r := &manifest.ReachabilityRef{
		BaseManifest: "v00000003",
		Deltas:       []manifest.IndexRef{{Hash: "d1"}, {Hash: "d2"}},
	}
	got := trimConsumedByHash(r, map[string]struct{}{}, "v00000099", false)
	if got.BaseManifest != "v00000003" {
		t.Errorf("BaseManifest = %q, want v00000003 (must not advance on no-op)", got.BaseManifest)
	}
	if len(got.Deltas) != 2 || got.Deltas[0].Hash != "d1" || got.Deltas[1].Hash != "d2" {
		t.Errorf("Deltas = %+v, want [d1 d2]", got.Deltas)
	}
	// Returned pointer must not alias the original.
	got.Deltas[0].Hash = "mutated"
	if r.Deltas[0].Hash != "d1" {
		t.Error("returned Deltas slice aliases original — must be a copy")
	}
}

func TestTrimConsumedByHash_RepackPathWithEmptyConsumed_AdvancesBase(t *testing.T) {
	// Repack path: m0 had no Reachability, but a concurrent push added one
	// during the run. consumedHashes is empty, yet advanceBaseManifest=true
	// because the base index was rebuilt. BaseManifest must advance; Deltas
	// from the concurrent push must be preserved.
	r := &manifest.ReachabilityRef{
		BaseManifest: "v00000003",
		Deltas:       []manifest.IndexRef{{Hash: "d_concurrent"}},
	}
	got := trimConsumedByHash(r, map[string]struct{}{}, "v00000099", true)
	if got.BaseManifest != "v00000099" {
		t.Errorf("BaseManifest = %q, want v00000099 (must advance on repack)", got.BaseManifest)
	}
	if len(got.Deltas) != 1 || got.Deltas[0].Hash != "d_concurrent" {
		t.Errorf("Deltas = %+v, want [d_concurrent]", got.Deltas)
	}
	// Returned pointer must not alias the original.
	got.Deltas[0].Hash = "mutated"
	if r.Deltas[0].Hash != "d_concurrent" {
		t.Error("returned Deltas slice aliases original — must be a copy")
	}
}

func TestBuildMergedBody_TrimsConsumedDeltas(t *testing.T) {
	prev := manifest.Body{
		DefaultBranch: "main",
		Refs:          map[string]string{"refs/heads/main": "abc"},
		Packs: []manifest.PackEntry{
			{PackID: "old1", PackKey: "K1"},
		},
		Indexes: manifest.Indexes{
			Reachability: &manifest.ReachabilityRef{
				Deltas: []manifest.IndexRef{
					{Hash: "d1"}, {Hash: "d2"}, {Hash: "d3"},
					{Hash: "d4"}, // concurrent push delta
				},
			},
		},
	}
	in := mergeInput{
		P0Keys:             []string{"K1"},
		NewPack:            manifest.PackEntry{PackID: "new1", PackKey: "Knew"},
		NewObjectMap:       manifest.IndexRef{Key: "bvom", Hash: "h1"},
		NewCommitGraph:     manifest.IndexRef{Key: "bvcg", Hash: "h2"},
		ConsumedHashes:     map[string]struct{}{"d1": {}, "d2": {}, "d3": {}}, // consumed d1, d2, d3; d4 was concurrent
		ConsumedDeltaCount: 3,
		BaseManifest:       "v00000010",
	}
	got := buildMergedBody(prev, in)
	if got.Indexes.Reachability == nil {
		t.Fatalf("Reachability is nil")
	}
	if len(got.Indexes.Reachability.Deltas) != 1 || got.Indexes.Reachability.Deltas[0].Hash != "d4" {
		t.Errorf("Deltas = %+v, want [d4]", got.Indexes.Reachability.Deltas)
	}
	if got.Indexes.Reachability.BaseManifest != "v00000010" {
		t.Errorf("BaseManifest = %q, want v00000010", got.Indexes.Reachability.BaseManifest)
	}
}

func TestBuildCompactOnlyBody_PreservesObjectMap(t *testing.T) {
	prevObjMap := &manifest.IndexRef{Key: "old-bvom", Hash: "h-bvom"}
	prev := manifest.Body{
		DefaultBranch: "main",
		Refs:          map[string]string{"refs/heads/main": "abc"},
		Packs: []manifest.PackEntry{
			{PackID: "p1", PackKey: "K1"},
		},
		Indexes: manifest.Indexes{
			ObjectMap:   prevObjMap,
			CommitGraph: &manifest.IndexRef{Key: "old-bvcg", Hash: "h-bvcg"},
			Reachability: &manifest.ReachabilityRef{
				Deltas: []manifest.IndexRef{
					{Hash: "d1"}, {Hash: "d2"}, {Hash: "d3"},
				},
			},
		},
	}
	in := compactOnlyInput{
		NewCommitGraph:     manifest.IndexRef{Key: "new-bvcg", Hash: "h-bvcg-new"},
		ConsumedHashes:     map[string]struct{}{"d1": {}, "d2": {}, "d3": {}},
		ConsumedDeltaCount: 3,
		BaseManifest:       "v00000010",
	}
	got := buildCompactOnlyBody(prev, in)

	// ObjectMap must be UNCHANGED — compact-only must not swap in a .bvom
	// that references a locally-built pack that is never uploaded.
	if got.Indexes.ObjectMap != prevObjMap {
		t.Errorf("ObjectMap changed: got %+v, want same pointer as prev (%+v)",
			got.Indexes.ObjectMap, prevObjMap)
	}
	if got.Indexes.ObjectMap == nil || got.Indexes.ObjectMap.Key != "old-bvom" {
		t.Errorf("ObjectMap key = %v, want old-bvom", got.Indexes.ObjectMap)
	}

	// CommitGraph IS updated.
	if got.Indexes.CommitGraph == nil || got.Indexes.CommitGraph.Key != "new-bvcg" {
		t.Errorf("CommitGraph not updated: %+v", got.Indexes.CommitGraph)
	}

	// Packs are unchanged.
	if len(got.Packs) != 1 || got.Packs[0].PackID != "p1" {
		t.Errorf("Packs changed: %+v", got.Packs)
	}

	// All deltas consumed.
	if got.Indexes.Reachability == nil {
		t.Fatal("Reachability is nil")
	}
	if len(got.Indexes.Reachability.Deltas) != 0 {
		t.Errorf("Deltas = %d, want 0 (all consumed)", len(got.Indexes.Reachability.Deltas))
	}
	if got.Indexes.Reachability.BaseManifest != "v00000010" {
		t.Errorf("BaseManifest = %q, want v00000010", got.Indexes.Reachability.BaseManifest)
	}
}

func TestBuildCompactOnlyBody_NilObjectMapPreserved(t *testing.T) {
	prev := manifest.Body{
		Packs: []manifest.PackEntry{{PackKey: "K1"}},
		Indexes: manifest.Indexes{
			ObjectMap: nil, // no .bvom yet
		},
	}
	in := compactOnlyInput{
		NewCommitGraph: manifest.IndexRef{Key: "bvcg-new", Hash: "h"},
	}
	got := buildCompactOnlyBody(prev, in)
	if got.Indexes.ObjectMap != nil {
		t.Errorf("ObjectMap = %+v, want nil (preserved from prev)", got.Indexes.ObjectMap)
	}
}

func TestBuildCompactOnlyBody_ConcurrentPushDeltasPreserved(t *testing.T) {
	prev := manifest.Body{
		Packs: []manifest.PackEntry{{PackKey: "K1"}},
		Indexes: manifest.Indexes{
			Reachability: &manifest.ReachabilityRef{
				Deltas: []manifest.IndexRef{
					{Hash: "d1"}, {Hash: "d2"}, // consumed
					{Hash: "d_concurrent"}, // concurrent push, NOT consumed
				},
			},
		},
	}
	in := compactOnlyInput{
		NewCommitGraph: manifest.IndexRef{Key: "bvcg", Hash: "h"},
		ConsumedHashes: map[string]struct{}{"d1": {}, "d2": {}},
		BaseManifest:   "v5",
	}
	got := buildCompactOnlyBody(prev, in)
	if got.Indexes.Reachability == nil {
		t.Fatal("Reachability is nil")
	}
	if len(got.Indexes.Reachability.Deltas) != 1 || got.Indexes.Reachability.Deltas[0].Hash != "d_concurrent" {
		t.Errorf("Deltas = %+v, want [d_concurrent]", got.Indexes.Reachability.Deltas)
	}
}

func TestBuildMergedBody_ZeroConsumedPreservesAllDeltas(t *testing.T) {
	prev := manifest.Body{
		Packs: []manifest.PackEntry{{PackKey: "K1"}},
		Indexes: manifest.Indexes{
			Reachability: &manifest.ReachabilityRef{
				Deltas: []manifest.IndexRef{{Hash: "d1"}, {Hash: "d2"}},
			},
		},
	}
	in := mergeInput{
		P0Keys:             []string{"K1"},
		NewPack:            manifest.PackEntry{PackKey: "Knew"},
		ConsumedHashes:     map[string]struct{}{},
		ConsumedDeltaCount: 0,
		BaseManifest:       "v1",
	}
	got := buildMergedBody(prev, in)
	if len(got.Indexes.Reachability.Deltas) != 2 {
		t.Errorf("Deltas = %d, want 2 (zero consumed)", len(got.Indexes.Reachability.Deltas))
	}
}
