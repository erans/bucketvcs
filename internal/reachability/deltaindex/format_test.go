package deltaindex

import (
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/pack"
)

func TestFormat_Magic(t *testing.T) {
	if string(Magic[:]) != "BVRD" {
		t.Fatalf("Magic = %q, want BVRD", Magic[:])
	}
}

func TestFormat_VersionCurrent(t *testing.T) {
	if VersionCurrent != 1 {
		t.Fatalf("VersionCurrent = %d, want 1", VersionCurrent)
	}
}

func TestFormat_HeaderSize(t *testing.T) {
	if HeaderSize != 32 {
		t.Fatalf("HeaderSize = %d, want 32", HeaderSize)
	}
}

func TestFormat_TrailerSize(t *testing.T) {
	if TrailerSize != 32 {
		t.Fatalf("TrailerSize = %d, want 32", TrailerSize)
	}
}

func TestParseHeader_RoundTrip(t *testing.T) {
	// Encode a Delta with known counts, then parse the header from the output.
	d := Delta{
		Commits: []CommitRecord{
			{OID: pack.OID{1}, Generation: 1},
			{OID: pack.OID{2}, Generation: 2},
			{OID: pack.OID{3}, Generation: 3},
		},
		RefTips: []RefTipDiff{
			{RefName: "refs/heads/main", NewOID: pack.OID{1}},
		},
		Packs: []pack.OID{{4}},
	}
	b, err := Encode(d)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	h, err := ParseHeader(b[:HeaderSize])
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if h.NCommits != 3 {
		t.Errorf("NCommits = %d, want 3", h.NCommits)
	}
	if h.NReftips != 1 {
		t.Errorf("NReftips = %d, want 1", h.NReftips)
	}
	if h.NPacks != 1 {
		t.Errorf("NPacks = %d, want 1", h.NPacks)
	}
	if h.Version != VersionCurrent {
		t.Errorf("Version = %d, want %d", h.Version, VersionCurrent)
	}
}

func TestParseHeader_TooShort(t *testing.T) {
	_, err := ParseHeader([]byte{1, 2, 3})
	if err == nil {
		t.Fatal("expected error for too-short header")
	}
}

func TestParseHeader_BadMagic(t *testing.T) {
	buf := make([]byte, HeaderSize)
	buf[0] = 'X'
	buf[1] = 'X'
	buf[2] = 'X'
	buf[3] = 'X'
	_, err := ParseHeader(buf)
	if err == nil {
		t.Fatal("expected error for bad magic")
	}
}
