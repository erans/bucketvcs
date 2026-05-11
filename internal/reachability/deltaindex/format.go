package deltaindex

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/bucketvcs/bucketvcs/internal/pack"
)

// Wire constants.
var Magic = [4]byte{'B', 'V', 'R', 'D'}

const (
	VersionCurrent uint32 = 1
	HeaderSize            = 32
	TrailerSize           = 32
	OIDLen                = 20
)

// Header is the parsed .bvrd header (cheap-read; 32 bytes).
type Header struct {
	Version  uint32
	NCommits uint32
	NReftips uint32
	NPacks   uint32
}

// ParseHeader parses the first HeaderSize bytes. Caller may have only
// the header in b; this function is intentionally permissive about
// what follows.
func ParseHeader(b []byte) (Header, error) {
	if len(b) < HeaderSize {
		return Header{}, fmt.Errorf("%w: short header (%d bytes)", ErrMalformed, len(b))
	}
	if !bytes.Equal(b[:4], Magic[:]) {
		return Header{}, fmt.Errorf("%w: bad magic", ErrMalformed)
	}
	return Header{
		Version:  binary.LittleEndian.Uint32(b[4:8]),
		NCommits: binary.LittleEndian.Uint32(b[8:12]),
		NReftips: binary.LittleEndian.Uint32(b[12:16]),
		NPacks:   binary.LittleEndian.Uint32(b[16:20]),
	}, nil
}

// CommitRecord is one new commit introduced by this push.
type CommitRecord struct {
	OID        pack.OID
	Generation uint32
	Parents    []pack.OID
}

// RefTipDiff is a ref update introduced by this push. OldOID is
// zero-valued for ref creation.
type RefTipDiff struct {
	RefName string
	OldOID  pack.OID
	NewOID  pack.OID
}

// Delta is the decoded form of one .bvrd file.
type Delta struct {
	Commits []CommitRecord // sorted by OID
	RefTips []RefTipDiff
	Packs   []pack.OID
}
