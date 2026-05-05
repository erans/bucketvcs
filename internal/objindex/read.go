package objindex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"sort"

	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Map is a parsed .bvom file held in memory. Lookup is O(log n) via
// binary search on the sorted-by-OID record table.
type Map struct {
	count     int
	records   []byte // count * recordSize
	packTable []string
}

// Open reads the entire .bvom from store, validates structure + trailer
// hash, and returns a Map.
func Open(ctx context.Context, store storage.ObjectStore, key string) (*Map, error) {
	rc, err := getReader(ctx, store, key)
	if err != nil {
		return nil, fmt.Errorf("objindex: get %s: %w", key, err)
	}
	defer rc.Close()
	all, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("objindex: read %s: %w", key, err)
	}
	if len(all) < headerSize+trailerSize {
		return nil, fmt.Errorf("%w: file too small (%d)", ErrCorrupt, len(all))
	}
	// Validate trailer BEFORE any parsing — defends against header-tampering
	// attacks where magic/version look right but the body is wrong.
	want := sha256.Sum256(all[:len(all)-trailerSize])
	if !bytes.Equal(want[:], all[len(all)-trailerSize:]) {
		return nil, fmt.Errorf("%w: trailer hash mismatch", ErrCorrupt)
	}
	// Parse header.
	if !bytes.Equal(all[:4], magic) {
		return nil, fmt.Errorf("%w: bad magic %x", ErrCorrupt, all[:4])
	}
	if v := binary.BigEndian.Uint32(all[4:8]); v != currentVer {
		return nil, fmt.Errorf("%w: version %d", ErrCorrupt, v)
	}
	count := binary.BigEndian.Uint64(all[8:16])
	// Bound count to a value that cannot overflow when multiplied by
	// recordSize and that fits in int (Lookup uses int via sort.Search).
	maxRecords := uint64(len(all)-headerSize-trailerSize) / uint64(recordSize)
	if count > maxRecords {
		return nil, fmt.Errorf("%w: count %d exceeds file capacity %d records",
			ErrCorrupt, count, maxRecords)
	}
	if count > uint64(math.MaxInt32) {
		return nil, fmt.Errorf("%w: count %d exceeds MaxInt32", ErrCorrupt, count)
	}
	packTblOff := binary.BigEndian.Uint64(all[16:24])
	expectedRecBytes := uint64(headerSize) + count*uint64(recordSize)
	if packTblOff != expectedRecBytes {
		return nil, fmt.Errorf("%w: pack_tbl offset mismatch (got %d, want %d)",
			ErrCorrupt, packTblOff, expectedRecBytes)
	}
	// Validate packTblOff range before slicing. count*recordSize was
	// already bounded above, but packTblOff is read from the file
	// independently and must fit before the trailer.
	if packTblOff+2 > uint64(len(all))-uint64(trailerSize) {
		return nil, fmt.Errorf("%w: pack-table header would overlap trailer (off=%d)",
			ErrCorrupt, packTblOff)
	}
	nPacks := binary.BigEndian.Uint16(all[packTblOff : packTblOff+2])
	tblBytes := uint64(nPacks) * uint64(packIDSize)
	expectedTotal := packTblOff + 2 + tblBytes + uint64(trailerSize)
	if uint64(len(all)) != expectedTotal {
		return nil, fmt.Errorf("%w: file size %d != expected %d (count=%d nPacks=%d)",
			ErrCorrupt, len(all), expectedTotal, count, nPacks)
	}
	packs := make([]string, nPacks)
	for i := 0; i < int(nPacks); i++ {
		off := packTblOff + 2 + uint64(i)*uint64(packIDSize)
		packs[i] = string(all[off : off+uint64(packIDSize)])
	}
	// Validate pack IDs are 40-char lowercase hex.
	for i, p := range packs {
		if !validPackID(p) {
			return nil, fmt.Errorf("%w: pack table entry %d %q is not 40-char lowercase hex",
				ErrCorrupt, i, p)
		}
	}
	records := all[headerSize : headerSize+count*recordSize]
	// Sanity: records sorted ascending; reject duplicates.
	for i := 1; i < int(count); i++ {
		prev := records[(i-1)*recordSize : (i-1)*recordSize+20]
		cur := records[i*recordSize : i*recordSize+20]
		if bytes.Compare(prev, cur) >= 0 {
			return nil, fmt.Errorf("%w: records not sorted at %d", ErrCorrupt, i)
		}
	}
	// Validate every record's pack_idx is within the pack table.
	for i := 0; i < int(count); i++ {
		idx := binary.BigEndian.Uint16(records[i*recordSize+20 : i*recordSize+22])
		if int(idx) >= int(nPacks) {
			return nil, fmt.Errorf("%w: record %d pack_idx %d >= nPacks %d",
				ErrCorrupt, i, idx, nPacks)
		}
	}
	return &Map{count: int(count), records: records, packTable: packs}, nil
}

// getReader fetches an object from store as an io.ReadCloser.
func getReader(ctx context.Context, store storage.ObjectStore, key string) (io.ReadCloser, error) {
	obj, err := store.Get(ctx, key, nil)
	if err != nil {
		return nil, err
	}
	return obj.Body, nil
}

// Count returns the number of indexed objects.
func (m *Map) Count() int { return m.count }

// Lookup returns (packID, offset, ok) for oid.
func (m *Map) Lookup(oid pack.OID) (string, uint64, bool) {
	pos := sort.Search(m.count, func(i int) bool {
		rec := m.records[i*recordSize : i*recordSize+20]
		return bytes.Compare(rec, oid[:]) >= 0
	})
	if pos == m.count {
		return "", 0, false
	}
	rec := m.records[pos*recordSize : (pos+1)*recordSize]
	if !bytes.Equal(rec[:20], oid[:]) {
		return "", 0, false
	}
	idx := binary.BigEndian.Uint16(rec[20:22])
	off := binary.BigEndian.Uint64(rec[24:32])
	if int(idx) >= len(m.packTable) {
		return "", 0, false
	}
	return m.packTable[idx], off, true
}
