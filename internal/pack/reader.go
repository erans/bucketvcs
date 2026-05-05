package pack

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// DefaultDeltaChainDepth bounds delta resolution recursion. Each delta
// hop consumes one unit; non-delta bases consume none. M2 picks a
// generous value; M9 may tune.
const DefaultDeltaChainDepth = 50

// DefaultObjectCacheEntries bounds the delta-base LRU. Unit: objects.
const DefaultObjectCacheEntries = 256

// DefaultObjectCacheBytes is the per-entry size threshold for the
// delta-base LRU. Objects larger than this skip the cache. 4 MiB is
// well above typical commit/tree/blob bodies and below what could
// trigger memory pressure when the LRU fills up.
const DefaultObjectCacheBytes = int64(4 * 1024 * 1024)

// DefaultObjectCacheTotalBytes bounds the total memory held by the
// delta-base LRU. With per-object cap (DefaultObjectCacheBytes) of
// 4 MiB and entry cap of 256, this gives a defense-in-depth ceiling.
const DefaultObjectCacheTotalBytes = int64(64 * 1024 * 1024)

// Reader is a pure-Go random-access pack reader.
type Reader struct {
	idx      *Idx
	packKey  string
	idxKey   string
	store    storage.ObjectStore
	chainCap int
	objCache *objectCache
	packSize int64
}

// Open loads the .idx in full from store and validates the .pack header
// magic + version + count. All subsequent object reads are lazy range
// GETs against store via StoreSource.
func Open(ctx context.Context, store storage.ObjectStore, packKey, idxKey string) (*Reader, error) {
	idxBytes, err := readAll(ctx, store, idxKey)
	if err != nil {
		return nil, fmt.Errorf("pack: read idx: %w", err)
	}
	idx, err := ParseIdx(bytes.NewReader(idxBytes), int64(len(idxBytes)))
	if err != nil {
		return nil, err
	}
	packMeta, err := store.Head(ctx, packKey)
	if err != nil {
		return nil, fmt.Errorf("pack: head pack: %w", err)
	}
	src := NewStoreSource(ctx, store, packKey, packMeta.Size)
	if err := validatePackHeader(src, idx); err != nil {
		return nil, err
	}
	// Verify pack self-integrity (SHA-1 of body == trailer) and that
	// the pack belongs to this idx (idx.PackTrailerSHA1 == pack trailer).
	bodyEnd := packMeta.Size - 20
	if bodyEnd < 12 {
		return nil, fmt.Errorf("%w: pack too small (%d bytes)", ErrPackCorrupt, packMeta.Size)
	}
	// Validate idx offsets fall within the pack object area (between
	// the 12-byte pack header and the 20-byte trailer). Compare in
	// uint64 space: casting a uint64 > MaxInt64 to int64 wraps negative
	// and would bypass the >= bodyEnd check.
	bodyEndU := uint64(bodyEnd)
	for k := 0; k < idx.Count(); k++ {
		off := idx.OffsetAt(k)
		if off < 12 || off >= bodyEndU {
			return nil, fmt.Errorf("%w: idx offset %d (entry %d) outside pack body [%d, %d)",
				ErrPackCorrupt, off, k, 12, bodyEnd)
		}
	}
	h := sha1.New()
	const chunk = 64 * 1024
	buf := make([]byte, chunk)
	pos := int64(0)
	for pos < bodyEnd {
		want := int64(chunk)
		if bodyEnd-pos < want {
			want = bodyEnd - pos
		}
		n, readErr := src.ReadAt(buf[:want], pos)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, fmt.Errorf("%w: hash pack body: %v", ErrPackCorrupt, readErr)
		}
		h.Write(buf[:n])
		pos += int64(n)
	}
	gotBodySHA := h.Sum(nil)
	gotTrailer := make([]byte, 20)
	if _, err := src.ReadAt(gotTrailer, bodyEnd); err != nil {
		return nil, fmt.Errorf("%w: read pack trailer: %v", ErrPackCorrupt, err)
	}
	if !bytes.Equal(gotBodySHA, gotTrailer) {
		return nil, fmt.Errorf("%w: pack self-SHA-1 mismatch (body=%x trailer=%x)",
			ErrPackCorrupt, gotBodySHA, gotTrailer)
	}
	wantTrailer := idx.PackTrailerSHA1()
	if !bytes.Equal(gotTrailer, wantTrailer[:]) {
		return nil, fmt.Errorf("%w: pack/idx trailer mismatch (pack=%x idx=%x)",
			ErrPackCorrupt, gotTrailer, wantTrailer[:])
	}
	return &Reader{
		idx: idx, packKey: packKey, idxKey: idxKey, store: store,
		chainCap: DefaultDeltaChainDepth,
		objCache: newObjectCache(DefaultObjectCacheEntries, DefaultObjectCacheBytes, DefaultObjectCacheTotalBytes),
		packSize: packMeta.Size,
	}, nil
}

func readAll(ctx context.Context, s storage.ObjectStore, key string) ([]byte, error) {
	obj, err := s.Get(ctx, key, nil)
	if err != nil {
		return nil, err
	}
	defer obj.Body.Close()
	return io.ReadAll(obj.Body)
}

// validatePackHeader checks the 12-byte pack header: magic "PACK", version
// 2, and object count == idx count.
func validatePackHeader(r io.ReaderAt, idx *Idx) error {
	hdr := make([]byte, 12)
	if _, err := r.ReadAt(hdr, 0); err != nil {
		return fmt.Errorf("%w: read pack header: %v", ErrPackCorrupt, err)
	}
	if string(hdr[:4]) != "PACK" {
		return fmt.Errorf("%w: pack magic %x", ErrPackCorrupt, hdr[:4])
	}
	if v := binary.BigEndian.Uint32(hdr[4:8]); v != 2 {
		return fmt.Errorf("%w: pack version %d", ErrPackCorrupt, v)
	}
	cnt := binary.BigEndian.Uint32(hdr[8:12])
	if int(cnt) != idx.Count() {
		return fmt.Errorf("%w: pack count %d != idx count %d", ErrPackCorrupt, cnt, idx.Count())
	}
	return nil
}

// Close releases reader resources. Safe to call multiple times.
func (r *Reader) Close() error { return nil }

// Has reports whether oid is present in this pack's idx.
func (r *Reader) Has(oid OID) bool {
	_, ok := r.idx.Lookup(oid)
	return ok
}

// hashGitObject returns the SHA-1 of (type SP size NUL body). Cheap helper
// for Get's identity check; we don't expose it because the rule of thumb
// is "consumers should call git's hash conventions through Object, not
// reach for SHA-1 directly."
func hashGitObject(typ ObjectType, body []byte) OID {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d", typ.String(), len(body))
	h.Write([]byte{0})
	h.Write(body)
	var o OID
	copy(o[:], h.Sum(nil))
	return o
}

// Get returns the fully-resolved object for oid, or an error.
//
// Verifies that the resolved object actually hashes to oid before
// returning, defending against a corrupt .idx whose OID->offset
// mapping points at the wrong pack content. Cache hits are also
// verified: the cached body is rehashed to guard against future
// cache-poisoning bugs and defense in depth against corrupt idx.
func (r *Reader) Get(ctx context.Context, oid OID) (*Object, error) {
	off, ok := r.idx.Lookup(oid)
	if !ok {
		return nil, fmt.Errorf("pack: oid %s not in idx", oid)
	}
	if obj, hit := r.objCache.get(off); hit {
		// Verify the cached object actually hashes to the requested OID.
		// Without this check, a corrupt idx that maps multiple OIDs to
		// the same offset (caught at idx parse time, but defense in
		// depth) or a future cache-poisoning bug could serve wrong
		// bytes under a different OID. Cost: one SHA-1 over the body
		// per cache hit (~hundreds of MB/s on modern CPUs).
		if hashGitObject(obj.Type, obj.Data) != oid {
			return nil, fmt.Errorf("%w: cache OID mismatch for off=%d", ErrPackCorrupt, off)
		}
		return &Object{Type: obj.Type, Size: obj.Size, Data: append([]byte(nil), obj.Data...)}, nil
	}
	// Build a per-call StoreSource so the call's ctx (not Open's)
	// governs range reads. This is essentially free (4-field struct).
	src := NewStoreSource(ctx, r.store, r.packKey, r.packSize)
	obj, err := resolveObject(src, r.idx, off, r.chainCap)
	if err != nil {
		return nil, err
	}
	got := hashGitObject(obj.Type, obj.Data)
	if got != oid {
		return nil, fmt.Errorf("%w: oid %s resolves to body hashing to %s",
			ErrPackCorrupt, oid, got)
	}
	cached := &Object{Type: obj.Type, Size: obj.Size, Data: append([]byte(nil), obj.Data...)}
	r.objCache.put(off, cached)
	return obj, nil
}

// ForEach calls fn for every (oid, packOffset) in the idx, in OID-sorted order.
// Returning a non-nil error terminates iteration with that error.
func (r *Reader) ForEach(fn func(OID, uint64) error) error {
	for i := 0; i < r.idx.Count(); i++ {
		if err := fn(r.idx.OIDAt(i), r.idx.OffsetAt(i)); err != nil {
			return err
		}
	}
	return nil
}

// Idx exposes the parsed idx for index-builders. Not for hot-path use.
func (r *Reader) Idx() *Idx { return r.idx }
