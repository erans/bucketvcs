package pack

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"os"
	"testing"
)

// hashObject returns the SHA-1 of (typeName SP size NUL body).
// Used by tests to verify resolveObject correctness.
func hashObject(typ ObjectType, body []byte) OID {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d", typ.String(), len(body))
	h.Write([]byte{0})
	h.Write(body)
	var o OID
	copy(o[:], h.Sum(nil))
	return o
}

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

func TestResolveObject_AllPackObjectsHashMatch_DeltaFixture(t *testing.T) {
	prefix, id, _ := makeDeltaPackRepo(t)
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
	// Track that we actually exercised the delta path.
	deltaCount := 0
	for i := 0; i < idx.Count(); i++ {
		oid := idx.OIDAt(i)
		off := idx.OffsetAt(i)
		hdr, err := readObjectHeader(r, int64(off))
		if err != nil {
			t.Fatalf("readObjectHeader: %v", err)
		}
		if hdr.Type == typeOFSDelta || hdr.Type == typeREFDelta {
			deltaCount++
		}
		obj, err := resolveObject(r, idx, off, 64)
		if err != nil {
			t.Fatalf("resolveObject %s: %v", oid, err)
		}
		// Verify SHA-1 of (typeStr SP size NUL body) matches the OID.
		h := sha1.New()
		fmt.Fprintf(h, "%s %d", obj.Type.String(), obj.Size)
		h.Write([]byte{0})
		h.Write(obj.Data)
		var got OID
		copy(got[:], h.Sum(nil))
		if got != oid {
			t.Fatalf("hash mismatch oid=%s type=%s size=%d got=%s",
				oid, obj.Type, obj.Size, got)
		}
	}
	if deltaCount == 0 {
		t.Fatalf("expected delta-producing fixture but pack has 0 OFS/REF_DELTA objects (git heuristics changed?)")
	}
	t.Logf("exercised %d delta objects out of %d total", deltaCount, idx.Count())
}

func TestResolveObject_DepthBound(t *testing.T) {
	prefix, id, _ := makeDeltaPackRepo(t)
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
	// Find any delta object and verify maxDepth=0 fails.
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
		t.Fatalf("delta fixture produced no delta objects")
	}
	off, _ := idx.Lookup(deltaOID)
	if _, err := resolveObject(r, idx, off, 0); err == nil {
		t.Fatalf("expected ErrDeltaChainTooDeep at depth 0")
	}
	// Depth 1 should succeed for a single-hop delta against a base.
	// (Some deltas chain deeper; in those cases depth=1 errors. Either
	//  outcome is acceptable for this assertion.)
}

func TestResolveObject_DepthAllowsNonDeltaAtZero(t *testing.T) {
	// A non-delta base must succeed at maxDepth=0 because no budget
	// is consumed by base resolution.
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
	if idx.Count() == 0 {
		t.Skip("empty fixture")
	}
	oid := idx.OIDAt(0)
	if _, err := resolveObject(bytes.NewReader(packBytes), idx, idx.OffsetAt(0), 0); err != nil {
		t.Fatalf("non-delta resolution at depth 0 should succeed: %v", err)
	}
	_ = oid
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

func TestApplyDelta_RejectsCopyExceedingResultSize(t *testing.T) {
	// Declare result_size=4 (the length of "xxxx"), but issue a COPY that
	// would write 10 bytes. The per-instruction check must fire.
	base := []byte("0123456789ABCDEFGHIJ")
	delta := buildCopyAndInsertDelta(t, base, []byte("xxxx"), []deltaOp{
		{copyFrom: 0, copyLen: 10},
	})
	if _, err := applyDelta(base, delta); err == nil {
		t.Fatalf("expected applyDelta to reject copy that exceeds result_size")
	}
}

func TestApplyDelta_RejectsInsertExceedingResultSize(t *testing.T) {
	// Declare result_size=2 but emit 5 bytes of INSERT. The per-instruction
	// check must fire before appending.
	var buf bytes.Buffer
	writeSizeVarint(&buf, 4) // base_size = 4
	writeSizeVarint(&buf, 2) // result_size = 2
	buf.WriteByte(5)         // INSERT 5 bytes
	buf.WriteString("hello")
	if _, err := applyDelta([]byte("base"), buf.Bytes()); err == nil {
		t.Fatalf("expected applyDelta to reject insert that exceeds result_size")
	}
}

func TestResolveObject_RejectsOfsDeltaBaseNotInIdx(t *testing.T) {
	// Build a synthetic pack buffer with an OFS_DELTA at offset 50 whose
	// BaseOffset points to offset 37 — a location that won't be in the idx
	// of our real delta fixture. resolveObjectRec must reject it.
	prefix, id, _ := makeDeltaPackRepo(t)
	idxBytes, err := os.ReadFile(prefix + "-" + id + ".idx")
	if err != nil {
		t.Fatalf("ReadFile idx: %v", err)
	}
	idx, err := ParseIdx(bytes.NewReader(idxBytes), int64(len(idxBytes)))
	if err != nil {
		t.Fatalf("ParseIdx: %v", err)
	}

	// Build a minimal synthetic pack: 200 zero bytes, then plant an
	// OFS_DELTA header at offset 50.
	//   byte 50: 0x60  — MSB clear (size=0), type=ofs_delta (6<<4), size_low=0
	//   byte 51: 13    — negOff varint (single byte, MSB clear): base = 50-13 = 37
	// Offset 37 is > 12 (pack header bound) and < 50 (self), so
	// readObjectHeader will accept it — but 37 is not in the real idx,
	// so HasOffset must reject it.
	syn := make([]byte, 200)
	syn[50] = 0x60
	syn[51] = 13
	if _, err := resolveObjectRec(bytes.NewReader(syn), idx, 50, 64, nil, nil); err == nil {
		t.Fatalf("expected ofs-delta base-not-in-idx rejection")
	}
}

func TestReadSizeVarint_RejectsOverlongPayload(t *testing.T) {
	// 10 continuation bytes (each 0x80) followed by a terminator with
	// the high bit set in its 7-bit payload — would shift bits past 63.
	buf := bytes.NewBuffer(nil)
	for i := 0; i < 10; i++ {
		buf.WriteByte(0x80)
	}
	buf.WriteByte(0x40) // payload 0b1000000 at shift=70 -> would wrap
	if _, err := readSizeVarint(buf); err == nil {
		t.Fatalf("expected overflow error for overlong varint")
	}
}

func TestReadSizeVarint_AcceptsMaxRepresentable(t *testing.T) {
	// 9 continuation bytes carrying 7 zero-payload bits each (shift up to 63),
	// then a terminator at shift=63 with payload bit 0 set: yields v=1<<63.
	buf := bytes.NewBuffer(nil)
	for i := 0; i < 9; i++ {
		buf.WriteByte(0x80)
	}
	buf.WriteByte(0x01)
	got, err := readSizeVarint(buf)
	if err != nil {
		t.Fatalf("readSizeVarint: %v", err)
	}
	want := uint64(1) << 63
	if got != want {
		t.Fatalf("got %#x, want %#x", got, want)
	}
}

func TestResolveObject_REFDeltaPath(t *testing.T) {
	// Build a minimal in-memory pack with two objects:
	//   object 1: blob "hello" at offset 12 (immediately after pack header)
	//   object 2: ref-delta whose base OID == SHA-1 of object 1, delta produces "hello world"
	// This exercises the typeREFDelta branch deterministically without
	// relying on git's deltification heuristics.

	// hashObj computes the git object SHA-1: sha1("type SP size NUL body").
	hashObj := func(typ ObjectType, body []byte) OID {
		h := sha1.New()
		fmt.Fprintf(h, "%s %d", typ.String(), len(body))
		h.Write([]byte{0})
		h.Write(body)
		var o OID
		copy(o[:], h.Sum(nil))
		return o
	}

	baseBody := []byte("hello")
	baseOID := hashObj(TypeBlob, baseBody)

	// Build the delta payload that transforms "hello" → "hello world":
	//   base_size  = 5 (varint)
	//   result_size = 11 (varint)
	//   COPY op 0x90: off selector bits = none (off==0, no bytes needed),
	//                 sz selector bit4 set (sz1=5); so header = 0x80|0x10 = 0x90, then 0x05
	//   INSERT 6 bytes: opcode 0x06, then " world"
	var deltaPayload bytes.Buffer
	writeSizeVarint(&deltaPayload, uint64(len(baseBody))) // base_size = 5
	writeSizeVarint(&deltaPayload, 11)                    // result_size = 11
	deltaPayload.WriteByte(0x90)                          // COPY: sz1 only (off==0, no off bytes)
	deltaPayload.WriteByte(0x05)                          // sz = 5
	deltaPayload.WriteByte(0x06)                          // INSERT 6 bytes
	deltaPayload.WriteString(" world")

	if deltaPayload.Len() >= 1<<4 {
		t.Fatalf("delta payload %d too large for single-byte size header in this test", deltaPayload.Len())
	}

	// Assemble the pack buffer.
	var pack bytes.Buffer

	// Pack header: "PACK" + version 2 + count 2.
	pack.WriteString("PACK")
	{
		var ver [4]byte
		binary.BigEndian.PutUint32(ver[:], 2)
		pack.Write(ver[:])
		var cnt [4]byte
		binary.BigEndian.PutUint32(cnt[:], 2)
		pack.Write(cnt[:])
	}

	// Object 1: blob "hello" at offset 12.
	off1 := uint64(pack.Len()) // should be 12
	// Type+size header: type=blob(3)<<4=0x30, low 4 bits = size=5 → 0x35, MSB clear.
	pack.WriteByte(0x35)
	{
		var zb bytes.Buffer
		zw := zlib.NewWriter(&zb)
		_, _ = zw.Write(baseBody)
		_ = zw.Close()
		pack.Write(zb.Bytes())
	}

	// Object 2: ref-delta at current offset.
	off2 := uint64(pack.Len())
	// Type+size header: type=ref_delta(7)<<4=0x70, low 4 bits = deltaPayload.Len(), MSB clear.
	pack.WriteByte(0x70 | byte(deltaPayload.Len()))
	// REF_DELTA base OID (20 bytes).
	pack.Write(baseOID[:])
	{
		var zb bytes.Buffer
		zw := zlib.NewWriter(&zb)
		_, _ = zw.Write(deltaPayload.Bytes())
		_ = zw.Close()
		pack.Write(zb.Bytes())
	}

	// Build the delta result OID for indexing.
	deltaResultOID := hashObj(TypeBlob, []byte("hello world"))

	// Build a synthetic .idx for both objects.
	idxData := buildIdxFor2Objects(t, baseOID, deltaResultOID, off1, off2)

	idx, err := ParseIdx(bytes.NewReader(idxData), int64(len(idxData)))
	if err != nil {
		t.Fatalf("ParseIdx: %v", err)
	}

	// Resolve the ref-delta object (object 2).
	off, ok := idx.Lookup(deltaResultOID)
	if !ok {
		t.Fatalf("ref-delta result OID not in idx")
	}
	obj, err := resolveObject(bytes.NewReader(pack.Bytes()), idx, off, 64)
	if err != nil {
		t.Fatalf("resolveObject ref-delta: %v", err)
	}
	if obj.Type != TypeBlob {
		t.Fatalf("type: got %v, want blob", obj.Type)
	}
	if string(obj.Data) != "hello world" {
		t.Fatalf("body: got %q, want %q", obj.Data, "hello world")
	}
}

// buildIdxFor2Objects constructs a minimal valid v2 .idx for exactly two
// objects at the given offsets. OIDs and offsets are placed in sorted order.
// The trailer idx_sha1 is computed correctly so ParseIdx's hash check passes.
func buildIdxFor2Objects(t *testing.T, oid1, oid2 OID, off1, off2 uint64) []byte {
	t.Helper()
	// Sort the two by OID.
	a, b := oid1, oid2
	offA, offB := off1, off2
	if bytes.Compare(a[:], b[:]) > 0 {
		a, b = b, a
		offA, offB = offB, offA
	}
	var buf bytes.Buffer
	// Magic + version.
	buf.Write([]byte{0xff, 0x74, 0x4f, 0x63})
	binWrite32(&buf, 2)
	// Fanout table: cumulative count of OIDs with first byte <= i.
	for i := 0; i < 256; i++ {
		var cnt uint32
		if int(a[0]) <= i {
			cnt++
		}
		if int(b[0]) <= i {
			cnt++
		}
		binWrite32(&buf, cnt)
	}
	// OID table (sorted).
	buf.Write(a[:])
	buf.Write(b[:])
	// CRC32 table (zeros; M2 stores but does not validate CRCs).
	binWrite32(&buf, 0)
	binWrite32(&buf, 0)
	// Offset table (both offsets fit in 31 bits for our small in-memory packs).
	binWrite32(&buf, uint32(offA))
	binWrite32(&buf, uint32(offB))
	// Trailer: 20-byte pack SHA placeholder + 20-byte idx self-SHA.
	buf.Write(make([]byte, 20)) // pack trailer placeholder
	// Compute idx_sha1 = SHA-1 of all bytes written so far.
	pre := buf.Bytes()
	h := sha1.New()
	h.Write(pre)
	idxSHA := h.Sum(nil)
	buf.Write(idxSHA)
	return buf.Bytes()
}
