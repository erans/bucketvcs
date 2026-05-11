package commitgraph

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/bucketvcs/bucketvcs/internal/pack"
)

// Build inflates every commit in packReader, derives parents from the
// commit body, and produces .bvcg bytes paired with the given tips.
//
// Note: commits in a real pack are typically delta-encoded, so a
// type-only optimization would not help — we'd still need to resolve
// deltas to discover the resolved type. The ctx parameter is honored
// per packReader.Get call.
func Build(ctx context.Context, packReader *pack.Reader, tips []Tip) ([]byte, error) {
	var commits []Record
	if err := packReader.ForEach(func(oid pack.OID, off uint64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		obj, err := packReader.Get(ctx, oid)
		if err != nil {
			return err
		}
		if obj.Type != pack.TypeCommit {
			return nil
		}
		parents, err := parseParents(obj.Data)
		if err != nil {
			return fmt.Errorf("commit %s: %w", oid, err)
		}
		commits = append(commits, Record{OID: oid, Parents: parents})
		return nil
	}); err != nil {
		return nil, err
	}
	// Compute generation numbers and populate each record's Generation field.
	gens := computeGenerations(commits)
	for i := range commits {
		commits[i].Generation = gens[commits[i].OID]
	}

	return build(commits, tips)
}

// computeGenerations computes generation numbers for all commits in the slice.
// gen(c) = 1 + max(gen(parent) for parent in c.Parents); roots have gen = 1.
// Parents whose OID is not in the slice resolve to gen 0 (they are not in
// this pack; a full-rebuild caller would not normally have dangling parents
// because Build validates them, but the helper is defensive).
// The returned map has an entry for every OID in commits.
func computeGenerations(commits []Record) map[pack.OID]uint32 {
	// Build an index from OID to slice position for O(1) lookup.
	idx := make(map[pack.OID]int, len(commits))
	for i := range commits {
		idx[commits[i].OID] = i
	}

	gensByOID := make(map[pack.OID]uint32, len(commits))
	visiting := make(map[pack.OID]bool, len(commits))

	var compute func(oid pack.OID) uint32
	compute = func(oid pack.OID) uint32 {
		if g, ok := gensByOID[oid]; ok {
			return g
		}
		if visiting[oid] {
			// Cycle guard: treat as gen 0 if we somehow re-enter.
			return 0
		}
		visiting[oid] = true
		defer delete(visiting, oid)

		i, ok := idx[oid]
		if !ok {
			// OID not in this pack; treat as gen 0.
			gensByOID[oid] = 0
			return 0
		}
		var maxParent uint32
		for _, p := range commits[i].Parents {
			if g := compute(p); g > maxParent {
				maxParent = g
			}
		}
		g := maxParent + 1
		gensByOID[oid] = g
		return g
	}

	for i := range commits {
		compute(commits[i].OID)
	}
	return gensByOID
}

// parseParents extracts parent OIDs from a commit body. Header lines:
//
//	tree <hex>\n
//	parent <hex>\n   (zero or more, immediately following tree)
//	author ...
//
// We scan until we hit the first non-tree-non-parent line.
func parseParents(body []byte) ([]pack.OID, error) {
	var parents []pack.OID
	for len(body) > 0 {
		nl := bytes.IndexByte(body, '\n')
		if nl < 0 {
			return parents, nil
		}
		line := body[:nl]
		body = body[nl+1:]
		switch {
		case bytes.HasPrefix(line, []byte("tree ")):
			continue
		case bytes.HasPrefix(line, []byte("parent ")):
			hex := string(line[len("parent "):])
			oid, err := pack.ParseOID(hex)
			if err != nil {
				return nil, fmt.Errorf("parse parent %q: %w", hex, err)
			}
			parents = append(parents, oid)
		default:
			return parents, nil
		}
	}
	return parents, nil
}

// build serializes the commit records and tips into the .bvcg binary format.
func build(commits []Record, tips []Tip) ([]byte, error) {
	// Sort by OID and reject duplicates.
	sort.Slice(commits, func(i, j int) bool {
		return bytes.Compare(commits[i].OID[:], commits[j].OID[:]) < 0
	})
	for i := 1; i < len(commits); i++ {
		if commits[i].OID == commits[i-1].OID {
			return nil, fmt.Errorf("commitgraph: duplicate commit %s", commits[i].OID)
		}
	}
	for _, c := range commits {
		if len(c.Parents) > maxParents {
			return nil, fmt.Errorf("commitgraph: %s has %d parents (max %d)",
				c.OID, len(c.Parents), maxParents)
		}
	}

	// Validate tips: every tip's OID must be in the commit set.
	commitSet := make(map[pack.OID]struct{}, len(commits))
	for _, c := range commits {
		commitSet[c.OID] = struct{}{}
	}
	for _, t := range tips {
		if _, ok := commitSet[t.OID]; !ok {
			return nil, fmt.Errorf("commitgraph: tip %s -> %s not in commit set",
				t.Ref, t.OID)
		}
	}

	// Validate parent edges: every parent OID must be a known commit.
	for _, c := range commits {
		for _, p := range c.Parents {
			if _, ok := commitSet[p]; !ok {
				return nil, fmt.Errorf("commitgraph: commit %s has dangling parent %s",
					c.OID, p)
			}
		}
	}

	// Sort tips by (ref, oid) for determinism before building the string
	// table, so that table layout is independent of the caller's tip order.
	sortedTips := make([]Tip, len(tips))
	copy(sortedTips, tips)
	sort.Slice(sortedTips, func(i, j int) bool {
		if sortedTips[i].Ref != sortedTips[j].Ref {
			return sortedTips[i].Ref < sortedTips[j].Ref
		}
		return bytes.Compare(sortedTips[i].OID[:], sortedTips[j].OID[:]) < 0
	})

	// Build string table for tip ref names from sortedTips. Dedup so the
	// same ref name appearing in multiple tip records points at one offset.
	stringOffset := make(map[string]uint32)
	var stringTable bytes.Buffer
	for _, t := range sortedTips {
		if _, ok := stringOffset[t.Ref]; ok {
			continue
		}
		stringOffset[t.Ref] = uint32(stringTable.Len())
		stringTable.WriteString(t.Ref)
		stringTable.WriteByte(0)
	}

	var buf bytes.Buffer
	buf.Grow(headerSize + len(tips)*tipSize + len(commits)*45 + stringTable.Len() + trailerSize)

	// Header.
	buf.Write(magic)
	_ = binary.Write(&buf, binary.BigEndian, currentVer)
	_ = binary.Write(&buf, binary.BigEndian, uint64(len(commits)))
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(sortedTips)))
	buf.Write(make([]byte, 12)) // reserved

	// Tips.
	for _, tt := range sortedTips {
		off, ok := stringOffset[tt.Ref]
		if !ok {
			return nil, fmt.Errorf("commitgraph: tip ref %q missing from string table", tt.Ref)
		}
		_ = binary.Write(&buf, binary.BigEndian, off)
		buf.Write(tt.OID[:])
	}

	// Commit records.
	for _, c := range commits {
		buf.Write(c.OID[:])
		_ = binary.Write(&buf, binary.LittleEndian, c.Generation) // v2: gen u32 LE
		buf.WriteByte(byte(len(c.Parents)))
		for _, p := range c.Parents {
			buf.Write(p[:])
		}
	}

	// String table.
	buf.Write(stringTable.Bytes())

	// Trailer.
	sum := sha256.Sum256(buf.Bytes())
	buf.Write(sum[:])

	return buf.Bytes(), nil
}
