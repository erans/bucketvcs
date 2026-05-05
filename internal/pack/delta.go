package pack

import (
	"bytes"
	"errors"
	"fmt"
	"io"
)

// ErrDeltaChainTooDeep is returned when a delta resolution exceeds the
// configured chain bound.
var ErrDeltaChainTooDeep = errors.New("pack: delta chain too deep")

// applyDelta applies a Git delta-encoded byte sequence against a base
// object body, returning the reconstructed result.
//
// Format (pack-format §OBJECT-DATA-DELTIFIED):
//
//	base_size  -- varint, LSB 7 bits + MSB continuation
//	result_size -- same encoding
//	instructions ...
//	  0x80 | mask: COPY
//	    low 4 bits select which of off1..off4 follow (LE)
//	    next 3 bits select which of sz1..sz3 follow (LE)
//	    size 0 means 0x10000 (Git's quirk)
//	  0x01..0x7f: INSERT (next N literal bytes)
//	  0x00: reserved
func applyDelta(base, delta []byte) ([]byte, error) {
	r := bytes.NewReader(delta)
	baseSize, err := readSizeVarint(r)
	if err != nil {
		return nil, fmt.Errorf("delta: read base size: %w", err)
	}
	const maxDeltaResult = uint64(maxObjectSize) // 1 GiB; matches inflateAt bound.
	if baseSize > maxDeltaResult {
		return nil, fmt.Errorf("delta: declared base size %d exceeds bound %d", baseSize, maxDeltaResult)
	}
	if int64(baseSize) != int64(len(base)) {
		return nil, fmt.Errorf("delta: declared base size %d != actual %d", baseSize, len(base))
	}
	resultSize, err := readSizeVarint(r)
	if err != nil {
		return nil, fmt.Errorf("delta: read result size: %w", err)
	}
	if resultSize > maxDeltaResult {
		return nil, fmt.Errorf("delta: declared result size %d exceeds bound %d", resultSize, maxDeltaResult)
	}
	var out []byte
	// Don't preallocate to declared resultSize; a corrupt delta could
	// claim 1 GiB. Grow incrementally; per-instruction bounds and the
	// final size check enforce the contract.
	for {
		op, err := r.ReadByte()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("delta: read op: %w", err)
		}
		switch {
		case op&0x80 != 0:
			// COPY: assemble offset (4 bytes LE) and size (3 bytes LE).
			var off uint32
			for i := uint(0); i < 4; i++ {
				if op&(1<<i) != 0 {
					b, err := r.ReadByte()
					if err != nil {
						return nil, fmt.Errorf("delta: copy off byte: %w", err)
					}
					off |= uint32(b) << (8 * i)
				}
			}
			var sz uint32
			for i := uint(0); i < 3; i++ {
				if op&(0x10<<i) != 0 {
					b, err := r.ReadByte()
					if err != nil {
						return nil, fmt.Errorf("delta: copy sz byte: %w", err)
					}
					sz |= uint32(b) << (8 * i)
				}
			}
			// Git quirk: sz == 0 means 0x10000.
			if sz == 0 {
				sz = 0x10000
			}
			if int64(off)+int64(sz) > int64(len(base)) {
				return nil, fmt.Errorf("delta: copy out of range off=%d sz=%d base=%d",
					off, sz, len(base))
			}
			if uint64(len(out))+uint64(sz) > resultSize {
				return nil, fmt.Errorf("delta: copy at pos=%d sz=%d would exceed result_size=%d",
					len(out), sz, resultSize)
			}
			out = append(out, base[off:off+sz]...)
		case op == 0:
			return nil, fmt.Errorf("delta: reserved opcode 0")
		default:
			// INSERT N literal bytes.
			n := int(op & 0x7f)
			if uint64(len(out))+uint64(n) > resultSize {
				return nil, fmt.Errorf("delta: insert n=%d at pos=%d would exceed result_size=%d",
					n, len(out), resultSize)
			}
			buf := make([]byte, n)
			if _, err := io.ReadFull(r, buf); err != nil {
				return nil, fmt.Errorf("delta: insert read: %w", err)
			}
			out = append(out, buf...)
		}
	}
	if int64(len(out)) != int64(resultSize) {
		return nil, fmt.Errorf("delta: result size %d != declared %d", len(out), resultSize)
	}
	return out, nil
}

// readSizeVarint reads the size-encoded varint used by the delta format
// (LSB-first, 7 bits per byte, MSB=continuation).
func readSizeVarint(r io.ByteReader) (uint64, error) {
	var v uint64
	var shift uint
	for {
		b, err := r.ReadByte()
		if err != nil {
			return 0, fmt.Errorf("pack: read size varint: %w", err)
		}
		// 7 bits of payload at position [shift, shift+7). Reject if any
		// payload bit would land past bit 63 of v.
		payload := uint64(b & 0x7f)
		if shift >= 64 || (shift > 57 && payload>>(64-shift) != 0) {
			return 0, fmt.Errorf("pack: size varint overflow")
		}
		v |= payload << shift
		if b&0x80 == 0 {
			return v, nil
		}
		shift += 7
	}
}

// resolveObject reads, decompresses, and (recursively) un-deltas the
// object at off in the pack. maxDepth bounds the chain length; visited
// detects cycles so that pathological packs surface as ErrPackCorrupt
// rather than ErrDeltaChainTooDeep.
func resolveObject(r io.ReaderAt, idx *Idx, off uint64, maxDepth int) (*Object, error) {
	return resolveObjectRec(r, idx, off, maxDepth, nil)
}

func resolveObjectRec(r io.ReaderAt, idx *Idx, off uint64, maxDepth int, visited map[uint64]struct{}) (*Object, error) {
	if _, seen := visited[off]; seen {
		return nil, fmt.Errorf("%w: delta cycle at offset %d", ErrPackCorrupt, off)
	}
	hdr, err := readObjectHeader(r, int64(off))
	if err != nil {
		return nil, err
	}
	switch hdr.Type {
	case TypeCommit, TypeTree, TypeBlob, TypeTag:
		// Non-delta bases never consume depth budget.
		body, err := inflateAt(r, int64(off)+hdr.HeaderLen, hdr.Size)
		if err != nil {
			return nil, err
		}
		return &Object{Type: hdr.Type, Size: int64(len(body)), Data: body}, nil
	case typeOFSDelta:
		// Each delta hop consumes one unit of depth budget.
		if maxDepth <= 0 {
			return nil, ErrDeltaChainTooDeep
		}
		if !idx.HasOffset(uint64(hdr.BaseOffset)) {
			return nil, fmt.Errorf("%w: ofs-delta base offset %d not an object in idx",
				ErrPackCorrupt, hdr.BaseOffset)
		}
		// Lazily allocate the visited map only when we recurse.
		if visited == nil {
			visited = make(map[uint64]struct{})
		}
		visited[off] = struct{}{}
		base, err := resolveObjectRec(r, idx, uint64(hdr.BaseOffset), maxDepth-1, visited)
		if err != nil {
			return nil, err
		}
		deltaBody, err := inflateAt(r, int64(off)+hdr.HeaderLen, hdr.Size)
		if err != nil {
			return nil, err
		}
		out, err := applyDelta(base.Data, deltaBody)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrPackCorrupt, err)
		}
		return &Object{Type: base.Type, Size: int64(len(out)), Data: out}, nil
	case typeREFDelta:
		// Each delta hop consumes one unit of depth budget.
		if maxDepth <= 0 {
			return nil, ErrDeltaChainTooDeep
		}
		baseOff, ok := idx.Lookup(hdr.BaseOID)
		if !ok {
			return nil, fmt.Errorf("%w: ref-delta base %s not in pack", ErrPackCorrupt, hdr.BaseOID)
		}
		if baseOff == off {
			return nil, fmt.Errorf("%w: ref-delta self-reference at offset %d", ErrPackCorrupt, off)
		}
		if visited == nil {
			visited = make(map[uint64]struct{})
		}
		visited[off] = struct{}{}
		base, err := resolveObjectRec(r, idx, baseOff, maxDepth-1, visited)
		if err != nil {
			return nil, err
		}
		deltaBody, err := inflateAt(r, int64(off)+hdr.HeaderLen, hdr.Size)
		if err != nil {
			return nil, err
		}
		out, err := applyDelta(base.Data, deltaBody)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrPackCorrupt, err)
		}
		return &Object{Type: base.Type, Size: int64(len(out)), Data: out}, nil
	default:
		return nil, fmt.Errorf("%w: bad type %v", ErrPackCorrupt, hdr.Type)
	}
}
