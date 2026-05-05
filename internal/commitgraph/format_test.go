package commitgraph

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/pack"
)

func oid(t *testing.T, hex string) pack.OID {
	t.Helper()
	o, err := pack.ParseOID(hex)
	if err != nil {
		t.Fatalf("ParseOID: %v", err)
	}
	return o
}

func TestBuild_HeaderAndTrailer(t *testing.T) {
	a := oid(t, "0000000000000000000000000000000000000001")
	b := oid(t, "0000000000000000000000000000000000000002")
	commits := []Record{
		{OID: a, Parents: nil},
		{OID: b, Parents: []pack.OID{a}},
	}
	tips := []Tip{{Ref: "refs/heads/main", OID: b}}
	out, err := build(commits, tips)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if string(out[:4]) != "BVCG" {
		t.Fatalf("magic: %q", out[:4])
	}
	if v := binary.BigEndian.Uint32(out[4:8]); v != 1 {
		t.Fatalf("version: %d", v)
	}
	if cnt := binary.BigEndian.Uint64(out[8:16]); cnt != 2 {
		t.Fatalf("n_commits: %d", cnt)
	}
	if nt := binary.BigEndian.Uint32(out[16:20]); nt != 1 {
		t.Fatalf("n_tips: %d", nt)
	}
	pre := out[:len(out)-trailerSize]
	want := sha256.Sum256(pre)
	if !bytes.Equal(want[:], out[len(out)-trailerSize:]) {
		t.Fatalf("trailer mismatch")
	}
}

func TestBuild_DeterministicSortOrder(t *testing.T) {
	a := oid(t, "0000000000000000000000000000000000000003")
	b := oid(t, "0000000000000000000000000000000000000001")
	c := oid(t, "0000000000000000000000000000000000000002")
	out1, err := build([]Record{{OID: a}, {OID: b}, {OID: c}}, nil)
	if err != nil {
		t.Fatalf("build1: %v", err)
	}
	out2, err := build([]Record{{OID: c}, {OID: a}, {OID: b}}, nil)
	if err != nil {
		t.Fatalf("build2: %v", err)
	}
	if !bytes.Equal(out1, out2) {
		t.Fatalf("non-deterministic build")
	}
}

func TestBuild_RejectsDuplicateCommits(t *testing.T) {
	dup := oid(t, "0000000000000000000000000000000000000001")
	commits := []Record{
		{OID: dup, Parents: nil},
		{OID: dup, Parents: nil},
	}
	if _, err := build(commits, nil); err == nil {
		t.Fatalf("expected duplicate-commit rejection")
	}
}

func TestBuild_RejectsTooManyParents(t *testing.T) {
	main := oid(t, "0000000000000000000000000000000000000001")
	parent := oid(t, "0000000000000000000000000000000000000002")
	parents := make([]pack.OID, 256)
	for i := range parents {
		parents[i] = parent
	}
	commits := []Record{{OID: main, Parents: parents}}
	if _, err := build(commits, nil); err == nil {
		t.Fatalf("expected too-many-parents rejection")
	}
}

func TestParseParents_TreeAndParentLines(t *testing.T) {
	// Realistic commit body excerpt.
	body := []byte("tree aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\nparent bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\nparent cccccccccccccccccccccccccccccccccccccccc\nauthor t <t@e> 1234 +0000\ncommitter t <t@e> 1234 +0000\n\nmessage\n")
	parents, err := parseParents(body)
	if err != nil {
		t.Fatalf("parseParents: %v", err)
	}
	if len(parents) != 2 {
		t.Fatalf("parents count: got %d, want 2", len(parents))
	}
}

func TestParseParents_NoParents(t *testing.T) {
	body := []byte("tree aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\nauthor t <t@e> 1234 +0000\n\nmessage\n")
	parents, err := parseParents(body)
	if err != nil {
		t.Fatalf("parseParents: %v", err)
	}
	if len(parents) != 0 {
		t.Fatalf("expected 0 parents, got %d", len(parents))
	}
}
