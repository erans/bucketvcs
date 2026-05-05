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
	Type      ObjectType
	Size      int64
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
		size |= int64(b[0]&0x7f) << shift
		shift += 7
		if shift > 63 {
			return hdr, fmt.Errorf("%w: size overflow", ErrPackCorrupt)
		}
	}
	hdr.Size = size

	switch hdr.Type {
	case typeOFSDelta:
		// Big-endian-ish "offset varint" per pack-format §DELTA-OFFSET.
		if _, err := r.ReadAt(b[:], off+read); err != nil {
			return hdr, fmt.Errorf("%w: read ofs varint: %v", ErrPackCorrupt, err)
		}
		read++
		negOff := int64(b[0] & 0x7f)
		for b[0]&0x80 != 0 {
			negOff++ // implicit +1 between continuation bytes
			negOff <<= 7
			if _, err := r.ReadAt(b[:], off+read); err != nil {
				return hdr, fmt.Errorf("%w: read ofs varint cont: %v", ErrPackCorrupt, err)
			}
			read++
			negOff |= int64(b[0] & 0x7f)
			if negOff < 0 {
				return hdr, fmt.Errorf("%w: ofs varint overflow", ErrPackCorrupt)
			}
		}
		hdr.BaseOffset = off - negOff
		if hdr.BaseOffset < 0 {
			return hdr, fmt.Errorf("%w: ofs base before pack start", ErrPackCorrupt)
		}
	case typeREFDelta:
		var oidBuf [20]byte
		if _, err := r.ReadAt(oidBuf[:], off+read); err != nil {
			return hdr, fmt.Errorf("%w: read ref-delta oid: %v", ErrPackCorrupt, err)
		}
		read += 20
		copy(hdr.BaseOID[:], oidBuf[:])
	}
	hdr.HeaderLen = read
	return hdr, nil
}

// inflateAt zlib-inflates exactly want bytes from the given offset.
func inflateAt(r io.ReaderAt, off int64, want int64) ([]byte, error) {
	// We don't know the compressed size, so wrap the ReaderAt in a section
	// reader that extends to EOF; zlib stops at the first stream end.
	const slack = int64(1 << 30)
	sr := io.NewSectionReader(r, off, slack)
	zr, err := zlib.NewReader(sr)
	if err != nil {
		return nil, fmt.Errorf("%w: zlib: %v", ErrPackCorrupt, err)
	}
	defer zr.Close()
	out := bytes.NewBuffer(make([]byte, 0, want))
	if _, err := io.CopyN(out, zr, want); err != nil {
		return nil, fmt.Errorf("%w: inflate copy: %v", ErrPackCorrupt, err)
	}
	if out.Len() != int(want) {
		return nil, fmt.Errorf("%w: inflated %d, want %d", ErrPackCorrupt, out.Len(), want)
	}
	return out.Bytes(), nil
}
