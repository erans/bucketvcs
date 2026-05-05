package objindex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
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
	// Skip if git is unavailable.
	if _, err := gitcli.Version(context.Background()); err != nil {
		t.Skip("git not available:", err)
	}
	work := t.TempDir()
	mustGit := func(args ...string) {
		t.Helper()
		out, err := gitcli.RunForTest(work, args...)
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	mustGit("init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(work, "f"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustGit("add", "f")
	mustGit("-c", "user.name=t", "-c", "user.email=t@e", "commit", "-m", "init")
	bare := filepath.Join(t.TempDir(), "bare")
	if err := gitcli.CloneBareMirror(context.Background(), work, bare); err != nil {
		t.Fatalf("CloneBareMirror: %v", err)
	}
	out := t.TempDir()
	prefix := filepath.Join(out, "pack")
	id, err := gitcli.PackObjectsAll(context.Background(), bare, prefix)
	if err != nil {
		t.Fatalf("PackObjectsAll: %v", err)
	}
	store := newTestStore(t)
	uploadFile := func(srcPath, dstKey string) {
		t.Helper()
		f, err := os.Open(srcPath)
		if err != nil {
			t.Fatalf("Open %s: %v", srcPath, err)
		}
		defer f.Close()
		if _, err := store.PutIfAbsent(context.Background(), dstKey, f, nil); err != nil {
			t.Fatalf("PutIfAbsent %s: %v", dstKey, err)
		}
	}
	uploadFile(prefix+"-"+id+".pack", "p.pack")
	uploadFile(prefix+"-"+id+".idx", "p.idx")
	r, err := pack.Open(context.Background(), store, "p.pack", "p.idx")
	if err != nil {
		t.Fatalf("pack.Open: %v", err)
	}
	defer r.Close()
	bvom, err := Build(r, id)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Open the result and verify each idx OID is found.
	if _, err := store.PutIfAbsent(context.Background(), "k.bvom", bytes.NewReader(bvom), nil); err != nil {
		t.Fatalf("Put bvom: %v", err)
	}
	m, err := Open(context.Background(), store, "k.bvom")
	if err != nil {
		t.Fatalf("Open bvom: %v", err)
	}
	for i := 0; i < r.Idx().Count(); i++ {
		o := r.Idx().OIDAt(i)
		_, _, ok := m.Lookup(o)
		if !ok {
			t.Fatalf("Lookup miss for %s", o)
		}
	}
}

func newTestStore(t *testing.T) storage.ObjectStore {
	t.Helper()
	s, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenAndLookup_RoundTrip(t *testing.T) {
	a := oidOf(t, "0000000000000000000000000000000000000001")
	b := oidOf(t, "1000000000000000000000000000000000000001")
	c := oidOf(t, "ff00000000000000000000000000000000000001")
	pid := strings.Repeat("a", 40)
	entries := []Entry{
		{OID: a, PackID: pid, Offset: 12},
		{OID: b, PackID: pid, Offset: 5000},
		{OID: c, PackID: pid, Offset: 90000},
	}
	out, err := build(entries)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "k.bvom", strings.NewReader(string(out)), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	m, err := Open(context.Background(), store, "k.bvom")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, e := range entries {
		got, off, ok := m.Lookup(e.OID)
		if !ok {
			t.Fatalf("Lookup(%s) miss", e.OID)
		}
		if got != e.PackID {
			t.Fatalf("Lookup pack: got %s, want %s", got, e.PackID)
		}
		if off != e.Offset {
			t.Fatalf("Lookup offset: got %d, want %d", off, e.Offset)
		}
	}
	bogus := oidOf(t, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	if _, _, ok := m.Lookup(bogus); ok {
		t.Fatalf("expected miss for bogus OID")
	}
}

func TestOpen_RejectsBadMagic(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "k", strings.NewReader("XXXXgarbage"), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := Open(context.Background(), store, "k"); err == nil {
		t.Fatalf("expected error on bad magic")
	}
}

func TestOpen_RejectsBadTrailer(t *testing.T) {
	a := oidOf(t, "0000000000000000000000000000000000000001")
	pid := strings.Repeat("a", 40)
	out, err := build([]Entry{{OID: a, PackID: pid, Offset: 1}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// Flip a byte in the data section, leave trailer.
	out[headerSize] ^= 0xff
	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "k", strings.NewReader(string(out)), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := Open(context.Background(), store, "k"); err == nil {
		t.Fatalf("expected trailer mismatch error")
	}
}

func TestOpen_RejectsTooSmall(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "k", strings.NewReader("x"), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := Open(context.Background(), store, "k"); err == nil {
		t.Fatalf("expected file-too-small error")
	}
}

func TestOpen_RejectsBadVersion(t *testing.T) {
	a := oidOf(t, "0000000000000000000000000000000000000001")
	pid := strings.Repeat("a", 40)
	out, err := build([]Entry{{OID: a, PackID: pid, Offset: 1}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// Bump version field to 99.
	out[7] = 99
	// Recompute trailer so it isn't the trailer check that fires.
	pre := out[:len(out)-trailerSize]
	want := sha256.Sum256(pre)
	copy(out[len(out)-trailerSize:], want[:])
	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "k", strings.NewReader(string(out)), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := Open(context.Background(), store, "k"); err == nil {
		t.Fatalf("expected version mismatch error")
	}
}

func TestOpen_RejectsCountTooLarge(t *testing.T) {
	// Build a valid 1-record idx, then rewrite the count field to a huge value.
	a := oidOf(t, "0000000000000000000000000000000000000001")
	pid := strings.Repeat("a", 40)
	out, err := build([]Entry{{OID: a, PackID: pid, Offset: 1}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// count is at bytes [8:16].
	binary.BigEndian.PutUint64(out[8:16], 1<<40)
	// Re-trailer to bypass trailer check.
	pre := out[:len(out)-trailerSize]
	want := sha256.Sum256(pre)
	copy(out[len(out)-trailerSize:], want[:])
	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "k", strings.NewReader(string(out)), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := Open(context.Background(), store, "k"); err == nil {
		t.Fatalf("expected count-too-large rejection")
	}
}

func TestOpen_RejectsSurplusBytesBeforeTrailer(t *testing.T) {
	a := oidOf(t, "0000000000000000000000000000000000000001")
	pid := strings.Repeat("a", 40)
	out, err := build([]Entry{{OID: a, PackID: pid, Offset: 1}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// Insert 8 surplus bytes between pack-table body and trailer.
	trailerStart := len(out) - trailerSize
	tampered := append([]byte(nil), out[:trailerStart]...)
	tampered = append(tampered, make([]byte, 8)...)
	tampered = append(tampered, out[trailerStart:]...)
	// Re-trailer.
	pre := tampered[:len(tampered)-trailerSize]
	want := sha256.Sum256(pre)
	copy(tampered[len(tampered)-trailerSize:], want[:])
	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "k", strings.NewReader(string(tampered)), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := Open(context.Background(), store, "k"); err == nil {
		t.Fatalf("expected surplus-bytes rejection")
	}
}

func TestOpen_RejectsRecordPackIdxOutOfRange(t *testing.T) {
	// Build a single-record idx, then mutate the record's pack_idx field
	// to a value beyond nPacks=1. Re-trailer to bypass the trailer check.
	a := oidOf(t, "0000000000000000000000000000000000000001")
	pid := strings.Repeat("a", 40)
	out, err := build([]Entry{{OID: a, PackID: pid, Offset: 1}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// pack_idx field is at record_offset + 20 (2 bytes BE).
	binary.BigEndian.PutUint16(out[headerSize+20:headerSize+22], 99)
	pre := out[:len(out)-trailerSize]
	want := sha256.Sum256(pre)
	copy(out[len(out)-trailerSize:], want[:])
	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "k", strings.NewReader(string(out)), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := Open(context.Background(), store, "k"); err == nil {
		t.Fatalf("expected pack_idx-out-of-range rejection")
	}
}

func TestBuild_RejectsNonHexPackID(t *testing.T) {
	bad := strings.Repeat("Z", 40) // not hex
	entries := []Entry{
		{OID: oidOf(t, "0000000000000000000000000000000000000001"), PackID: bad, Offset: 1},
	}
	if _, err := build(entries); err == nil {
		t.Fatalf("expected non-hex pack_id rejection")
	}
}

func TestBuild_RejectsUppercasePackID(t *testing.T) {
	bad := strings.Repeat("A", 40) // valid hex but uppercase
	entries := []Entry{
		{OID: oidOf(t, "0000000000000000000000000000000000000001"), PackID: bad, Offset: 1},
	}
	if _, err := build(entries); err == nil {
		t.Fatalf("expected uppercase pack_id rejection (M2 requires lowercase)")
	}
}

func TestOpen_RejectsPackTblOffPastFileEnd(t *testing.T) {
	// Build a valid 1-record idx, then tamper packTblOff to a large value.
	// The existing pack_tbl offset-mismatch check fires, confirming the
	// validation path that guards the slice is in place.
	a := oidOf(t, "0000000000000000000000000000000000000001")
	pid := strings.Repeat("a", 40)
	out, err := build([]Entry{{OID: a, PackID: pid, Offset: 1}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	binary.BigEndian.PutUint64(out[16:24], 999) // wrong packTblOff
	pre := out[:len(out)-trailerSize]
	want := sha256.Sum256(pre)
	copy(out[len(out)-trailerSize:], want[:])
	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "k", strings.NewReader(string(out)), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := Open(context.Background(), store, "k"); err == nil {
		t.Fatalf("expected packTblOff mismatch rejection")
	}
}

func TestBuild_MultiPack_DeterministicAcrossInputOrder(t *testing.T) {
	pidA := strings.Repeat("a", 40)
	pidB := strings.Repeat("b", 40)
	pidC := strings.Repeat("c", 40)
	a := oidOf(t, "0000000000000000000000000000000000000001")
	b := oidOf(t, "0000000000000000000000000000000000000002")
	c := oidOf(t, "0000000000000000000000000000000000000003")
	mk := func(order []Entry) []byte {
		out, err := build(order)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		return out
	}
	out1 := mk([]Entry{
		{OID: a, PackID: pidA, Offset: 1},
		{OID: b, PackID: pidB, Offset: 2},
		{OID: c, PackID: pidC, Offset: 3},
	})
	out2 := mk([]Entry{
		{OID: c, PackID: pidC, Offset: 3},
		{OID: a, PackID: pidA, Offset: 1},
		{OID: b, PackID: pidB, Offset: 2},
	})
	out3 := mk([]Entry{
		{OID: b, PackID: pidB, Offset: 2},
		{OID: c, PackID: pidC, Offset: 3},
		{OID: a, PackID: pidA, Offset: 1},
	})
	if !bytes.Equal(out1, out2) || !bytes.Equal(out2, out3) {
		t.Fatalf("multi-pack build is not deterministic across input orderings")
	}
}

func TestOpenWithExpectedHash_RejectsMismatch(t *testing.T) {
	a := oidOf(t, "0000000000000000000000000000000000000001")
	pid := strings.Repeat("a", 40)
	out, err := build([]Entry{{OID: a, PackID: pid, Offset: 1}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "k", strings.NewReader(string(out)), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	bogus := strings.Repeat("e", 64)
	if _, err := OpenWithExpectedHash(context.Background(), store, "k", bogus); err == nil {
		t.Fatalf("expected hash mismatch error")
	}
}

func TestOpenWithExpectedHash_AcceptsMatch(t *testing.T) {
	a := oidOf(t, "0000000000000000000000000000000000000001")
	pid := strings.Repeat("a", 40)
	out, err := build([]Entry{{OID: a, PackID: pid, Offset: 1}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "k", strings.NewReader(string(out)), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	sum := sha256.Sum256(out)
	hexHash := fmt.Sprintf("%x", sum)
	if _, err := OpenWithExpectedHash(context.Background(), store, "k", hexHash); err != nil {
		t.Fatalf("OpenWithExpectedHash: %v", err)
	}
}
