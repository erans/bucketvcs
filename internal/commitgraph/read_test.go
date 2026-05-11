package commitgraph

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/pack"
)

func TestRead_V2_RoundtripsGeneration(t *testing.T) {
	commits := []Record{
		{OID: oidA, Generation: 1, Parents: nil},
		{OID: oidB, Generation: 2, Parents: []pack.OID{oidA}},
	}
	tips := []Tip{{Ref: "refs/heads/main", OID: oidB}}
	bts, err := build(commits, tips)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	rdr, err := Open(bts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	g, ok := rdr.GenerationOf(oidA)
	if !ok || g != 1 {
		t.Errorf("GenerationOf(A) = (%d, %v), want (1, true)", g, ok)
	}
	g, ok = rdr.GenerationOf(oidB)
	if !ok || g != 2 {
		t.Errorf("GenerationOf(B) = (%d, %v), want (2, true)", g, ok)
	}
}

func TestRead_V1_GenerationIsZero(t *testing.T) {
	bts := makeV1FixtureSingleCommit(t, oidA)
	rdr, err := Open(bts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	g, ok := rdr.GenerationOf(oidA)
	if !ok {
		t.Fatalf("commit A should be present in v1 fixture")
	}
	if g != 0 {
		t.Fatalf("v1 GenerationOf(A) = %d, want 0", g)
	}
}

func TestReader_GenerationOf_NotFound(t *testing.T) {
	commits := []Record{{OID: oidA, Generation: 7, Parents: nil}}
	tips := []Tip{{Ref: "refs/heads/main", OID: oidA}}
	bts, _ := build(commits, tips)
	rdr, _ := Open(bts)
	if _, ok := rdr.GenerationOf(oidB); ok {
		t.Fatalf("GenerationOf(B) ok=true, want false")
	}
}

func TestReader_RecordOf(t *testing.T) {
	commits := []Record{
		{OID: oidA, Generation: 1, Parents: nil},
		{OID: oidB, Generation: 2, Parents: []pack.OID{oidA}},
	}
	tips := []Tip{{Ref: "refs/heads/main", OID: oidB}}
	bts, _ := build(commits, tips)
	rdr, _ := Open(bts)
	rec, ok := rdr.RecordOf(oidB)
	if !ok {
		t.Fatalf("RecordOf(B) not found")
	}
	if rec.Generation != 2 {
		t.Fatalf("RecordOf(B).Generation = %d, want 2", rec.Generation)
	}
	if len(rec.Parents) != 1 || rec.Parents[0] != oidA {
		t.Fatalf("RecordOf(B).Parents = %v, want [A]", rec.Parents)
	}
}

// TestOpen_RejectsDanglingParent_BytesPath verifies that the bytes-based Open
// (used by the reachability path) also rejects dangling parent OIDs.
// OpenFromStore's dangling-parent check is tested in format_test.go;
// this test covers the previously-unvalidated Open path.
func TestOpen_RejectsDanglingParent_BytesPath(t *testing.T) {
	// Build a valid two-commit graph A→B, then corrupt the parent field of B
	// to point at an unknown OID (oidC). The trailer must be recomputed to
	// pass the hash check — this tests that parseRecordBytes-level validation
	// (not just the trailer) catches the dangling reference.
	oidC := pack.OID{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 3}
	commits := []Record{
		{OID: oidA, Generation: 1, Parents: nil},
		{OID: oidB, Generation: 2, Parents: []pack.OID{oidA}},
	}
	tips := []Tip{{Ref: "refs/heads/main", OID: oidB}}
	bts, err := build(commits, tips)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// Locate oidA's bytes within the body and replace them with oidC so that
	// oidB now references a parent that does not exist. We search for oidA
	// starting after the header+tips section (which begins after headerSize bytes).
	// oidB's record comes after oidA's record in the sorted body.
	// v2 record layout: oid(20) + gen(4) + n_parents(1) + parents[n]*20.
	// oidB's record: oidB(20) + gen(4) + n_parents(1) + parent=oidA(20).
	// We want to corrupt the parent field only (the second occurrence of oidA).
	modified := make([]byte, len(bts)-trailerSize)
	copy(modified, bts[:len(bts)-trailerSize])

	found := 0
	for i := headerSize; i <= len(modified)-20; i++ {
		if [20]byte(modified[i:i+20]) == oidA {
			found++
			if found == 2 {
				// Second occurrence is the parent reference in oidB's record.
				copy(modified[i:i+20], oidC[:])
				break
			}
		}
	}
	if found < 2 {
		t.Fatalf("did not find two occurrences of oidA in fixture (found %d); fixture layout may have changed", found)
	}

	// Recompute trailer so the hash check passes — we want to test the
	// parent-validation logic, not just the hash check.
	sum := sha256.Sum256(modified)
	corrupted := append(modified, sum[:]...)

	_, err = Open(corrupted)
	if err == nil {
		t.Fatal("Open: want error for dangling parent, got nil")
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("Open: want ErrCorrupt, got %v", err)
	}
}

func makeV1FixtureSingleCommit(t *testing.T, oid pack.OID) []byte {
	t.Helper()
	var buf []byte
	// header: "BVCG" + version=1 + n_commits=1 + n_tips=0 + reserved 12B
	buf = append(buf, []byte("BVCG")...)
	u4 := make([]byte, 4)
	binary.BigEndian.PutUint32(u4, 1)
	buf = append(buf, u4...)
	u8 := make([]byte, 8)
	binary.BigEndian.PutUint64(u8, 1)
	buf = append(buf, u8...)
	binary.BigEndian.PutUint32(u4, 0)
	buf = append(buf, u4...)
	buf = append(buf, make([]byte, 12)...)
	// commit record (v1): oid(20) + n_parents(1) + 0 parents
	buf = append(buf, oid[:]...)
	buf = append(buf, byte(0))
	// trailer: SHA-256
	sum := sha256.Sum256(buf)
	buf = append(buf, sum[:]...)
	return buf
}
