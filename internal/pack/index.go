package pack

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
)

// idx v2 magic and version bytes per pack-format.txt.
var idxMagic = []byte{0xff, 0x74, 0x4f, 0x63}

const (
	idxVersion         uint32 = 2
	idxFanoutEntries          = 256
	idxFanoutBytes            = idxFanoutEntries * 4
	idxHeaderBytes            = 8 // magic+version
	idxOIDSize                = 20
	idxCRCSize                = 4
	idxOffsetSize             = 4
	idxLargeOffsetSize        = 8
	idxTrailerSize            = 40 // pack-sha1 + idx-sha1
	idxOffsetMSB              = uint32(1) << 31
)

// ErrIdxCorrupt is returned when an .idx file fails structural checks.
var ErrIdxCorrupt = errors.New("pack: idx corrupt")

// Idx is a parsed .idx v2 file.
type Idx struct {
	count       int
	fanout      [256]uint32
	oids        []byte // count*20
	crcs        []byte // count*4 (CRC32 per object; M2 stores but does not validate against pack CRC -- M9 may)
	offsets     []byte // count*4
	largeOffs   []uint64
	packTrailer [20]byte // SHA-1 of pack file (per idx footer)
	idxSelfSHA  [20]byte
}

// ParseIdx reads a v2 .idx file from r. size must equal r's content
// length so the trailer offset is known.
func ParseIdx(r io.ReaderAt, size int64) (*Idx, error) {
	if size < int64(idxHeaderBytes+idxFanoutBytes+idxTrailerSize) {
		return nil, fmt.Errorf("%w: too small (%d)", ErrIdxCorrupt, size)
	}
	buf := make([]byte, idxHeaderBytes)
	if _, err := r.ReadAt(buf, 0); err != nil {
		return nil, fmt.Errorf("%w: read header: %v", ErrIdxCorrupt, err)
	}
	if string(buf[:4]) != string(idxMagic) {
		return nil, fmt.Errorf("%w: bad magic %x", ErrIdxCorrupt, buf[:4])
	}
	if v := binary.BigEndian.Uint32(buf[4:8]); v != idxVersion {
		return nil, fmt.Errorf("%w: unsupported version %d", ErrIdxCorrupt, v)
	}
	idx := &Idx{}
	fanoutBuf := make([]byte, idxFanoutBytes)
	if _, err := r.ReadAt(fanoutBuf, int64(idxHeaderBytes)); err != nil {
		return nil, fmt.Errorf("%w: read fanout: %v", ErrIdxCorrupt, err)
	}
	for i := 0; i < idxFanoutEntries; i++ {
		idx.fanout[i] = binary.BigEndian.Uint32(fanoutBuf[i*4:])
	}
	idx.count = int(idx.fanout[255])
	// Validate fanout monotonicity.
	for i := 1; i < idxFanoutEntries; i++ {
		if idx.fanout[i] < idx.fanout[i-1] {
			return nil, fmt.Errorf("%w: fanout non-monotonic at %d", ErrIdxCorrupt, i)
		}
	}

	// Sanity: file must be at least header + fanout + 28*count + trailer.
	// (count*20 oid + count*4 crc + count*4 offset = 28*count.)
	needed := int64(idxHeaderBytes+idxFanoutBytes) +
		int64(idx.count)*int64(idxOIDSize+idxCRCSize+idxOffsetSize) +
		int64(idxTrailerSize)
	if needed > size {
		return nil, fmt.Errorf("%w: count %d exceeds file size %d (needs ≥%d)",
			ErrIdxCorrupt, idx.count, size, needed)
	}
	off := int64(idxHeaderBytes + idxFanoutBytes)
	idx.oids = make([]byte, idx.count*idxOIDSize)
	if _, err := r.ReadAt(idx.oids, off); err != nil {
		return nil, fmt.Errorf("%w: read oid table: %v", ErrIdxCorrupt, err)
	}
	off += int64(idx.count * idxOIDSize)
	idx.crcs = make([]byte, idx.count*idxCRCSize)
	if _, err := r.ReadAt(idx.crcs, off); err != nil {
		return nil, fmt.Errorf("%w: read crc table: %v", ErrIdxCorrupt, err)
	}
	off += int64(idx.count * idxCRCSize)
	idx.offsets = make([]byte, idx.count*idxOffsetSize)
	if _, err := r.ReadAt(idx.offsets, off); err != nil {
		return nil, fmt.Errorf("%w: read offset table: %v", ErrIdxCorrupt, err)
	}
	off += int64(idx.count * idxOffsetSize)

	largeBytes := size - off - int64(idxTrailerSize)
	if largeBytes < 0 || largeBytes%int64(idxLargeOffsetSize) != 0 {
		return nil, fmt.Errorf("%w: large-offset section size %d", ErrIdxCorrupt, largeBytes)
	}
	if largeBytes > 0 {
		raw := make([]byte, largeBytes)
		if _, err := r.ReadAt(raw, off); err != nil {
			return nil, fmt.Errorf("%w: read large offsets: %v", ErrIdxCorrupt, err)
		}
		idx.largeOffs = make([]uint64, largeBytes/int64(idxLargeOffsetSize))
		for i := range idx.largeOffs {
			idx.largeOffs[i] = binary.BigEndian.Uint64(raw[i*idxLargeOffsetSize:])
		}
		off += largeBytes
	}

	// Validate every offset that points into largeOffs is in bounds.
	for k := 0; k < idx.count; k++ {
		raw := binary.BigEndian.Uint32(idx.offsets[k*idxOffsetSize:])
		if raw&idxOffsetMSB != 0 {
			li := int(raw &^ idxOffsetMSB)
			if li >= len(idx.largeOffs) {
				return nil, fmt.Errorf("%w: offset %d points to large-index %d (have %d)",
					ErrIdxCorrupt, k, li, len(idx.largeOffs))
			}
		}
	}
	trailer := make([]byte, idxTrailerSize)
	if _, err := r.ReadAt(trailer, off); err != nil {
		return nil, fmt.Errorf("%w: read trailer: %v", ErrIdxCorrupt, err)
	}
	copy(idx.packTrailer[:], trailer[:20])
	// idxSelfSHA: stored for completeness but not verified at M2. The
	// idx file is read from a content-addressed bucket adapter (M0
	// conformance) which performs its own integrity checks; verifying
	// SHA-1 here would duplicate that work. M9 may revisit if hashing
	// during read becomes valuable for diagnostic purposes.
	copy(idx.idxSelfSHA[:], trailer[20:])
	return idx, nil
}

// Count returns the number of indexed objects.
func (i *Idx) Count() int { return i.count }

// Fanout returns a copy of the 256-entry fanout table.
func (i *Idx) Fanout() [256]uint32 { return i.fanout }

// PackTrailerSHA1 returns the .pack file SHA-1 recorded in the .idx footer.
func (i *Idx) PackTrailerSHA1() [20]byte { return i.packTrailer }

// OIDAt returns the OID at the given (sorted) index position. Panics
// if i is out of range.
func (i *Idx) OIDAt(n int) OID {
	var o OID
	copy(o[:], i.oids[n*idxOIDSize:(n+1)*idxOIDSize])
	return o
}

// OffsetAt returns the pack-file byte offset for the OID at position n.
func (i *Idx) OffsetAt(n int) uint64 {
	raw := binary.BigEndian.Uint32(i.offsets[n*idxOffsetSize:])
	if raw&idxOffsetMSB == 0 {
		return uint64(raw)
	}
	largeIdx := int(raw &^ idxOffsetMSB)
	return i.largeOffs[largeIdx]
}

// Lookup returns the pack-file offset for oid, or false if absent.
func (i *Idx) Lookup(oid OID) (uint64, bool) {
	first := oid[0]
	lo := 0
	if first > 0 {
		lo = int(i.fanout[first-1])
	}
	hi := int(i.fanout[first])
	if lo == hi {
		return 0, false
	}
	pos := sort.Search(hi-lo, func(k int) bool {
		var got OID
		copy(got[:], i.oids[(lo+k)*idxOIDSize:])
		for b := 0; b < idxOIDSize; b++ {
			if got[b] != oid[b] {
				return got[b] >= oid[b]
			}
		}
		return true
	})
	if pos == hi-lo {
		return 0, false
	}
	abs := lo + pos
	var got OID
	copy(got[:], i.oids[abs*idxOIDSize:])
	if got != oid {
		return 0, false
	}
	return i.OffsetAt(abs), true
}
