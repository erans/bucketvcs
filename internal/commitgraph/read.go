package commitgraph

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

const maxIndexSize = int64(1 << 30) // 1 GiB

// readBounded reads from an ObjectStore object up to maxIndexSize bytes,
// returning ErrCorrupt if the source exceeds the cap (which signals a
// crafted oversize manifest reference rather than a legitimate index).
func readBounded(ctx context.Context, store storage.ObjectStore, key string) ([]byte, error) {
	obj, err := store.Get(ctx, key, nil)
	if err != nil {
		return nil, fmt.Errorf("commitgraph: get %s: %w", key, err)
	}
	defer obj.Body.Close()
	all, err := io.ReadAll(io.LimitReader(obj.Body, maxIndexSize+1))
	if err != nil {
		return nil, fmt.Errorf("commitgraph: read %s: %w", key, err)
	}
	if int64(len(all)) > maxIndexSize {
		return nil, fmt.Errorf("%w: index size > %d bytes", ErrCorrupt, maxIndexSize)
	}
	return all, nil
}

// Graph holds a parsed .bvcg in memory. Parents lookup is O(log n)
// via binary search on the sorted-by-OID commit records.
type Graph struct {
	commits       []byte // raw commit records section
	commitOffsets []int  // byte offset of each record within commits
	tips          []Tip
}

// Open reads the entire .bvcg from store, validates trailer, parses.
func Open(ctx context.Context, store storage.ObjectStore, key string) (*Graph, error) {
	all, err := readBounded(ctx, store, key)
	if err != nil {
		return nil, err
	}
	if len(all) < headerSize+trailerSize {
		return nil, fmt.Errorf("%w: too small (%d bytes)", ErrCorrupt, len(all))
	}
	want := sha256.Sum256(all[:len(all)-trailerSize])
	if !bytes.Equal(want[:], all[len(all)-trailerSize:]) {
		return nil, fmt.Errorf("%w: trailer hash mismatch", ErrCorrupt)
	}
	if !bytes.Equal(all[:4], magic) {
		return nil, fmt.Errorf("%w: magic %x", ErrCorrupt, all[:4])
	}
	if v := binary.BigEndian.Uint32(all[4:8]); v != currentVer {
		return nil, fmt.Errorf("%w: version %d", ErrCorrupt, v)
	}
	nCommits := binary.BigEndian.Uint64(all[8:16])
	nTips := binary.BigEndian.Uint32(all[16:20])

	if nCommits > uint64(math.MaxInt32) {
		return nil, fmt.Errorf("%w: n_commits %d exceeds MaxInt32", ErrCorrupt, nCommits)
	}
	// Bound nCommits against the section's minimum-record capacity.
	// Each commit record is at least 21 bytes (20-byte OID + 1-byte n_parents + 0 parents).
	commitsSection := int64(len(all)) - int64(headerSize) - int64(nTips)*int64(tipSize) - int64(trailerSize)
	if commitsSection < 0 {
		return nil, fmt.Errorf("%w: tips overflow header+trailer space", ErrCorrupt)
	}
	const minRecordBytes = 21 // oid(20) + n_parents(1) + 0 parents
	maxCommits := uint64(commitsSection / minRecordBytes)
	if nCommits > maxCommits {
		return nil, fmt.Errorf("%w: n_commits %d exceeds file capacity %d", ErrCorrupt, nCommits, maxCommits)
	}
	if int64(nTips)*int64(tipSize) > int64(len(all))-int64(headerSize)-int64(trailerSize) {
		return nil, fmt.Errorf("%w: n_tips %d exceeds file capacity", ErrCorrupt, nTips)
	}

	tipsStart := headerSize
	tipsEnd := tipsStart + int(nTips)*tipSize
	if tipsEnd > len(all)-trailerSize {
		return nil, fmt.Errorf("%w: tips overflow", ErrCorrupt)
	}
	tipsBuf := all[tipsStart:tipsEnd]

	commitsStart := tipsEnd
	commitOffsets, commitsBytes, err := scanCommits(all[commitsStart:len(all)-trailerSize], int(nCommits))
	if err != nil {
		return nil, err
	}

	stringTable := all[commitsStart+commitsBytes : len(all)-trailerSize]

	tips := make([]Tip, 0, nTips)
	for i := 0; i < int(nTips); i++ {
		off := binary.BigEndian.Uint32(tipsBuf[i*tipSize : i*tipSize+4])
		var oid pack.OID
		copy(oid[:], tipsBuf[i*tipSize+4:i*tipSize+24])
		ref, err := readCString(stringTable, int(off))
		if err != nil {
			return nil, fmt.Errorf("%w: tip ref: %v", ErrCorrupt, err)
		}
		tips = append(tips, Tip{Ref: ref, OID: oid})
	}

	// Build commit OID set from parsed records.
	commitSet := make(map[pack.OID]struct{}, len(commitOffsets))
	for _, ofs := range commitOffsets {
		var coid pack.OID
		copy(coid[:], all[commitsStart+ofs:commitsStart+ofs+20])
		commitSet[coid] = struct{}{}
	}
	// Validate tip OIDs.
	for _, t := range tips {
		if _, ok := commitSet[t.OID]; !ok {
			return nil, fmt.Errorf("%w: tip %s -> %s not in commit set",
				ErrCorrupt, t.Ref, t.OID)
		}
	}
	// Validate parent OIDs.
	for i, ofs := range commitOffsets {
		nParents := int(all[commitsStart+ofs+20])
		for p := 0; p < nParents; p++ {
			var poid pack.OID
			copy(poid[:], all[commitsStart+ofs+21+p*20:commitsStart+ofs+21+(p+1)*20])
			if _, ok := commitSet[poid]; !ok {
				return nil, fmt.Errorf("%w: commit at index %d has dangling parent %s",
					ErrCorrupt, i, poid)
			}
		}
	}

	return &Graph{
		commits:       all[commitsStart : commitsStart+commitsBytes],
		commitOffsets: commitOffsets,
		tips:          tips,
	}, nil
}

func scanCommits(buf []byte, nCommits int) (offsets []int, totalBytes int, err error) {
	offsets = make([]int, 0, nCommits)
	pos := 0
	for i := 0; i < nCommits; i++ {
		offsets = append(offsets, pos)
		if pos+21 > len(buf) {
			return nil, 0, fmt.Errorf("%w: commit record %d truncated", ErrCorrupt, i)
		}
		nParents := int(buf[pos+20])
		recLen := 20 + 1 + nParents*20
		if pos+recLen > len(buf) {
			return nil, 0, fmt.Errorf("%w: commit record %d parents truncated", ErrCorrupt, i)
		}
		if i > 0 {
			prev := buf[offsets[i-1] : offsets[i-1]+20]
			cur := buf[pos : pos+20]
			if bytes.Compare(prev, cur) >= 0 {
				return nil, 0, fmt.Errorf("%w: commits not sorted at %d", ErrCorrupt, i)
			}
		}
		pos += recLen
	}
	return offsets, pos, nil
}

func readCString(buf []byte, off int) (string, error) {
	if off < 0 || off >= len(buf) {
		return "", fmt.Errorf("offset %d out of range %d", off, len(buf))
	}
	end := bytes.IndexByte(buf[off:], 0)
	if end < 0 {
		return "", fmt.Errorf("unterminated string at %d", off)
	}
	return string(buf[off : off+end]), nil
}

// Parents returns the parent OIDs of the given commit, in commit order.
// Returns (nil, false) if the commit is not in the graph.
func (g *Graph) Parents(oid pack.OID) ([]pack.OID, bool) {
	pos := sort.Search(len(g.commitOffsets), func(i int) bool {
		off := g.commitOffsets[i]
		return bytes.Compare(g.commits[off:off+20], oid[:]) >= 0
	})
	if pos == len(g.commitOffsets) {
		return nil, false
	}
	off := g.commitOffsets[pos]
	if !bytes.Equal(g.commits[off:off+20], oid[:]) {
		return nil, false
	}
	n := int(g.commits[off+20])
	parents := make([]pack.OID, n)
	for i := 0; i < n; i++ {
		copy(parents[i][:], g.commits[off+21+i*20:off+21+(i+1)*20])
	}
	return parents, true
}

// Tips returns a copy of the registered (ref, oid) tips.
func (g *Graph) Tips() []Tip {
	out := make([]Tip, len(g.tips))
	copy(out, g.tips)
	return out
}
