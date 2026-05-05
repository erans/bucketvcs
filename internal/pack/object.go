package pack

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
)

// ObjectHeader describes a pack-encoded object's type, size, and the
// number of bytes consumed by the variable-length header itself (so the
// caller knows where the zlib payload begins).
type ObjectHeader struct {
	Type ObjectType
	Size int64
	// HeaderLen is the number of bytes consumed by the variable-length header
	// itself, including OFS_DELTA negOff varint or REF_DELTA OID.
	HeaderLen int64
	// For ofs_delta: BaseOffset is set to the absolute pack offset of
	// the base object. For ref_delta: BaseOID is set.
	BaseOffset int64
	BaseOID    OID
}

// ErrPackCorrupt is returned when a pack file fails structural checks.
var ErrPackCorrupt = errors.New("pack: pack corrupt")

// readObjectHeader parses the variable-length header at the given pack
// offset. The encoding is documented at pack-format §HEADER:
//
//	byte 0: MSB | typ(3) | size_low(4)
//	while MSB set: byte n: MSB | size_extra(7)
//
// For ofs_delta types: a "base offset" big-endian-ish varint follows
// the type+size header; the base lives at this_offset - base_offset.
// For ref_delta types: a 20-byte OID follows.
func readObjectHeader(r io.ReaderAt, off int64) (ObjectHeader, error) {
	var hdr ObjectHeader
	var b [1]byte
	read := int64(0)
	if _, err := r.ReadAt(b[:], off+read); err != nil {
		return hdr, fmt.Errorf("%w: read first header byte: %v", ErrPackCorrupt, err)
	}
	read++
	hdr.Type = ObjectType((b[0] >> 4) & 0x07)
	size := int64(b[0] & 0x0f)
	shift := uint(4)
	for b[0]&0x80 != 0 {
		if _, err := r.ReadAt(b[:], off+read); err != nil {
			return hdr, fmt.Errorf("%w: read header continuation: %v", ErrPackCorrupt, err)
		}
		read++
		payload := int64(b[0] & 0x7f)
		// Guard before the shift: any payload bit landing at/past bit 63
		// would overflow int64 and could silently flip sign.
		if shift >= 63 || (shift > 56 && payload>>(63-shift) != 0) {
			return hdr, fmt.Errorf("%w: size varint overflow", ErrPackCorrupt)
		}
		size |= payload << shift
		shift += 7
	}
	hdr.Size = size
	if hdr.Size < 0 {
		return hdr, fmt.Errorf("%w: size varint produced negative value", ErrPackCorrupt)
	}

	switch hdr.Type {
	case typeOFSDelta:
		// Big-endian-ish "offset varint" per pack-format §DELTA-OFFSET.
		if _, err := r.ReadAt(b[:], off+read); err != nil {
			return hdr, fmt.Errorf("%w: read ofs varint: %v", ErrPackCorrupt, err)
		}
		read++
		// Decode into uint64 to detect overflow before any conversion.
		negOff := uint64(b[0] & 0x7f)
		const maxNegOff = uint64(1) << 62 // safe int64 ceiling
		for b[0]&0x80 != 0 {
			negOff++ // implicit +1 between continuation bytes
			if negOff > maxNegOff>>7 {
				return hdr, fmt.Errorf("%w: ofs varint overflow", ErrPackCorrupt)
			}
			negOff <<= 7
			if _, err := r.ReadAt(b[:], off+read); err != nil {
				return hdr, fmt.Errorf("%w: read ofs varint cont: %v", ErrPackCorrupt, err)
			}
			read++
			negOff |= uint64(b[0] & 0x7f)
		}
		if negOff > uint64(off) {
			return hdr, fmt.Errorf("%w: ofs base before pack start", ErrPackCorrupt)
		}
		hdr.BaseOffset = off - int64(negOff)
		// Pack header is 12 bytes; reject bases inside header or at/after
		// the current object.
		if hdr.BaseOffset < 12 || hdr.BaseOffset >= off {
			return hdr, fmt.Errorf("%w: ofs_delta base offset %d invalid (this=%d)",
				ErrPackCorrupt, hdr.BaseOffset, off)
		}
	case typeREFDelta:
		var oidBuf [20]byte
		if _, err := r.ReadAt(oidBuf[:], off+read); err != nil {
			return hdr, fmt.Errorf("%w: read ref-delta oid: %v", ErrPackCorrupt, err)
		}
		read += 20
		copy(hdr.BaseOID[:], oidBuf[:])
	case TypeCommit, TypeTree, TypeBlob, TypeTag:
		// No additional bytes after size header for non-delta types.
	default:
		return hdr, fmt.Errorf("%w: unknown object type %d", ErrPackCorrupt, hdr.Type)
	}
	hdr.HeaderLen = read
	return hdr, nil
}

// maxObjectSize is the upper bound on any single inflated object or delta
// result. 1 GiB is well above any plausible Git object; anything larger
// is treated as a corrupt-pack indicator. Used by both inflateAt and
// applyDelta so the limit is consistent across the package.
const maxObjectSize = int64(1 << 30)

// inflateAt zlib-inflates exactly want bytes from the given offset.
func inflateAt(r io.ReaderAt, off int64, want int64) ([]byte, error) {
	if want < 0 {
		return nil, fmt.Errorf("%w: negative inflate size %d", ErrPackCorrupt, want)
	}
	if want > maxObjectSize {
		return nil, fmt.Errorf("%w: inflate size %d exceeds bound %d", ErrPackCorrupt, want, maxObjectSize)
	}
	// We don't know the compressed size, so wrap the ReaderAt in a section
	// reader that extends to EOF; zlib stops at the first stream end.
	// slack: cap on inflate input bytes from this offset. 1 GiB is well
	// above any plausible compressed Git object size; if a real stream
	// exceeds this, zlib will surface ErrUnexpectedEOF on Read.
	const slack = int64(1 << 30)
	sr := io.NewSectionReader(r, off, slack)
	zr, err := zlib.NewReader(sr)
	if err != nil {
		return nil, fmt.Errorf("%w: zlib: %v", ErrPackCorrupt, err)
	}
	var out bytes.Buffer
	// Note: we intentionally don't preallocate to `want`. A corrupt pack
	// could declare a huge size; bytes.Buffer grows incrementally and
	// the maxObjectSize cap (validated above) bounds the worst case.
	if _, err := io.CopyN(&out, zr, want); err != nil {
		return nil, fmt.Errorf("%w: inflate copy: %v", ErrPackCorrupt, err)
	}
	if int64(out.Len()) != want {
		return nil, fmt.Errorf("%w: inflated %d, want %d", ErrPackCorrupt, out.Len(), want)
	}
	// The zlib stream must be exactly `want` bytes long — reading one
	// more byte must hit EOF. Anything else means corruption (the pack
	// header lied about the object size).
	var probe [1]byte
	n, err := zr.Read(probe[:])
	if n > 0 {
		return nil, fmt.Errorf("%w: zlib stream emitted more than %d bytes", ErrPackCorrupt, want)
	}
	if err != io.EOF {
		return nil, fmt.Errorf("%w: zlib stream tail: %v", ErrPackCorrupt, err)
	}
	// Close validates the Adler-32 trailer.
	if err := zr.Close(); err != nil {
		return nil, fmt.Errorf("%w: zlib close: %v", ErrPackCorrupt, err)
	}
	return out.Bytes(), nil
}
