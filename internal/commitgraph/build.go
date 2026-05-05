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
func Build(packReader *pack.Reader, tips []Tip) ([]byte, error) {
	var commits []Record
	if err := packReader.ForEach(func(oid pack.OID, off uint64) error {
		obj, err := packReader.Get(context.Background(), oid)
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
	return build(commits, tips)
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

// build emits the .bvcg bytes for already-extracted commit records and tips.
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
	buf.Grow(headerSize + len(tips)*tipSize + len(commits)*40 + stringTable.Len() + trailerSize)

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
