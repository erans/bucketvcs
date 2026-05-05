package pack

import (
	"bytes"
	"context"
	"crypto/sha1"
	"fmt"
	"os"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
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

// Silence unused import if context isn't needed yet.
var _ = context.Background
var _ = gitcli.Version
