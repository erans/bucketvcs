package objindex

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/bucketvcs/bucketvcs/internal/pack"
)

// Build produces .bvom bytes from packReader's idx and the given pack ID.
// All entries are emitted with pack_idx=0 (M2 has one pack per repo).
func Build(packReader *pack.Reader, packID string) ([]byte, error) {
	if !validPackID(packID) {
		return nil, fmt.Errorf("objindex: packID must be 40-char lowercase hex (got %q)", packID)
	}
	idx := packReader.Idx()
	entries := make([]Entry, 0, idx.Count())
	for i := 0; i < idx.Count(); i++ {
		entries = append(entries, Entry{
			OID: idx.OIDAt(i), PackID: packID, Offset: idx.OffsetAt(i),
		})
	}
	return build(entries)
}

func build(entries []Entry) ([]byte, error) {
	// Build a sorted, deduplicated pack-id table for deterministic
	// output regardless of caller input order.
	seen := make(map[string]struct{})
	for _, e := range entries {
		if !validPackID(e.PackID) {
			return nil, fmt.Errorf("objindex: invalid pack_id %q (want 40-char lowercase hex)", e.PackID)
		}
		seen[e.PackID] = struct{}{}
	}
	packTable := make([]string, 0, len(seen))
	for pid := range seen {
		packTable = append(packTable, pid)
	}
	sort.Strings(packTable)
	if len(packTable) > 0xffff {
		return nil, fmt.Errorf("objindex: too many distinct packs (%d)", len(packTable))
	}
	idOf := make(map[string]uint16, len(packTable))
	for i, pid := range packTable {
		idOf[pid] = uint16(i)
	}
	// Sort entries by OID.
	sort.Slice(entries, func(i, j int) bool {
		return bytes.Compare(entries[i].OID[:], entries[j].OID[:]) < 0
	})
	// Detect duplicates.
	for i := 1; i < len(entries); i++ {
		if entries[i].OID == entries[i-1].OID {
			return nil, fmt.Errorf("objindex: duplicate OID %s", entries[i].OID)
		}
	}

	count := uint64(len(entries))
	packTblOff := uint64(headerSize) + count*recordSize

	var buf bytes.Buffer
	buf.Grow(int(packTblOff) + 2 + len(packTable)*packIDSize + trailerSize)

	// Header.
	buf.Write(magic)
	_ = binary.Write(&buf, binary.BigEndian, currentVer)
	_ = binary.Write(&buf, binary.BigEndian, count)
	_ = binary.Write(&buf, binary.BigEndian, packTblOff)
	buf.Write(make([]byte, 8)) // reserved

	// Records.
	rec := make([]byte, recordSize)
	for _, e := range entries {
		copy(rec[:20], e.OID[:])
		binary.BigEndian.PutUint16(rec[20:22], idOf[e.PackID])
		rec[22] = 0
		rec[23] = 0
		binary.BigEndian.PutUint64(rec[24:32], e.Offset)
		buf.Write(rec)
	}

	// Pack-id table.
	_ = binary.Write(&buf, binary.BigEndian, uint16(len(packTable)))
	for _, id := range packTable {
		buf.WriteString(id)
	}

	// Trailer = SHA-256 over everything so far.
	sum := sha256.Sum256(buf.Bytes())
	buf.Write(sum[:])

	return buf.Bytes(), nil
}
