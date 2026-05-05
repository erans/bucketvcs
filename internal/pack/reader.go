package pack

import (
	"bytes"
	"context"
	"encoding/binary"
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

// Reader is a pure-Go random-access pack reader.
type Reader struct {
	idx      *Idx
	pack     io.ReaderAt
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
	return &Reader{
		idx: idx, pack: src, packKey: packKey, idxKey: idxKey, store: store,
		chainCap: DefaultDeltaChainDepth,
		objCache: newObjectCache(DefaultObjectCacheEntries),
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

// Get returns the fully-resolved object for oid, or an error.
func (r *Reader) Get(ctx context.Context, oid OID) (*Object, error) {
	off, ok := r.idx.Lookup(oid)
	if !ok {
		return nil, fmt.Errorf("pack: oid %s not in idx", oid)
	}
	if obj, hit := r.objCache.get(off); hit {
		return obj, nil
	}
	obj, err := resolveObject(r.pack, r.idx, off, r.chainCap)
	if err != nil {
		return nil, err
	}
	r.objCache.put(off, obj)
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
