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
	version       uint32 // VersionV1 or VersionV2
}

// OpenFromStore reads the entire .bvcg from store, validates trailer, parses.
func OpenFromStore(ctx context.Context, store storage.ObjectStore, key string) (*Graph, error) {
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
	ver := binary.BigEndian.Uint32(all[4:8])
	if ver != VersionV1 && ver != VersionV2 {
		return nil, fmt.Errorf("%w: version %d", ErrCorrupt, ver)
	}
	nCommits := binary.BigEndian.Uint64(all[8:16])
	nTips := binary.BigEndian.Uint32(all[16:20])

	if nCommits > uint64(math.MaxInt32) {
		return nil, fmt.Errorf("%w: n_commits %d exceeds MaxInt32", ErrCorrupt, nCommits)
	}
	// Bound nCommits against the section's minimum-record capacity.
	// v1 records: oid(20) + n_parents(1) + parents = 21 bytes minimum.
	// v2 records: oid(20) + gen(4) + n_parents(1) + parents = 25 bytes minimum.
	commitsSection := int64(len(all)) - int64(headerSize) - int64(nTips)*int64(tipSize) - int64(trailerSize)
	if commitsSection < 0 {
		return nil, fmt.Errorf("%w: tips overflow header+trailer space", ErrCorrupt)
	}
	minRecordBytes := int64(25) // v2 default
	if ver == VersionV1 {
		minRecordBytes = 21
	}
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
	commitOffsets, commitsBytes, err := scanCommits(all[commitsStart:len(all)-trailerSize], int(nCommits), ver)
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
	// v1: n_parents at offset +20 (after oid), parents start at +21.
	// v2: n_parents at offset +24 (after oid+gen), parents start at +25.
	nParentsOffset := 24
	parentsStart := 25
	if ver == VersionV1 {
		nParentsOffset = 20
		parentsStart = 21
	}
	for i, ofs := range commitOffsets {
		nParents := int(all[commitsStart+ofs+nParentsOffset])
		for p := 0; p < nParents; p++ {
			var poid pack.OID
			copy(poid[:], all[commitsStart+ofs+parentsStart+p*20:commitsStart+ofs+parentsStart+(p+1)*20])
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
		version:       ver,
	}, nil
}

func scanCommits(buf []byte, nCommits int, ver uint32) (offsets []int, totalBytes int, err error) {
	offsets = make([]int, 0, nCommits)
	pos := 0
	for i := 0; i < nCommits; i++ {
		offsets = append(offsets, pos)
		var nParents int
		var recLen int
		if ver == VersionV2 {
			// v2 record: oid(20) + gen(4) + n_parents(1) + parents[n]*20
			if pos+25 > len(buf) {
				return nil, 0, fmt.Errorf("%w: commit record %d truncated", ErrCorrupt, i)
			}
			nParents = int(buf[pos+24]) // n_parents at offset +24 (after oid+gen)
			recLen = 20 + 4 + 1 + nParents*20
		} else {
			// v1 record: oid(20) + n_parents(1) + parents[n]*20 — no generation field.
			if pos+21 > len(buf) {
				return nil, 0, fmt.Errorf("%w: commit record %d truncated", ErrCorrupt, i)
			}
			nParents = int(buf[pos+20]) // n_parents at offset +20 (after oid)
			recLen = 20 + 1 + nParents*20
		}
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

// Reader is an in-memory parsed .bvcg that provides O(1) lookup by OID.
// It accepts both v1 and v2 files. On v1 files all Generation values are 0.
type Reader struct {
	records map[pack.OID]Record
}

// Open parses a .bvcg from a raw byte slice, accepting v1 and v2.
// It validates the SHA-256 trailer, magic, and version before parsing.
func Open(bts []byte) (*Reader, error) {
	if len(bts) < headerSize+trailerSize {
		return nil, fmt.Errorf("%w: too small (%d bytes)", ErrCorrupt, len(bts))
	}
	want := sha256.Sum256(bts[:len(bts)-trailerSize])
	if !bytes.Equal(want[:], bts[len(bts)-trailerSize:]) {
		return nil, fmt.Errorf("%w: trailer hash mismatch", ErrCorrupt)
	}
	if !bytes.Equal(bts[:4], magic) {
		return nil, fmt.Errorf("%w: magic %x", ErrCorrupt, bts[:4])
	}
	ver := binary.BigEndian.Uint32(bts[4:8])
	if ver != VersionV1 && ver != VersionV2 {
		return nil, fmt.Errorf("%w: unsupported version %d", ErrCorrupt, ver)
	}
	nCommits := binary.BigEndian.Uint64(bts[8:16])
	nTips := binary.BigEndian.Uint32(bts[16:20])

	if nCommits > uint64(math.MaxInt32) {
		return nil, fmt.Errorf("%w: n_commits %d exceeds MaxInt32", ErrCorrupt, nCommits)
	}

	tipsStart := headerSize
	tipsEnd := tipsStart + int(nTips)*tipSize
	if tipsEnd > len(bts)-trailerSize {
		return nil, fmt.Errorf("%w: tips overflow", ErrCorrupt)
	}

	commitsStart := tipsEnd
	body := bts[commitsStart : len(bts)-trailerSize]

	records, err := parseRecordBytes(body, int(nCommits), ver)
	if err != nil {
		return nil, err
	}

	// Validate parent references — every parent OID must be a commit in
	// the same graph. Mirrors OpenFromStore's dangling-parent check.
	for _, rec := range records {
		for _, p := range rec.Parents {
			if _, ok := records[p]; !ok {
				return nil, fmt.Errorf("%w: commit %s has dangling parent %s", ErrCorrupt, rec.OID, p)
			}
		}
	}

	return &Reader{records: records}, nil
}

// parseRecordBytes parses nCommits commit records from buf, handling v1 and v2 layout.
func parseRecordBytes(buf []byte, nCommits int, ver uint32) (map[pack.OID]Record, error) {
	records := make(map[pack.OID]Record, nCommits)
	pos := 0
	for i := 0; i < nCommits; i++ {
		if ver == VersionV2 {
			// v2: oid(20) + gen(4) + n_parents(1) + parents[n]*20
			if pos+25 > len(buf) {
				return nil, fmt.Errorf("%w: commit record %d truncated", ErrCorrupt, i)
			}
			var oid pack.OID
			copy(oid[:], buf[pos:pos+20])
			gen := binary.LittleEndian.Uint32(buf[pos+20 : pos+24])
			nParents := int(buf[pos+24])
			recLen := 25 + nParents*20
			if pos+recLen > len(buf) {
				return nil, fmt.Errorf("%w: commit record %d parents truncated", ErrCorrupt, i)
			}
			parents := make([]pack.OID, nParents)
			for j := 0; j < nParents; j++ {
				copy(parents[j][:], buf[pos+25+j*20:pos+25+(j+1)*20])
			}
			if _, dup := records[oid]; dup {
				return nil, fmt.Errorf("%w: duplicate commit %s", ErrCorrupt, oid)
			}
			records[oid] = Record{OID: oid, Generation: gen, Parents: parents}
			pos += recLen
		} else {
			// v1: oid(20) + n_parents(1) + parents[n]*20 — no generation field.
			if pos+21 > len(buf) {
				return nil, fmt.Errorf("%w: commit record %d truncated", ErrCorrupt, i)
			}
			var oid pack.OID
			copy(oid[:], buf[pos:pos+20])
			nParents := int(buf[pos+20])
			recLen := 21 + nParents*20
			if pos+recLen > len(buf) {
				return nil, fmt.Errorf("%w: commit record %d parents truncated", ErrCorrupt, i)
			}
			parents := make([]pack.OID, nParents)
			for j := 0; j < nParents; j++ {
				copy(parents[j][:], buf[pos+21+j*20:pos+21+(j+1)*20])
			}
			if _, dup := records[oid]; dup {
				return nil, fmt.Errorf("%w: duplicate commit %s", ErrCorrupt, oid)
			}
			records[oid] = Record{OID: oid, Generation: 0, Parents: parents}
			pos += recLen
		}
	}
	return records, nil
}

// GenerationOf returns the commit-graph generation number for oid.
// On v1 files all generations are 0 and ok=true indicates the commit IS present.
// Returns (0, false) if oid isn't present.
func (r *Reader) GenerationOf(oid pack.OID) (uint32, bool) {
	rec, ok := r.records[oid]
	if !ok {
		return 0, false
	}
	return rec.Generation, true
}

// RecordOf returns the full commit record for oid.
func (r *Reader) RecordOf(oid pack.OID) (Record, bool) {
	rec, ok := r.records[oid]
	return rec, ok
}

// IterRecords calls f for each commit in the graph. The iteration
// order is undefined. Used by reachability.LoadGenLookup.
func (r *Reader) IterRecords(f func(oid pack.OID, gen uint32)) {
	for oid, rec := range r.records {
		f(oid, rec.Generation)
	}
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
	// Layout depends on version:
	// v1: oid(20) + n_parents(1) + parents[n]*20 → n_parents at +20, parents at +21.
	// v2: oid(20) + gen(4) + n_parents(1) + parents[n]*20 → n_parents at +24, parents at +25.
	nParentsOffset := 24
	parentsOffset := 25
	if g.version == VersionV1 {
		nParentsOffset = 20
		parentsOffset = 21
	}
	n := int(g.commits[off+nParentsOffset])
	parents := make([]pack.OID, n)
	for i := 0; i < n; i++ {
		copy(parents[i][:], g.commits[off+parentsOffset+i*20:off+parentsOffset+(i+1)*20])
	}
	return parents, true
}

// Tips returns a copy of the registered (ref, oid) tips.
func (g *Graph) Tips() []Tip {
	out := make([]Tip, len(g.tips))
	copy(out, g.tips)
	return out
}
