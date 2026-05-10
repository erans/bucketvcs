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
