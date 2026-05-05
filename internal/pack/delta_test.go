package pack

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"os"
	"testing"
)

func TestApplyDelta_InsertOnly(t *testing.T) {
	base := []byte("the quick brown fox")
	result := []byte("the lazy dog")
	delta := buildSyntheticInsertOnlyDelta(t, len(base), result)
	got, err := applyDelta(base, delta)
	if err != nil {
		t.Fatalf("applyDelta: %v", err)
	}
	if !bytes.Equal(got, result) {
		t.Fatalf("applyDelta: got %q, want %q", got, result)
	}
}

func TestApplyDelta_CopyAndInsert(t *testing.T) {
	base := []byte("the quick brown fox jumps")
	result := []byte("the brown fox jumps over the quick")
	delta := buildCopyAndInsertDelta(t, base, result, []deltaOp{
		{copyFrom: 0, copyLen: 4},   // "the "
		{copyFrom: 10, copyLen: 15}, // "brown fox jumps"
		{insert: []byte(" over the")},
		{copyFrom: 3, copyLen: 6}, // " quick"
	})
	got, err := applyDelta(base, delta)
	if err != nil {
		t.Fatalf("applyDelta: %v", err)
	}
	if !bytes.Equal(got, result) {
		t.Fatalf("applyDelta: got %q, want %q", got, result)
	}
}

func TestApplyDelta_RejectsBaseSizeMismatch(t *testing.T) {
	delta := buildSyntheticInsertOnlyDelta(t, 999, []byte("x"))
	if _, err := applyDelta([]byte("base"), delta); err == nil {
		t.Fatalf("expected base size mismatch error")
	}
}

func TestApplyDelta_RejectsCopyOutOfRange(t *testing.T) {
	base := []byte("hi")
	delta := buildCopyAndInsertDelta(t, base, []byte("HiHi"), []deltaOp{
		{copyFrom: 100, copyLen: 2}, // out of range
	})
	if _, err := applyDelta(base, delta); err == nil {
		t.Fatalf("expected copy out-of-range error")
	}
}

func TestApplyDelta_RejectsReservedOpcode(t *testing.T) {
	var buf bytes.Buffer
	writeSizeVarint(&buf, 1)
	writeSizeVarint(&buf, 1)
	buf.WriteByte(0x00) // reserved
	if _, err := applyDelta([]byte("x"), buf.Bytes()); err == nil {
		t.Fatalf("expected reserved opcode error")
	}
}

func TestResolveObject_AllPackObjectsHashMatch(t *testing.T) {
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
	r := bytes.NewReader(packBytes)
	for i := 0; i < idx.Count(); i++ {
		oid := idx.OIDAt(i)
		obj, err := resolveObject(r, idx, idx.OffsetAt(i), 64)
		if err != nil {
			t.Fatalf("resolveObject %s: %v", oid, err)
		}
		// Hash (type SP size NUL body) and compare to OID.
		h := sha1.New()
		fmt.Fprintf(h, "%s %d", obj.Type.String(), obj.Size)
		h.Write([]byte{0})
		h.Write(obj.Data)
		var got OID
		copy(got[:], h.Sum(nil))
		if got != oid {
			t.Fatalf("resolveObject hash mismatch: oid=%s type=%s size=%d got=%s",
				oid, obj.Type, obj.Size, got)
		}
	}
}

func TestResolveObject_DepthBound(t *testing.T) {
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
	// Find a delta object (if any). If none in this fixture, skip.
	r := bytes.NewReader(packBytes)
	var deltaOID OID
	found := false
	for i := 0; i < idx.Count(); i++ {
		hdr, err := readObjectHeader(r, int64(idx.OffsetAt(i)))
		if err != nil {
			continue
		}
		if hdr.Type == typeOFSDelta || hdr.Type == typeREFDelta {
			deltaOID = idx.OIDAt(i)
			found = true
			break
		}
	}
	if !found {
		t.Skip("no delta objects in fixture pack")
	}
	off, _ := idx.Lookup(deltaOID)
	if _, err := resolveObject(r, idx, off, 0); err == nil {
		t.Fatalf("expected ErrDeltaChainTooDeep at depth 0")
	}
}

// deltaOp helps tests describe a sequence of copy/insert operations.
type deltaOp struct {
	copyFrom, copyLen int
	insert            []byte
}

func buildSyntheticInsertOnlyDelta(t *testing.T, baseSize int, result []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	writeSizeVarint(&out, uint64(baseSize))
	writeSizeVarint(&out, uint64(len(result)))
	for len(result) > 0 {
		n := len(result)
		if n > 127 {
			n = 127
		}
		out.WriteByte(byte(n))
		out.Write(result[:n])
		result = result[n:]
	}
	return out.Bytes()
}

func buildCopyAndInsertDelta(t *testing.T, base, result []byte, ops []deltaOp) []byte {
	t.Helper()
	var out bytes.Buffer
	writeSizeVarint(&out, uint64(len(base)))
	writeSizeVarint(&out, uint64(len(result)))
	for _, op := range ops {
		if op.insert != nil {
			rem := op.insert
			for len(rem) > 0 {
				n := len(rem)
				if n > 127 {
					n = 127
				}
				out.WriteByte(byte(n))
				out.Write(rem[:n])
				rem = rem[n:]
			}
			continue
		}
		// Copy op: top bit set; following bits select which offset/size
		// bytes are present, written in little-endian order.
		var hdr byte = 0x80
		var enc bytes.Buffer
		off := uint32(op.copyFrom)
		if off&0x000000ff != 0 {
			hdr |= 0x01
			enc.WriteByte(byte(off & 0xff))
		}
		if off&0x0000ff00 != 0 {
			hdr |= 0x02
			enc.WriteByte(byte((off >> 8) & 0xff))
		}
		if off&0x00ff0000 != 0 {
			hdr |= 0x04
			enc.WriteByte(byte((off >> 16) & 0xff))
		}
		if off&0xff000000 != 0 {
			hdr |= 0x08
			enc.WriteByte(byte((off >> 24) & 0xff))
		}
		sz := uint32(op.copyLen)
		if sz&0x0000ff != 0 {
			hdr |= 0x10
			enc.WriteByte(byte(sz & 0xff))
		}
		if sz&0x00ff00 != 0 {
			hdr |= 0x20
			enc.WriteByte(byte((sz >> 8) & 0xff))
		}
		if sz&0xff0000 != 0 {
			hdr |= 0x40
			enc.WriteByte(byte((sz >> 16) & 0xff))
		}
		out.WriteByte(hdr)
		out.Write(enc.Bytes())
	}
	return out.Bytes()
}

func writeSizeVarint(w *bytes.Buffer, n uint64) {
	for {
		b := byte(n & 0x7f)
		n >>= 7
		if n != 0 {
			b |= 0x80
		}
		w.WriteByte(b)
		if n == 0 {
			return
		}
	}
}
