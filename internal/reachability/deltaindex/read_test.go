package deltaindex

import (
	"bytes"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/pack"
)

func TestDecode_Roundtrip(t *testing.T) {
	d := Delta{
		Commits: []CommitRecord{
			{OID: oidFix(0xA1), Generation: 1, Parents: nil},
			{OID: oidFix(0xB1), Generation: 2, Parents: []pack.OID{oidFix(0xA1)}},
		},
		RefTips: []RefTipDiff{
			{RefName: "refs/heads/main", NewOID: oidFix(0xB1)},
			{RefName: "refs/tags/v1", NewOID: oidFix(0xA1)},
		},
		Packs: []pack.OID{oidFix(0xC1)},
	}
	bts, err := Encode(d)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := Decode(bts)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(got.Commits) != 2 {
		t.Fatalf("commits len=%d", len(got.Commits))
	}
	// After sort, oidFix(0xA1) comes first
	if got.Commits[1].Generation != 2 {
		t.Fatalf("gen lost: %d", got.Commits[1].Generation)
	}
	wantParent := oidFix(0xA1)
	if !bytes.Equal(got.Commits[1].Parents[0][:], wantParent[:]) {
		t.Fatalf("parents lost")
	}
	if len(got.RefTips) != 2 || got.RefTips[0].RefName != "refs/heads/main" {
		t.Fatalf("reftips lost: %+v", got.RefTips)
	}
	if len(got.Packs) != 1 || got.Packs[0] != oidFix(0xC1) {
		t.Fatalf("packs lost")
	}
}

func TestDecode_ContentHashStability(t *testing.T) {
	d := Delta{Commits: []CommitRecord{{OID: oidFix(0xA1), Generation: 1}}}
	a, _ := Encode(d)
	b, _ := Encode(d)
	if !bytes.Equal(a, b) {
		t.Fatalf("encode not stable")
	}
}
