package objindex

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/pack"
)

func oidOf(t *testing.T, hex string) pack.OID {
	t.Helper()
	o, err := pack.ParseOID(hex)
	if err != nil {
		t.Fatalf("ParseOID: %v", err)
	}
	return o
}

func TestBuild_HeaderAndMagic(t *testing.T) {
	pid := strings.Repeat("a", 40)
	entries := []Entry{
		{OID: oidOf(t, "0123456789abcdef0123456789abcdef01234567"), PackID: pid, Offset: 12},
		{OID: oidOf(t, "1123456789abcdef0123456789abcdef01234567"), PackID: pid, Offset: 200},
	}
	out, err := build(entries)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if string(out[:4]) != "BVOM" {
		t.Fatalf("magic: got %q", out[:4])
	}
	if v := binary.BigEndian.Uint32(out[4:8]); v != 1 {
		t.Fatalf("version: got %d", v)
	}
	if cnt := binary.BigEndian.Uint64(out[8:16]); cnt != 2 {
		t.Fatalf("count: got %d", cnt)
	}
}

func TestBuild_SortsRecords(t *testing.T) {
	pid := strings.Repeat("a", 40)
	hi := oidOf(t, "ffffffffffffffffffffffffffffffffffffffff")
	lo := oidOf(t, "0000000000000000000000000000000000000001")
	entries := []Entry{
		{OID: hi, PackID: pid, Offset: 1},
		{OID: lo, PackID: pid, Offset: 2},
	}
	out, err := build(entries)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	rec0 := out[recordsStart() : recordsStart()+recordSize]
	if !bytes.Equal(rec0[:20], lo[:]) {
		t.Fatalf("records not sorted")
	}
}

func TestBuild_TrailerHash(t *testing.T) {
	pid := strings.Repeat("a", 40)
	entries := []Entry{
		{OID: oidOf(t, "0000000000000000000000000000000000000001"), PackID: pid, Offset: 12},
	}
	out, err := build(entries)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	pre := out[:len(out)-32]
	want := sha256.Sum256(pre)
	got := out[len(out)-32:]
	if !bytes.Equal(want[:], got) {
		t.Fatalf("trailer hash mismatch")
	}
}

func TestBuild_Determinism(t *testing.T) {
	pid := strings.Repeat("a", 40)
	mk := func() []Entry {
		return []Entry{
			{OID: oidOf(t, "1111111111111111111111111111111111111111"), PackID: pid, Offset: 1},
			{OID: oidOf(t, "0000000000000000000000000000000000000001"), PackID: pid, Offset: 2},
		}
	}
	out1, err := build(mk())
	if err != nil {
		t.Fatalf("build1: %v", err)
	}
	out2, err := build(mk())
	if err != nil {
		t.Fatalf("build2: %v", err)
	}
	if !bytes.Equal(out1, out2) {
		t.Fatalf("non-deterministic build output")
	}
}

func TestBuild_RejectsDuplicateOID(t *testing.T) {
	pid := strings.Repeat("a", 40)
	dup := oidOf(t, "0000000000000000000000000000000000000001")
	entries := []Entry{
		{OID: dup, PackID: pid, Offset: 12},
		{OID: dup, PackID: pid, Offset: 34},
	}
	if _, err := build(entries); err == nil {
		t.Fatalf("expected duplicate-OID error")
	}
}

func TestBuild_RejectsBadPackIDLength(t *testing.T) {
	entries := []Entry{
		{OID: oidOf(t, "0000000000000000000000000000000000000001"), PackID: "short", Offset: 12},
	}
	if _, err := build(entries); err == nil {
		t.Fatalf("expected bad pack_id length error")
	}
}

func TestBuild_FromPackReader(t *testing.T) {
	// Use the same makeOnePackRepo as internal/pack but indirectly:
	// build a pack reader from a fixture, then call objindex.Build.
	// We can't import internal/pack tests directly; spin up a tiny inline
	// pack-creation here via gitcli.
	t.Skip("integration test; covered by pack reader tests once Build(packReader) is exercised by the importer")
}
