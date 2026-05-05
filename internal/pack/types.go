// Package pack implements a pure-Go random-access reader over Git's
// .pack/.idx v2 format, designed to read from a storage.ObjectStore
// (range GETs, not local files). M3's fetch negotiation will hold one
// Reader per pack per repo and call Get on the hot path.
package pack

import (
	"encoding/hex"
	"fmt"
)

// OID is a SHA-1 object identifier. M2 is SHA-1 only (§20); SHA-256
// support is deferred per the design doc.
type OID [20]byte

// String returns the lowercase hex form of the OID.
func (o OID) String() string {
	return hex.EncodeToString(o[:])
}

// ParseOID parses a 40-char hex string (case-insensitive) into an OID.
// String always returns the lowercase form per Git convention.
func ParseOID(s string) (OID, error) {
	var o OID
	if len(s) != 40 {
		return o, fmt.Errorf("pack: ParseOID: bad length %d (want 40)", len(s))
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return o, fmt.Errorf("pack: ParseOID: %w", err)
	}
	copy(o[:], b)
	return o, nil
}

// ObjectType is a Git object type. The numeric values match the
// `obj_type` field encoded in pack object headers (RFC pack-format §HEADER).
type ObjectType uint8

const (
	TypeInvalid ObjectType = 0
	TypeCommit  ObjectType = 1
	TypeTree    ObjectType = 2
	TypeBlob    ObjectType = 3
	TypeTag     ObjectType = 4
	// 5 is reserved for future use per the pack format.
	typeOFSDelta ObjectType = 6
	typeREFDelta ObjectType = 7
)

// String returns the lower-case Git type name ("commit", "tree", "blob",
// "tag"). Delta types and Invalid return their internal labels for use
// in error messages.
func (t ObjectType) String() string {
	switch t {
	case TypeCommit:
		return "commit"
	case TypeTree:
		return "tree"
	case TypeBlob:
		return "blob"
	case TypeTag:
		return "tag"
	case typeOFSDelta:
		return "ofs_delta"
	case typeREFDelta:
		return "ref_delta"
	default:
		return fmt.Sprintf("invalid(%d)", uint8(t))
	}
}

// Object is a fully-resolved Git object: deltas applied, payload inflated.
//
// Data is owned by the caller; mutation is permitted. The Reader's
// internal cache holds a separate snapshot, so caller mutations cannot
// poison subsequent Get calls. (Earlier M2 drafts required Data to be
// treated as read-only; that contract is no longer needed for safety.)
type Object struct {
	Type ObjectType
	Size int64  // length of Data; matches `git cat-file -s` semantics
	Data []byte // commit/tree/blob/tag content; never a delta
}
