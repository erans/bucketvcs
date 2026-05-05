package pack

import (
	"bytes"
	"crypto/sha1"
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
	if int64(baseSize) != int64(len(base)) {
		return nil, fmt.Errorf("delta: declared base size %d != actual %d", baseSize, len(base))
	}
	resultSize, err := readSizeVarint(r)
	if err != nil {
		return nil, fmt.Errorf("delta: read result size: %w", err)
	}
	out := make([]byte, 0, resultSize)
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
			out = append(out, base[off:off+sz]...)
		case op == 0:
			return nil, fmt.Errorf("delta: reserved opcode 0")
		default:
			// INSERT N literal bytes.
			n := int(op & 0x7f)
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
		v |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return v, nil
		}
		shift += 7
		if shift > 63 {
			return 0, fmt.Errorf("pack: size varint overflow")
		}
	}
}

// resolveObject reads, decompresses, and (recursively) un-deltas the
// object at off in the pack. maxDepth bounds the chain length.
func resolveObject(r io.ReaderAt, idx *Idx, off uint64, maxDepth int) (*Object, error) {
	if maxDepth <= 0 {
		return nil, ErrDeltaChainTooDeep
	}
	hdr, err := readObjectHeader(r, int64(off))
	if err != nil {
		return nil, err
	}
	switch hdr.Type {
	case TypeCommit, TypeTree, TypeBlob, TypeTag:
		body, err := inflateAt(r, int64(off)+hdr.HeaderLen, hdr.Size)
		if err != nil {
			return nil, err
		}
		return &Object{Type: hdr.Type, Size: int64(len(body)), Data: body}, nil
	case typeOFSDelta:
		base, err := resolveObject(r, idx, uint64(hdr.BaseOffset), maxDepth-1)
		if err != nil {
			return nil, err
		}
		deltaBody, err := inflateAt(r, int64(off)+hdr.HeaderLen, hdr.Size)
		if err != nil {
			return nil, err
		}
		out, err := applyDelta(base.Data, deltaBody)
		if err != nil {
			return nil, err
		}
		return &Object{Type: base.Type, Size: int64(len(out)), Data: out}, nil
	case typeREFDelta:
		baseOff, ok := idx.Lookup(hdr.BaseOID)
		if !ok {
			return nil, fmt.Errorf("%w: ref-delta base %s not in pack", ErrPackCorrupt, hdr.BaseOID)
		}
		base, err := resolveObject(r, idx, baseOff, maxDepth-1)
		if err != nil {
			return nil, err
		}
		deltaBody, err := inflateAt(r, int64(off)+hdr.HeaderLen, hdr.Size)
		if err != nil {
			return nil, err
		}
		out, err := applyDelta(base.Data, deltaBody)
		if err != nil {
			return nil, err
		}
		return &Object{Type: base.Type, Size: int64(len(out)), Data: out}, nil
	default:
		return nil, fmt.Errorf("%w: bad type %v", ErrPackCorrupt, hdr.Type)
	}
}

// hashObject returns the SHA-1 of (typeName SP size NUL body).
// Used by tests to verify resolveObject correctness.
func hashObject(typ ObjectType, body []byte) OID {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d", typ.String(), len(body))
	h.Write([]byte{0})
	h.Write(body)
	var o OID
	copy(o[:], h.Sum(nil))
	return o
}
