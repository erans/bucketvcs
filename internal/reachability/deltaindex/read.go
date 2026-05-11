package deltaindex

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/bucketvcs/bucketvcs/internal/pack"
)

// ErrMalformed is returned when the .bvrd bytes don't match the format.
var ErrMalformed = errors.New("deltaindex: malformed")

// Decode parses .bvrd bytes into a Delta. The trailing SHA-256 is
// verified; a mismatch returns ErrMalformed.
func Decode(b []byte) (*Delta, error) {
	if len(b) < HeaderSize+TrailerSize {
		return nil, fmt.Errorf("%w: too short (%d bytes)", ErrMalformed, len(b))
	}
	if !bytes.Equal(b[:4], Magic[:]) {
		return nil, fmt.Errorf("%w: bad magic %q", ErrMalformed, b[:4])
	}
	body := b[:len(b)-TrailerSize]
	wantSum := b[len(b)-TrailerSize:]
	gotSum := sha256.Sum256(body)
	if !bytes.Equal(gotSum[:], wantSum) {
		return nil, fmt.Errorf("%w: trailer hash mismatch", ErrMalformed)
	}

	r := &reader{buf: body, off: 0}
	if err := r.skip(4); err != nil {
		return nil, err
	}
	ver, err := r.readU32()
	if err != nil {
		return nil, err
	}
	if ver != VersionCurrent {
		return nil, fmt.Errorf("%w: version %d not supported", ErrMalformed, ver)
	}
	nCommits, err := r.readU32()
	if err != nil {
		return nil, err
	}
	nReftips, err := r.readU32()
	if err != nil {
		return nil, err
	}
	nPacks, err := r.readU32()
	if err != nil {
		return nil, err
	}
	if err := r.skip(12); err != nil {
		return nil, err
	}

	d := &Delta{
		Commits: make([]CommitRecord, 0, nCommits),
		RefTips: make([]RefTipDiff, 0, nReftips),
		Packs:   make([]pack.OID, 0, nPacks),
	}

	for i := uint32(0); i < nCommits; i++ {
		var c CommitRecord
		if err := r.readOID(&c.OID); err != nil {
			return nil, err
		}
		c.Generation, err = r.readU32()
		if err != nil {
			return nil, err
		}
		nParents, err := r.readU8()
		if err != nil {
			return nil, err
		}
		c.Parents = make([]pack.OID, nParents)
		for j := uint8(0); j < nParents; j++ {
			if err := r.readOID(&c.Parents[j]); err != nil {
				return nil, err
			}
		}
		d.Commits = append(d.Commits, c)
	}

	type tipRaw struct {
		off    uint32
		newOID pack.OID
		oldOID pack.OID
	}
	raws := make([]tipRaw, nReftips)
	for i := uint32(0); i < nReftips; i++ {
		off, err := r.readU32()
		if err != nil {
			return nil, err
		}
		var newOID, oldOID pack.OID
		if err := r.readOID(&newOID); err != nil {
			return nil, err
		}
		if err := r.readOID(&oldOID); err != nil {
			return nil, err
		}
		raws[i] = tipRaw{off: off, newOID: newOID, oldOID: oldOID}
	}

	for i := uint32(0); i < nPacks; i++ {
		var p pack.OID
		if err := r.readOID(&p); err != nil {
			return nil, err
		}
		d.Packs = append(d.Packs, p)
	}

	for _, name := range []string{"trees_blobs_tags", "bitmap"} {
		n, err := r.readU32()
		if err != nil {
			return nil, err
		}
		remaining := uint32(len(r.buf) - r.off)
		if n > remaining {
			return nil, fmt.Errorf("%w: reserved section %q: %d byte length exceeds remaining buffer (%d)", ErrMalformed, name, n, remaining)
		}
		if err := r.skip(int(n)); err != nil {
			return nil, fmt.Errorf("%w: reserved %q section: %v", ErrMalformed, name, err)
		}
	}

	strtabLen, err := r.readU32()
	if err != nil {
		return nil, err
	}
	strtab, err := r.readBytes(int(strtabLen))
	if err != nil {
		return nil, err
	}

	for _, raw := range raws {
		name, err := readNUL(strtab, raw.off)
		if err != nil {
			return nil, err
		}
		d.RefTips = append(d.RefTips, RefTipDiff{
			RefName: name,
			NewOID:  raw.newOID,
			OldOID:  raw.oldOID,
		})
	}
	return d, nil
}

type reader struct {
	buf []byte
	off int
}

func (r *reader) skip(n int) error {
	if r.off+n > len(r.buf) {
		return fmt.Errorf("%w: short read of %d at offset %d", ErrMalformed, n, r.off)
	}
	r.off += n
	return nil
}
func (r *reader) readU8() (uint8, error) {
	if r.off+1 > len(r.buf) {
		return 0, fmt.Errorf("%w: short u8 at %d", ErrMalformed, r.off)
	}
	b := r.buf[r.off]
	r.off++
	return b, nil
}
func (r *reader) readU32() (uint32, error) {
	if r.off+4 > len(r.buf) {
		return 0, fmt.Errorf("%w: short u32 at %d", ErrMalformed, r.off)
	}
	v := binary.LittleEndian.Uint32(r.buf[r.off:])
	r.off += 4
	return v, nil
}
func (r *reader) readOID(out *pack.OID) error {
	if r.off+OIDLen > len(r.buf) {
		return fmt.Errorf("%w: short oid at %d", ErrMalformed, r.off)
	}
	copy(out[:], r.buf[r.off:r.off+OIDLen])
	r.off += OIDLen
	return nil
}
func (r *reader) readBytes(n int) ([]byte, error) {
	if r.off+n > len(r.buf) {
		return nil, fmt.Errorf("%w: short bytes(%d) at %d", ErrMalformed, n, r.off)
	}
	b := r.buf[r.off : r.off+n]
	r.off += n
	return b, nil
}
func readNUL(strtab []byte, off uint32) (string, error) {
	if int(off) >= len(strtab) {
		return "", fmt.Errorf("%w: strtab offset %d out of range", ErrMalformed, off)
	}
	end := bytes.IndexByte(strtab[off:], 0)
	if end < 0 {
		return "", fmt.Errorf("%w: unterminated string at %d", ErrMalformed, off)
	}
	return string(strtab[off : int(off)+end]), nil
}
