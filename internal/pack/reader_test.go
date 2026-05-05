package pack

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestReader_OpenValidatesPackMagic(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "p.pack", strings.NewReader("garbage12345"), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := store.PutIfAbsent(context.Background(), "p.idx", strings.NewReader("garbage"), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := Open(context.Background(), store, "p.pack", "p.idx"); err == nil {
		t.Fatalf("expected Open to fail on garbage pack/idx")
	}
}

func TestReader_GetMatchesGitCatFile(t *testing.T) {
	prefix, id, bareDir := makeOnePackRepo(t)
	store := newTestStore(t)
	uploadFile(t, store, prefix+"-"+id+".pack", "p.pack")
	uploadFile(t, store, prefix+"-"+id+".idx", "p.idx")
	r, err := Open(context.Background(), store, "p.pack", "p.idx")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()

	oids, err := gitcli.RevListAllObjects(context.Background(), bareDir)
	if err != nil {
		t.Fatalf("RevListAllObjects: %v", err)
	}
	for _, oidStr := range oids {
		oid, err := ParseOID(oidStr)
		if err != nil {
			t.Fatalf("ParseOID: %v", err)
		}
		if !r.Has(oid) {
			t.Fatalf("Has(%s) = false", oidStr)
		}
		obj, err := r.Get(context.Background(), oid)
		if err != nil {
			t.Fatalf("Get(%s): %v", oidStr, err)
		}
		// Verify SHA-1 of (typeStr SP size NUL body) matches the OID.
		h := sha1.New()
		fmt.Fprintf(h, "%s %d", obj.Type.String(), obj.Size)
		h.Write([]byte{0})
		h.Write(obj.Data)
		var got OID
		copy(got[:], h.Sum(nil))
		if got != oid {
			t.Fatalf("Get hash mismatch for %s: got=%s", oidStr, got)
		}
	}
}

func TestReader_GetWithDeltaFixture(t *testing.T) {
	prefix, id, bareDir := makeDeltaPackRepo(t)
	store := newTestStore(t)
	uploadFile(t, store, prefix+"-"+id+".pack", "p.pack")
	uploadFile(t, store, prefix+"-"+id+".idx", "p.idx")
	r, err := Open(context.Background(), store, "p.pack", "p.idx")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()
	oids, err := gitcli.RevListAllObjects(context.Background(), bareDir)
	if err != nil {
		t.Fatalf("RevListAllObjects: %v", err)
	}
	for _, oidStr := range oids {
		oid, err := ParseOID(oidStr)
		if err != nil {
			t.Fatalf("ParseOID: %v", err)
		}
		obj, err := r.Get(context.Background(), oid)
		if err != nil {
			t.Fatalf("Get(%s): %v", oidStr, err)
		}
		h := sha1.New()
		fmt.Fprintf(h, "%s %d", obj.Type.String(), obj.Size)
		h.Write([]byte{0})
		h.Write(obj.Data)
		var got OID
		copy(got[:], h.Sum(nil))
		if got != oid {
			t.Fatalf("Get hash mismatch for %s", oidStr)
		}
	}
}

func TestReader_ForEach_OrderAndCount(t *testing.T) {
	prefix, id, _ := makeOnePackRepo(t)
	store := newTestStore(t)
	uploadFile(t, store, prefix+"-"+id+".pack", "p.pack")
	uploadFile(t, store, prefix+"-"+id+".idx", "p.idx")
	r, err := Open(context.Background(), store, "p.pack", "p.idx")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()
	var prev OID
	first := true
	count := 0
	if err := r.ForEach(func(oid OID, off uint64) error {
		if !first && bytes.Compare(oid[:], prev[:]) <= 0 {
			t.Fatalf("ForEach not OID-sorted")
		}
		prev = oid
		first = false
		count++
		return nil
	}); err != nil {
		t.Fatalf("ForEach: %v", err)
	}
	if count == 0 {
		t.Fatalf("ForEach saw no objects")
	}
}

func TestReader_GetMissingOID(t *testing.T) {
	prefix, id, _ := makeOnePackRepo(t)
	store := newTestStore(t)
	uploadFile(t, store, prefix+"-"+id+".pack", "p.pack")
	uploadFile(t, store, prefix+"-"+id+".idx", "p.idx")
	r, err := Open(context.Background(), store, "p.pack", "p.idx")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()
	var bogus OID
	if _, err := r.Get(context.Background(), bogus); err == nil {
		t.Fatalf("Get on missing OID should error")
	}
}

func TestReader_CacheReturnsSameDataOnHit(t *testing.T) {
	prefix, id, _ := makeOnePackRepo(t)
	store := newTestStore(t)
	uploadFile(t, store, prefix+"-"+id+".pack", "p.pack")
	uploadFile(t, store, prefix+"-"+id+".idx", "p.idx")
	r, err := Open(context.Background(), store, "p.pack", "p.idx")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()
	if r.Idx().Count() == 0 {
		t.Skip("empty fixture")
	}
	oid := r.Idx().OIDAt(0)
	a, err := r.Get(context.Background(), oid)
	if err != nil {
		t.Fatalf("Get a: %v", err)
	}
	b, err := r.Get(context.Background(), oid)
	if err != nil {
		t.Fatalf("Get b: %v", err)
	}
	if !bytes.Equal(a.Data, b.Data) {
		t.Fatalf("cache hit returned different bytes")
	}
}

func uploadFile(t *testing.T, store storage.ObjectStore, srcPath, dstKey string) {
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

func TestReader_Open_RejectsPackIdxMismatch(t *testing.T) {
	// Build a valid pack/idx pair, then mutate a body byte in the pack
	// without recomputing the trailer. The self-SHA-1 check should catch
	// this because SHA-1(header+mutated_body) != pack trailer.
	prefix, id, _ := makeOnePackRepo(t)
	packBytes, err := os.ReadFile(prefix + "-" + id + ".pack")
	if err != nil {
		t.Fatalf("ReadFile pack: %v", err)
	}
	idxBytes, err := os.ReadFile(prefix + "-" + id + ".idx")
	if err != nil {
		t.Fatalf("ReadFile idx: %v", err)
	}
	// Mutate a byte well inside the pack body (after the 12-byte header,
	// before the 20-byte trailer). The trailer is now stale relative to
	// the mutated body, so SHA-1(header+body) != trailer.
	if len(packBytes) < 32+20 {
		t.Skip("pack too small to mutate body safely")
	}
	tampered := append([]byte(nil), packBytes...)
	tampered[12] ^= 0xff // mutate first byte of object data
	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "p.pack", bytes.NewReader(tampered), nil); err != nil {
		t.Fatalf("Put pack: %v", err)
	}
	if _, err := store.PutIfAbsent(context.Background(), "p.idx", bytes.NewReader(idxBytes), nil); err != nil {
		t.Fatalf("Put idx: %v", err)
	}
	if _, err := Open(context.Background(), store, "p.pack", "p.idx"); err == nil {
		t.Fatalf("expected Open to reject tampered pack")
	}
}

func TestReader_Get_RespectsCallContext(t *testing.T) {
	prefix, id, _ := makeOnePackRepo(t)
	baseStore := newTestStore(t)
	uploadFile(t, baseStore, prefix+"-"+id+".pack", "p.pack")
	uploadFile(t, baseStore, prefix+"-"+id+".idx", "p.idx")

	// Wrap the store so GetRange returns ctx.Err() when the context is done.
	// localfs doesn't check ctx on every call, so we need this shim to verify
	// that Reader.Get actually threads the caller's ctx to StoreSource.
	wrapped := &ctxCheckStore{ObjectStore: baseStore}

	r, err := Open(context.Background(), wrapped, "p.pack", "p.idx")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()
	if r.Idx().Count() == 0 {
		t.Skip("empty fixture")
	}
	oid := r.Idx().OIDAt(0)
	// Call Get with an already-canceled ctx; ctxCheckStore will surface it.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Get(ctx, oid); err == nil {
		t.Fatalf("expected error from canceled ctx")
	}
}

// ctxCheckStore wraps an ObjectStore and fails GetRange immediately when
// the context is already done. This lets tests verify ctx is threaded through.
type ctxCheckStore struct {
	storage.ObjectStore
}

func (s *ctxCheckStore) GetRange(ctx context.Context, key string, start, end int64) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.ObjectStore.GetRange(ctx, key, start, end)
}

func TestReader_Get_RejectsIDXOIDMismatch(t *testing.T) {
	// Build a minimal pack: one blob "hello".
	body := []byte("hello")
	realOID := hashGitObject(TypeBlob, body)

	// Pack: header (12) + object header (1 byte: 0x35 = type=blob, size=5, no continuation)
	// + zlib(body) + trailing 20-byte SHA-1 of (header+object data).
	var pack bytes.Buffer
	pack.WriteString("PACK")
	{
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], 2) // version
		pack.Write(b[:])
		binary.BigEndian.PutUint32(b[:], 1) // count
		pack.Write(b[:])
	}
	pack.WriteByte(0x35)
	{
		var zb bytes.Buffer
		zw := zlib.NewWriter(&zb)
		zw.Write(body)
		zw.Close()
		pack.Write(zb.Bytes())
	}
	// Compute pack trailer = SHA-1 of all bytes so far.
	trailer := sha1.Sum(pack.Bytes())
	pack.Write(trailer[:])

	// Now build a synthetic idx that LIES about the OID. Claim the blob
	// at offset 12 has a different OID than `realOID`.
	fakeOID := OID{0x42}
	for i := 1; i < 20; i++ {
		fakeOID[i] = 0x42
	}

	idxBytes := buildIdxLiar(t, fakeOID, 12, trailer)

	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "p.pack", bytes.NewReader(pack.Bytes()), nil); err != nil {
		t.Fatalf("Put pack: %v", err)
	}
	if _, err := store.PutIfAbsent(context.Background(), "p.idx", bytes.NewReader(idxBytes), nil); err != nil {
		t.Fatalf("Put idx: %v", err)
	}
	r, err := Open(context.Background(), store, "p.pack", "p.idx")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()

	// Get(fakeOID) finds it in the idx, resolves to the real blob bytes,
	// but the body hashes to realOID, not fakeOID. Must reject.
	if _, err := r.Get(context.Background(), fakeOID); err == nil {
		t.Fatalf("expected ErrPackCorrupt for idx OID-mismatch")
	}

	_ = realOID // silence unused
}

func TestReader_GetCacheImmuneToCallerMutation(t *testing.T) {
	prefix, id, _ := makeOnePackRepo(t)
	store := newTestStore(t)
	uploadFile(t, store, prefix+"-"+id+".pack", "p.pack")
	uploadFile(t, store, prefix+"-"+id+".idx", "p.idx")
	r, err := Open(context.Background(), store, "p.pack", "p.idx")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()
	if r.Idx().Count() == 0 {
		t.Skip("empty fixture")
	}
	oid := r.Idx().OIDAt(0)
	a, err := r.Get(context.Background(), oid)
	if err != nil {
		t.Fatalf("Get a: %v", err)
	}
	originalLen := len(a.Data)
	// Mutate caller's copy in-place.
	for i := range a.Data {
		a.Data[i] ^= 0xff
	}
	// Re-fetch — should be unaffected by the mutation above.
	b, err := r.Get(context.Background(), oid)
	if err != nil {
		t.Fatalf("Get b: %v", err)
	}
	if len(b.Data) != originalLen {
		t.Fatalf("len: got %d, want %d", len(b.Data), originalLen)
	}
	// Hash b again to verify; if cache was poisoned, hashGitObject would mismatch.
	gotHash := hashGitObject(b.Type, b.Data)
	if gotHash != oid {
		t.Fatalf("cache was poisoned: rehash %s != oid %s", gotHash, oid)
	}
}

func TestObjectCache_SkipsLargeObjects(t *testing.T) {
	c := newObjectCache(10, 100, 0) // max 100 bytes per entry, no total limit
	// Object below threshold caches.
	small := &Object{Type: TypeBlob, Size: 50, Data: make([]byte, 50)}
	c.put(1, small)
	if _, hit := c.get(1); !hit {
		t.Fatalf("small object should be cached")
	}
	// Object above threshold is skipped.
	big := &Object{Type: TypeBlob, Size: 200, Data: make([]byte, 200)}
	c.put(2, big)
	if _, hit := c.get(2); hit {
		t.Fatalf("large object should not be cached")
	}
}

func TestReader_Get_HashCheckOnCacheHit(t *testing.T) {
	// First Get warms the cache; mutate cache entry's bytes to a different
	// valid object; second Get for the same OID must reject as
	// ErrPackCorrupt because the body no longer hashes to oid.
	prefix, id, _ := makeOnePackRepo(t)
	store := newTestStore(t)
	uploadFile(t, store, prefix+"-"+id+".pack", "p.pack")
	uploadFile(t, store, prefix+"-"+id+".idx", "p.idx")
	r, err := Open(context.Background(), store, "p.pack", "p.idx")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()
	if r.Idx().Count() == 0 {
		t.Skip("empty fixture")
	}
	oid := r.Idx().OIDAt(0)
	if _, err := r.Get(context.Background(), oid); err != nil {
		t.Fatalf("first Get: %v", err)
	}
	// Tamper with the cache entry. Reach into the LRU and mutate the
	// cached object's Data via the package-private cache API.
	off, _ := r.Idx().Lookup(oid)
	cached, hit := r.objCache.get(off)
	if !hit {
		t.Fatalf("expected cache hit after first Get")
	}
	if len(cached.Data) == 0 {
		t.Skip("cached object is empty; can't tamper")
	}
	cached.Data[0] ^= 0xff
	if _, err := r.Get(context.Background(), oid); err == nil {
		t.Fatalf("expected ErrPackCorrupt on tampered cache entry")
	}
}

// buildIdxLiar constructs a single-entry .idx for the given (lying) OID
// pointing at the given offset, with the given pack trailer SHA-1.
func buildIdxLiar(t *testing.T, oid OID, offset uint32, packSHA [20]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.Write([]byte{0xff, 0x74, 0x4f, 0x63})
	binWrite32(&buf, 2)
	for i := 0; i < 256; i++ {
		var cnt uint32
		if oid[0] <= byte(i) {
			cnt = 1
		}
		binWrite32(&buf, cnt)
	}
	buf.Write(oid[:])
	binWrite32(&buf, 0) // crc
	binWrite32(&buf, offset)
	pre := append([]byte(nil), buf.Bytes()...)
	buf.Write(packSHA[:])
	// idx_self_sha
	h := sha1.New()
	h.Write(pre)
	h.Write(packSHA[:])
	buf.Write(h.Sum(nil))
	return buf.Bytes()
}

func TestReader_Open_RejectsIDXOffsetOutOfBounds(t *testing.T) {
	prefix, id, _ := makeOnePackRepo(t)
	packBytes, err := os.ReadFile(prefix + "-" + id + ".pack")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	idxBytes, err := os.ReadFile(prefix + "-" + id + ".idx")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	idx, err := ParseIdx(bytes.NewReader(idxBytes), int64(len(idxBytes)))
	if err != nil {
		t.Fatalf("ParseIdx: %v", err)
	}
	if idx.Count() == 0 {
		t.Skip("empty fixture")
	}
	// Mutate the first offset in the idx to point past the pack body.
	// idx layout: header(8) + fanout(1024) + oids(count*20) + crcs(count*4) + offsets(count*4) ...
	count := idx.Count()
	offsetTableStart := 8 + 1024 + count*20 + count*4
	bigOffset := uint32(len(packBytes) + 1000)
	tampered := append([]byte(nil), idxBytes...)
	binary.BigEndian.PutUint32(tampered[offsetTableStart:offsetTableStart+4], bigOffset)
	// Re-trailer.
	pre := tampered[:len(tampered)-40]
	idxSelfSHA := sha1.Sum(pre)
	copy(tampered[len(tampered)-20:], idxSelfSHA[:])

	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "p.pack", bytes.NewReader(packBytes), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := store.PutIfAbsent(context.Background(), "p.idx", bytes.NewReader(tampered), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := Open(context.Background(), store, "p.pack", "p.idx"); err == nil {
		t.Fatalf("expected Open to reject idx offset outside pack body")
	}
}

func TestObjectCache_TotalByteBudget(t *testing.T) {
	// 10 entries max, 1000 bytes per object, 5000 bytes total budget.
	c := newObjectCache(10, 1000, 5000)
	mk := func(size int) *Object {
		return &Object{Type: TypeBlob, Size: int64(size), Data: make([]byte, size)}
	}
	for i := uint64(0); i < 7; i++ {
		c.put(i, mk(1000))
	}
	// 7 × 1000 = 7000 bytes attempted, but budget is 5000, so cache should
	// hold at most 5 entries (oldest 2 evicted).
	c.mu.Lock()
	count := c.order.Len()
	totalBytes := c.totalBytes
	c.mu.Unlock()
	if totalBytes > 5000 {
		t.Fatalf("totalBytes: got %d, exceeds budget 5000", totalBytes)
	}
	if count > 5 {
		t.Fatalf("entries: got %d, want <=5 given budget", count)
	}
	// Oldest entries (off 0, 1) must have been evicted.
	if _, hit := c.get(0); hit {
		t.Fatalf("offset 0 should have been evicted")
	}
	// Most-recent must still be present.
	if _, hit := c.get(6); !hit {
		t.Fatalf("offset 6 should still be cached")
	}
}
