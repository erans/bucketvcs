// Package objindex implements the M2 object-to-pack map (.bvom).
//
// Layout (per spec §3.4):
//
//	header  (32 bytes)  : magic "BVOM", version u32, count u64,
//	                      pack_tbl u64, reserved 8 bytes
//	records (count*32)  : oid (20) + pack_idx (u16) + reserved (2) + offset (u64),
//	                      sorted ascending by oid
//	pack_tbl            : n_packs u16, then n_packs * 40 bytes (ASCII hex pack_id)
//	trailer (32 bytes)  : SHA-256 over everything before trailer
package objindex

import (
	"errors"

	"github.com/bucketvcs/bucketvcs/internal/pack"
)

// validPackID reports whether s is a 40-character lowercase hex string.
func validPackID(s string) bool {
	if len(s) != packIDSize {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

const (
	headerSize  = 32
	recordSize  = 32
	trailerSize = 32
	packIDSize  = 40 // ASCII hex SHA-1
	currentVer  = uint32(1)
)

var magic = []byte{'B', 'V', 'O', 'M'}

// ErrCorrupt indicates a malformed .bvom file.
var ErrCorrupt = errors.New("objindex: corrupt")

// Entry pairs an OID with its pack location.
type Entry struct {
	OID    pack.OID
	PackID string // 40-char hex SHA-1
	Offset uint64
}

func recordsStart() int { return headerSize }
