package pack

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"io"
	"os"
	"testing"
)

func TestReadObjectHeader_AllObjectsValid(t *testing.T) {
	prefix, id, _ := makeOnePackRepo(t)
	packBytes, err := os.ReadFile(prefix + "-" + id + ".pack")
	if err != nil {
		t.Fatalf("ReadFile pack: %v", err)
	}
	idxBytes, err := os.ReadFile(prefix + "-" + id + ".idx")
	if err != nil {
		t.Fatalf("ReadFile idx: %v", err)
	}
	idx, err := ParseIdx(bytes.NewReader(idxBytes), int64(len(idxBytes)))
	if err != nil {
		t.Fatalf("ParseIdx: %v", err)
	}
	for i := 0; i < idx.Count(); i++ {
		oid := idx.OIDAt(i)
		off := idx.OffsetAt(i)
		hdr, err := readObjectHeader(bytes.NewReader(packBytes), int64(off))
		if err != nil {
			t.Fatalf("readObjectHeader at oid=%s off=%d: %v", oid, off, err)
		}
		// Type must be a recognized pack-format type.
		switch hdr.Type {
		case TypeCommit, TypeTree, TypeBlob, TypeTag, typeOFSDelta, typeREFDelta:
			// ok
		default:
			t.Fatalf("unexpected type %v at oid %s", hdr.Type, oid)
		}
		if hdr.Size < 0 {
			t.Fatalf("negative size %d at oid %s", hdr.Size, oid)
		}
		if hdr.HeaderLen <= 0 {
			t.Fatalf("zero/neg HeaderLen at oid %s", oid)
		}
	}
}

func TestInflateAt_NonDeltaObjectsHashMatch(t *testing.T) {
	prefix, id, _ := makeOnePackRepo(t)
	packBytes, err := os.ReadFile(prefix + "-" + id + ".pack")
	if err != nil {
		t.Fatalf("ReadFile pack: %v", err)
	}
	idxBytes, err := os.ReadFile(prefix + "-" + id + ".idx")
	if err != nil {
		t.Fatalf("ReadFile idx: %v", err)
	}
	idx, err := ParseIdx(bytes.NewReader(idxBytes), int64(len(idxBytes)))
	if err != nil {
		t.Fatalf("ParseIdx: %v", err)
	}
	checked := 0
	for i := 0; i < idx.Count(); i++ {
		oid := idx.OIDAt(i)
		off := idx.OffsetAt(i)
		hdr, err := readObjectHeader(bytes.NewReader(packBytes), int64(off))
		if err != nil {
			t.Fatalf("readObjectHeader: %v", err)
		}
		if hdr.Type == typeOFSDelta || hdr.Type == typeREFDelta {
			continue // delta resolution lands in Task 9
		}
		body, err := inflateAt(bytes.NewReader(packBytes), int64(off)+hdr.HeaderLen, hdr.Size)
		if err != nil {
			t.Fatalf("inflate %s: %v", oid, err)
		}
		// Recompute the SHA-1 of (type SP size NUL body) and verify it
		// equals the OID — the strongest equivalence check.
		var typeStr string
		switch hdr.Type {
		case TypeCommit:
			typeStr = "commit"
		case TypeTree:
			typeStr = "tree"
		case TypeBlob:
			typeStr = "blob"
		case TypeTag:
			typeStr = "tag"
		}
		hashed := sha1.New()
		fmt.Fprintf(hashed, "%s %d", typeStr, hdr.Size)
		hashed.Write([]byte{0})
		hashed.Write(body)
		var got OID
		copy(got[:], hashed.Sum(nil))
		if got != oid {
			t.Fatalf("inflated body hash mismatch for %s (type %s, size %d): got %s",
				oid, typeStr, hdr.Size, got)
		}
		checked++
	}
	if checked == 0 {
		t.Fatalf("expected to check >=1 non-delta object, got 0")
	}
}

func TestReadObjectHeader_RejectsTruncated(t *testing.T) {
	// A 0-byte pack section: ReadAt fails to read even the first byte.
	if _, err := readObjectHeader(bytes.NewReader([]byte{}), 0); err == nil {
		t.Fatalf("expected error on empty input")
	}
}

// bytesReaderForTest wraps []byte as io.ReaderAt.
func bytesReaderForTest(b []byte) io.ReaderAt {
	return bytes.NewReader(b)
}

func TestReadObjectHeader_RejectsNegativeSizeFromVarint(t *testing.T) {
	// Construct a header that terminates with bit-3 set at shift=60,
	// making the int64 size negative.
	// First byte: MSB set, type=blob (3<<4=0x30), size_low=0 -> 0xb0
	// Then 8 continuation bytes of 0x80 (MSB set, value 0).
	// After N continuation bytes: shift = 4 + 7*N. N=8 -> shift=60.
	// Final byte: 0x08 (MSB clear, value 8 << 60 = bit 63 = sign bit).
	b := append([]byte{0xb0}, make([]byte, 8)...)
	for i := 1; i <= 8; i++ {
		b[i] = 0x80
	}
	b = append(b, 0x08)
	if _, err := readObjectHeader(bytesReaderForTest(b), 0); err == nil {
		t.Fatalf("expected rejection of negative size from varint")
	}
}

func TestReadObjectHeader_RejectsOfsDeltaBaseInsideHeader(t *testing.T) {
	// Build a synthetic ofs_delta that would point its base into the pack
	// header. The "this offset" is 50; negOff=50 -> base=0. Need to write
	// the bytes at offset 50 of a 200-byte buffer; preceding bytes can be
	// zero (we only ReadAt at off=50).
	pack := make([]byte, 200)
	// First byte: MSB clear (terminate), type=ofs_delta (6) << 4 = 0x60, size_low=0
	pack[50] = 0x60
	// negOff varint: single byte, value=50, MSB clear -> 0x32
	pack[51] = 0x32
	if _, err := readObjectHeader(bytes.NewReader(pack), 50); err == nil {
		t.Fatalf("expected rejection of ofs_delta with base inside pack header")
	}
}

func TestReadObjectHeader_RejectsOfsDeltaBaseAtOrAfterSelf(t *testing.T) {
	// Build a synthetic ofs_delta where negOff=0 -> base == this offset.
	pack := make([]byte, 200)
	pack[50] = 0x60
	pack[51] = 0x00 // negOff = 0
	if _, err := readObjectHeader(bytes.NewReader(pack), 50); err == nil {
		t.Fatalf("expected rejection of ofs_delta with base == this offset")
	}
}
