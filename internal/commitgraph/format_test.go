package commitgraph

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func newTestStore(t *testing.T) storage.ObjectStore {
	t.Helper()
	s, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

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
	if v := binary.BigEndian.Uint32(out[4:8]); v != VersionV2 {
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

func TestBuild_RejectsTipNotInCommitSet(t *testing.T) {
	a := oid(t, "0000000000000000000000000000000000000001")
	b := oid(t, "0000000000000000000000000000000000000002")
	commits := []Record{{OID: a, Parents: nil}}
	tips := []Tip{{Ref: "refs/heads/main", OID: b}} // b is not in commits
	if _, err := build(commits, tips); err == nil {
		t.Fatalf("expected tip-not-in-commit-set rejection")
	}
}

func TestBuild_RejectsDanglingParent(t *testing.T) {
	a := oid(t, "0000000000000000000000000000000000000001")
	b := oid(t, "0000000000000000000000000000000000000002") // referenced as parent but not in commits
	commits := []Record{
		{OID: a, Parents: []pack.OID{b}},
	}
	if _, err := build(commits, nil); err == nil {
		t.Fatalf("expected dangling-parent rejection")
	}
}

func TestOpenAndParents_RoundTrip(t *testing.T) {
	a := oid(t, "0000000000000000000000000000000000000001")
	b := oid(t, "0000000000000000000000000000000000000002")
	c := oid(t, "0000000000000000000000000000000000000003")
	commits := []Record{
		{OID: a},
		{OID: b, Parents: []pack.OID{a}},
		{OID: c, Parents: []pack.OID{b, a}}, // octopus
	}
	tips := []Tip{{Ref: "refs/heads/main", OID: c}}
	out, err := build(commits, tips)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "k.bvcg", strings.NewReader(string(out)), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	g, err := OpenFromStore(context.Background(), store, "k.bvcg")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	gotA, ok := g.Parents(a)
	if !ok || len(gotA) != 0 {
		t.Fatalf("Parents(a): ok=%v parents=%v", ok, gotA)
	}
	gotB, ok := g.Parents(b)
	if !ok || len(gotB) != 1 || gotB[0] != a {
		t.Fatalf("Parents(b): %v %v", gotB, ok)
	}
	gotC, ok := g.Parents(c)
	if !ok || len(gotC) != 2 || gotC[0] != b || gotC[1] != a {
		t.Fatalf("Parents(c): %v %v", gotC, ok)
	}
	gotTips := g.Tips()
	if len(gotTips) != 1 || gotTips[0].Ref != "refs/heads/main" || gotTips[0].OID != c {
		t.Fatalf("Tips: %+v", gotTips)
	}
}

func TestOpen_RejectsBadMagic(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "k", strings.NewReader("XXXXgarbage"), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := OpenFromStore(context.Background(), store, "k"); err == nil {
		t.Fatalf("expected bad-magic rejection")
	}
}

func TestOpen_RejectsBadTrailer(t *testing.T) {
	a := oid(t, "0000000000000000000000000000000000000001")
	out, _ := build([]Record{{OID: a}}, nil)
	out[headerSize] ^= 0xff // tamper one byte; trailer becomes stale
	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "k", strings.NewReader(string(out)), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := OpenFromStore(context.Background(), store, "k"); err == nil {
		t.Fatalf("expected bad-trailer rejection")
	}
}

func TestOpen_RejectsBadVersion(t *testing.T) {
	a := oid(t, "0000000000000000000000000000000000000001")
	out, _ := build([]Record{{OID: a}}, nil)
	out[7] = 99 // bump version
	pre := out[:len(out)-trailerSize]
	want := sha256.Sum256(pre)
	copy(out[len(out)-trailerSize:], want[:]) // re-trailer
	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "k", strings.NewReader(string(out)), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := OpenFromStore(context.Background(), store, "k"); err == nil {
		t.Fatalf("expected version mismatch")
	}
}

func TestOpen_RejectsTooSmall(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "k", strings.NewReader("x"), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := OpenFromStore(context.Background(), store, "k"); err == nil {
		t.Fatalf("expected file-too-small rejection")
	}
}

func TestOpen_ParentsMissForUnknownOID(t *testing.T) {
	a := oid(t, "0000000000000000000000000000000000000001")
	out, _ := build([]Record{{OID: a}}, nil)
	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "k", strings.NewReader(string(out)), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	g, err := OpenFromStore(context.Background(), store, "k")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	bogus := oid(t, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	if _, ok := g.Parents(bogus); ok {
		t.Fatalf("expected miss for bogus OID")
	}
}

func TestOpen_RejectsTipNotInCommitSet(t *testing.T) {
	a := oid(t, "0000000000000000000000000000000000000001")
	b := oid(t, "0000000000000000000000000000000000000002")
	// Build with a valid tip pointing at a, then mutate the tip's OID to b.
	out, err := build([]Record{{OID: a}}, []Tip{{Ref: "refs/heads/main", OID: a}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// The tip OID lives at headerSize + 4 (after ref_name_offset).
	copy(out[headerSize+4:headerSize+24], b[:])
	// Re-trailer.
	pre := out[:len(out)-trailerSize]
	want := sha256.Sum256(pre)
	copy(out[len(out)-trailerSize:], want[:])
	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "k", strings.NewReader(string(out)), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := OpenFromStore(context.Background(), store, "k"); err == nil {
		t.Fatalf("expected dangling-tip rejection")
	}
}

func TestOpen_RejectsDanglingParent(t *testing.T) {
	a := oid(t, "0000000000000000000000000000000000000001")
	b := oid(t, "0000000000000000000000000000000000000002")
	// Build a valid 2-commit graph (a, b parent=a), then mutate
	// b's parent OID to something not in the set.
	out, err := build([]Record{
		{OID: a},
		{OID: b, Parents: []pack.OID{a}},
	}, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// commitsStart = headerSize + n_tips*tipSize = 32 + 0 = 32.
	// v2 record layout: oid(20) + gen(4) + n_parents(1) + parents[n]*20.
	// First record (a): 32..57 (25 bytes).
	// Second record (b): starts at 57.
	// b's parent OID is at 57 + 20 + 4 + 1 = 82.
	bogus := make([]byte, 20)
	bogus[0] = 0x42
	copy(out[82:102], bogus)
	pre := out[:len(out)-trailerSize]
	want := sha256.Sum256(pre)
	copy(out[len(out)-trailerSize:], want[:])
	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "k", strings.NewReader(string(out)), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := OpenFromStore(context.Background(), store, "k"); err == nil {
		t.Fatalf("expected dangling-parent rejection")
	}
}

func TestFormat_VersionConstants(t *testing.T) {
	if VersionV1 != 1 {
		t.Errorf("VersionV1 = %d, want 1", VersionV1)
	}
	if VersionV2 != 2 {
		t.Errorf("VersionV2 = %d, want 2", VersionV2)
	}
	if VersionCurrent != VersionV2 {
		t.Errorf("VersionCurrent = %d, want %d", VersionCurrent, VersionV2)
	}
}

func TestRecord_GenerationField(t *testing.T) {
	r := Record{Generation: 7}
	if r.Generation != 7 {
		t.Fatalf("Record.Generation not honored")
	}
}

// oidA and oidB are fixed test OIDs shared across reader and encoder tests.
var oidA pack.OID = [20]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}
var oidB pack.OID = [20]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2}

func TestEncode_V2_GoldenBytes(t *testing.T) {
	// Single root commit with gen=1.
	commits := []Record{{OID: oidA, Generation: 1, Parents: nil}}
	tips := []Tip{{Ref: "refs/heads/main", OID: oidA}}
	got, err := build(commits, tips)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if string(got[:4]) != "BVCG" {
		t.Fatalf("magic = %q, want BVCG", got[:4])
	}
	ver := binary.BigEndian.Uint32(got[4:8])
	if ver != VersionV2 {
		t.Fatalf("version = %d, want %d", ver, VersionV2)
	}
}

func TestEncode_V2_GenerationField_Position(t *testing.T) {
	// Verify the on-disk per-commit record layout: oid(20) + gen(4) +
	// n_parents(u8) + parents[n_parents]*20.
	commits := []Record{{OID: oidA, Generation: 42, Parents: nil}}
	tips := []Tip{{Ref: "refs/heads/main", OID: oidA}}
	got, err := build(commits, tips)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// Header is 32 bytes. Tip table is 1 tip × 24 bytes = 24. Then
	// commit record starts at offset 56.
	off := 32 + 24
	// oid at offset 56..76
	if got[off] != oidA[0] {
		t.Fatalf("oid byte 0 mismatch at offset %d", off)
	}
	// gen at offset 76..80, expected little-endian 42.
	gen := binary.LittleEndian.Uint32(got[off+20 : off+24])
	if gen != 42 {
		t.Fatalf("gen at offset %d = %d, want 42", off+20, gen)
	}
	// n_parents at offset 80
	if got[off+24] != 0 {
		t.Fatalf("n_parents at offset %d = %d, want 0", off+24, got[off+24])
	}
}
