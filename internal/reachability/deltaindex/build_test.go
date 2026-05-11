package deltaindex

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/pack"
)

func TestEncode_EmptyDelta(t *testing.T) {
	bts, err := Encode(Delta{})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(bts) < HeaderSize+TrailerSize {
		t.Fatalf("too short: %d", len(bts))
	}
	if string(bts[:4]) != "BVRD" {
		t.Fatalf("magic = %q", bts[:4])
	}
	if binary.LittleEndian.Uint32(bts[4:8]) != VersionCurrent {
		t.Fatalf("version mismatch")
	}
	// Trailer = SHA-256 of preceding bytes.
	want := sha256.Sum256(bts[:len(bts)-TrailerSize])
	got := bts[len(bts)-TrailerSize:]
	if !bytes.Equal(want[:], got) {
		t.Fatalf("trailer mismatch")
	}
}

func TestEncode_DeterministicGivenSortedInput(t *testing.T) {
	d := Delta{
		Commits: []CommitRecord{
			{OID: oidFix(0xA1), Generation: 1, Parents: nil},
			{OID: oidFix(0xB1), Generation: 2, Parents: []pack.OID{oidFix(0xA1)}},
		},
		RefTips: []RefTipDiff{{RefName: "refs/heads/main", NewOID: oidFix(0xB1)}},
		Packs:   []pack.OID{oidFix(0xC1)},
	}
	a, err := Encode(d)
	if err != nil {
		t.Fatalf("Encode #1: %v", err)
	}
	b, err := Encode(d)
	if err != nil {
		t.Fatalf("Encode #2: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("Encode is non-deterministic")
	}
}

// TestEncode_PacksOrderIndependent verifies that two Encode calls with Packs
// in different input orders produce identical bytes (Encode sorts Packs internally).
func TestEncode_PacksOrderIndependent(t *testing.T) {
	p1 := oidFix(0x10)
	p2 := oidFix(0x20)
	p3 := oidFix(0x30)

	d1 := Delta{
		Packs: []pack.OID{p3, p1, p2}, // unsorted
	}
	d2 := Delta{
		Packs: []pack.OID{p1, p2, p3}, // sorted
	}

	b1, err := Encode(d1)
	if err != nil {
		t.Fatalf("Encode d1: %v", err)
	}
	b2, err := Encode(d2)
	if err != nil {
		t.Fatalf("Encode d2: %v", err)
	}
	if !bytes.Equal(b1, b2) {
		t.Fatalf("Encode with different Pack ordering produced different bytes: Encode must sort Packs internally")
	}
}

func oidFix(b byte) pack.OID {
	var o pack.OID
	o[0] = b
	return o
}
