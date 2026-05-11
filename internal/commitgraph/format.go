// Package commitgraph implements the M2-local commit graph (.bvcg).
//
// Layout (per spec §3.5):
//
//	header (32 bytes)  : magic "BVCG", version u32, n_commits u64,
//	                     n_tips u32, reserved 12 bytes
//	tips (n_tips × 24) : ref_name_offset u32, oid 20 bytes
//	commits sorted by oid: oid 20 + n_parents u8 + parent_oids[n_parents]*20
//	string table       : NUL-terminated UTF-8 strings (ref names)
//	trailer (32 bytes) : SHA-256 over preceding bytes
package commitgraph

import (
	"errors"

	"github.com/bucketvcs/bucketvcs/internal/pack"
)

// Format versions:
//
//   v1 (pre-M10) — no generation numbers; reader returns 0.
//   v2 (M10)     — adds u32 generation_number after the oid in each
//                  commit record.
const (
	VersionV1      uint32 = 1
	VersionV2      uint32 = 2
	VersionCurrent uint32 = VersionV2
)

const (
	headerSize  = 32
	tipSize     = 24
	trailerSize = 32
	currentVer  = VersionCurrent // v2: includes generation number in each commit record
	maxParents  = 255 // n_parents is uint8
)

var magic = []byte{'B', 'V', 'C', 'G'}

// ErrCorrupt indicates a malformed .bvcg file.
var ErrCorrupt = errors.New("commitgraph: corrupt")

// Tip names a ref and the commit it points to.
type Tip struct {
	Ref string
	OID pack.OID
}

// Record is one commit's entry: its OID, generation number, and parent OIDs in commit order.
// Generation = 1 + max(generations of parents); root commits have generation = 1.
// On v1 files, Generation is 0 after read.
type Record struct {
	OID        pack.OID
	Generation uint32
	Parents    []pack.OID
}
