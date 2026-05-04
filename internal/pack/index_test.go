package pack

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := gitcli.Version(context.Background()); err != nil {
		t.Skip("git not available:", err)
	}
}

// makeOnePackRepo authors a small repo and produces a single pack via
// gitcli.PackObjectsAll. Returns the prefix passed to PackObjectsAll,
// the pack_id, and the bare repo path used to build it.
func makeOnePackRepo(t *testing.T) (prefix, packID, bareDir string) {
	t.Helper()
	skipIfNoGit(t)
	work := t.TempDir()
	mustGit := func(args ...string) {
		t.Helper()
		out, err := gitcli.RunForTest(work, args...)
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	mustGit("init", "--initial-branch=main")
	for _, msg := range []string{"a\n", "b\n", "c\n"} {
		if err := os.WriteFile(filepath.Join(work, "f"), []byte(msg), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		mustGit("add", "f")
		mustGit("-c", "user.name=t", "-c", "user.email=t@e",
			"commit", "-m", msg)
	}
	bareDir = filepath.Join(t.TempDir(), "bare")
	if err := gitcli.CloneBareMirror(context.Background(), work, bareDir); err != nil {
		t.Fatalf("CloneBareMirror: %v", err)
	}
	out := t.TempDir()
	prefix = filepath.Join(out, "pack")
	id, err := gitcli.PackObjectsAll(context.Background(), bareDir, prefix)
	if err != nil {
		t.Fatalf("PackObjectsAll: %v", err)
	}
	return prefix, id, bareDir
}

func TestParseIdx_RoundTripFanoutAndCount(t *testing.T) {
	prefix, id, _ := makeOnePackRepo(t)
	idxBytes, err := os.ReadFile(prefix + "-" + id + ".idx")
	if err != nil {
		t.Fatalf("ReadFile idx: %v", err)
	}
	idx, err := ParseIdx(bytes.NewReader(idxBytes), int64(len(idxBytes)))
	if err != nil {
		t.Fatalf("ParseIdx: %v", err)
	}
	if idx.Count() == 0 {
		t.Fatalf("expected non-zero object count")
	}
	// Fanout invariant: fanout[255] == count.
	if idx.Fanout()[255] != uint32(idx.Count()) {
		t.Fatalf("fanout[255]=%d != count=%d", idx.Fanout()[255], idx.Count())
	}
	// Iteration is OID-sorted.
	var prev OID
	first := true
	for i := 0; i < idx.Count(); i++ {
		oid := idx.OIDAt(i)
		if !first {
			if bytes.Compare(oid[:], prev[:]) <= 0 {
				t.Fatalf("OIDs not strictly ascending at %d", i)
			}
		}
		prev = oid
		first = false
	}
}

func TestIdx_LookupReturnsOffset(t *testing.T) {
	prefix, id, _ := makeOnePackRepo(t)
	idxBytes, err := os.ReadFile(prefix + "-" + id + ".idx")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	idx, err := ParseIdx(bytes.NewReader(idxBytes), int64(len(idxBytes)))
	if err != nil {
		t.Fatalf("ParseIdx: %v", err)
	}
	for i := 0; i < idx.Count(); i++ {
		oid := idx.OIDAt(i)
		off, ok := idx.Lookup(oid)
		if !ok {
			t.Fatalf("Lookup miss for OID at index %d", i)
		}
		_ = off
	}
}

func TestIdx_LookupMiss(t *testing.T) {
	prefix, id, _ := makeOnePackRepo(t)
	idxBytes, err := os.ReadFile(prefix + "-" + id + ".idx")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	idx, err := ParseIdx(bytes.NewReader(idxBytes), int64(len(idxBytes)))
	if err != nil {
		t.Fatalf("ParseIdx: %v", err)
	}
	var bogus OID
	if _, ok := idx.Lookup(bogus); ok {
		t.Fatalf("expected miss for zero OID")
	}
}

func TestParseIdx_RejectsBadMagic(t *testing.T) {
	garbage := make([]byte, 8+1024+40)
	copy(garbage[:4], []byte{0x00, 0x00, 0x00, 0x00}) // bad magic
	if _, err := ParseIdx(bytes.NewReader(garbage), int64(len(garbage))); err == nil {
		t.Fatalf("expected ParseIdx to reject bad magic")
	}
}

func TestParseIdx_RejectsBadVersion(t *testing.T) {
	garbage := make([]byte, 8+1024+40)
	copy(garbage[:4], []byte{0xff, 0x74, 0x4f, 0x63}) // good magic
	garbage[7] = 99                                    // bad version
	if _, err := ParseIdx(bytes.NewReader(garbage), int64(len(garbage))); err == nil {
		t.Fatalf("expected ParseIdx to reject bad version")
	}
}

// buildSyntheticIdxWithLargeOffset constructs the smallest valid .idx
// containing one OID whose offset is encoded in the large-offset table.
// The single entry's offset value is 0x100000000 (just past 32-bit).
func buildSyntheticIdxWithLargeOffset(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	// Header: magic + version
	buf.Write([]byte{0xff, 0x74, 0x4f, 0x63})
	binWrite32(&buf, 2)
	// Fanout: only the entry whose first byte is 0x42 -- so fanout[i] is 0
	// for i < 0x42 and 1 for i >= 0x42.
	for i := 0; i < 256; i++ {
		if i < 0x42 {
			binWrite32(&buf, 0)
		} else {
			binWrite32(&buf, 1)
		}
	}
	// OID: 0x42 followed by 19 zero bytes
	oid := make([]byte, 20)
	oid[0] = 0x42
	buf.Write(oid)
	// CRC32 (placeholder zero)
	binWrite32(&buf, 0)
	// Offset table: MSB set, low 31 bits = 0 (index into largeOffs)
	binWrite32(&buf, 1<<31)
	// Large offset table: one entry, value 0x100000000
	binWrite64(&buf, 0x100000000)
	// Trailer: 20 bytes pack_sha + 20 bytes idx_sha (zero placeholders)
	buf.Write(make([]byte, 40))
	return buf.Bytes()
}

func binWrite32(buf *bytes.Buffer, v uint32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	buf.Write(b[:])
}

func binWrite64(buf *bytes.Buffer, v uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	buf.Write(b[:])
}

func TestParseIdx_LargeOffsetPath(t *testing.T) {
	data := buildSyntheticIdxWithLargeOffset(t)
	idx, err := ParseIdx(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("ParseIdx: %v", err)
	}
	if idx.Count() != 1 {
		t.Fatalf("count: got %d, want 1", idx.Count())
	}
	off := idx.OffsetAt(0)
	if off != 0x100000000 {
		t.Fatalf("OffsetAt: got %#x, want 0x100000000", off)
	}
	var oid OID
	oid[0] = 0x42
	got, ok := idx.Lookup(oid)
	if !ok {
		t.Fatalf("Lookup: miss")
	}
	if got != 0x100000000 {
		t.Fatalf("Lookup: got %#x, want 0x100000000", got)
	}
}

func TestParseIdx_RejectsLargeOffsetIndexOutOfRange(t *testing.T) {
	// Build an idx where the offset table has MSB set but largeOffs is empty.
	// Easiest: take the synthetic large-offset idx and truncate the largeOffs
	// section, leaving the trailer in place.
	data := buildSyntheticIdxWithLargeOffset(t)
	// largeOffs sits between the offset table (at -8-40 from end) and the
	// trailer. Removing those 8 bytes leaves trailer adjacent to offset table.
	tampered := append([]byte{}, data[:len(data)-8-40]...)
	tampered = append(tampered, data[len(data)-40:]...)
	if _, err := ParseIdx(bytes.NewReader(tampered), int64(len(tampered))); err == nil {
		t.Fatalf("expected ParseIdx to reject large-offset index out of range")
	}
}

func TestParseIdx_RejectsCountExceedingFileSize(t *testing.T) {
	// Build a header+fanout that claims 1M entries, in a small file.
	var buf bytes.Buffer
	buf.Write([]byte{0xff, 0x74, 0x4f, 0x63})
	binWrite32(&buf, 2)
	for i := 0; i < 255; i++ {
		binWrite32(&buf, 0)
	}
	binWrite32(&buf, 1_000_000) // fanout[255] = 1M, way more than file allows
	buf.Write(make([]byte, 40)) // trailer-shaped bytes
	if _, err := ParseIdx(bytes.NewReader(buf.Bytes()), int64(buf.Len())); err == nil {
		t.Fatalf("expected ParseIdx to reject count exceeding file size")
	}
}

func TestParseIdx_RejectsUnsortedOIDs(t *testing.T) {
	// Build a valid 2-entry idx, then swap the OIDs to violate ordering.
	var buf bytes.Buffer
	buf.Write([]byte{0xff, 0x74, 0x4f, 0x63})
	binWrite32(&buf, 2)
	for i := 0; i < 256; i++ {
		// fanout: 0 for i < 0x10, 1 for 0x10 <= i < 0x42, 2 for i >= 0x42
		switch {
		case i < 0x10:
			binWrite32(&buf, 0)
		case i < 0x42:
			binWrite32(&buf, 1)
		default:
			binWrite32(&buf, 2)
		}
	}
	// OID table: write 0x42 first then 0x10 -- unsorted.
	hi := make([]byte, 20)
	hi[0] = 0x42
	lo := make([]byte, 20)
	lo[0] = 0x10
	buf.Write(hi)
	buf.Write(lo)
	// CRCs (2 × 4 bytes)
	binWrite32(&buf, 0)
	binWrite32(&buf, 0)
	// Offsets (2 × 4 bytes, no MSB)
	binWrite32(&buf, 12)
	binWrite32(&buf, 32)
	// Trailer (40 bytes)
	buf.Write(make([]byte, 40))

	if _, err := ParseIdx(bytes.NewReader(buf.Bytes()), int64(buf.Len())); err == nil {
		t.Fatalf("expected ParseIdx to reject unsorted OIDs")
	}
}

func TestParseIdx_RejectsFanoutMismatch(t *testing.T) {
	// Valid sorted 2-OID idx but with a bogus fanout that doesn't match.
	var buf bytes.Buffer
	buf.Write([]byte{0xff, 0x74, 0x4f, 0x63})
	binWrite32(&buf, 2)
	// Bogus fanout: claim 2 entries with first byte == 0, but actual entries
	// have first bytes 0x10 and 0x42.
	for i := 0; i < 256; i++ {
		binWrite32(&buf, 2)
	}
	lo := make([]byte, 20)
	lo[0] = 0x10
	hi := make([]byte, 20)
	hi[0] = 0x42
	buf.Write(lo)
	buf.Write(hi)
	binWrite32(&buf, 0)
	binWrite32(&buf, 0)
	binWrite32(&buf, 12)
	binWrite32(&buf, 32)
	buf.Write(make([]byte, 40))

	if _, err := ParseIdx(bytes.NewReader(buf.Bytes()), int64(buf.Len())); err == nil {
		t.Fatalf("expected ParseIdx to reject fanout/oid-table mismatch")
	}
}
