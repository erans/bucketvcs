package gc

import "strings"

// pointerVersionLine is the first-line magic for an LFS pointer blob.
// We accept either trailing-newline or trailing-CRLF/space variants
// implicitly via the HasPrefix check below.
const pointerVersionLine = "version https://git-lfs.github.com/spec/v1"

// ParsePointer inspects up to the first 1024 bytes of a blob and
// returns the referenced LFS object OID (lowercase 64-hex sha256) if
// the blob is a valid LFS pointer file. Returns ("", false) otherwise.
//
// LFS pointer file format (LFS spec):
//
//	version https://git-lfs.github.com/spec/v1
//	oid sha256:<64-lowercase-hex>
//	size <int>
//
// We validate only what's load-bearing for GC: the version line, the
// oid line's sha256: prefix, and the OID's 64-lowercase-hex shape. We
// don't validate size or reject extra trailing lines — the goal is
// "is this a pointer we can extract an oid from", not "is this a
// fully spec-conformant pointer".
func ParsePointer(b []byte) (oid string, ok bool) {
	// 1024-byte cap. Real pointers are < 200 bytes; this is generous.
	if len(b) > 1024 {
		b = b[:1024]
	}
	s := string(b)
	// First line must be the version magic.
	nl := strings.IndexByte(s, '\n')
	if nl < 0 {
		return "", false
	}
	if strings.TrimRight(s[:nl], "\r ") != pointerVersionLine {
		return "", false
	}
	rest := s[nl+1:]
	for {
		nl := strings.IndexByte(rest, '\n')
		var line string
		if nl < 0 {
			line = rest
			rest = ""
		} else {
			line = rest[:nl]
			rest = rest[nl+1:]
		}
		line = strings.TrimRight(line, "\r ")
		if strings.HasPrefix(line, "oid sha256:") {
			hex := line[len("oid sha256:"):]
			if isLowerHex64(hex) {
				return hex, true
			}
			return "", false
		}
		if rest == "" {
			break
		}
	}
	return "", false
}

// isLowerHex64 returns true if s is exactly 64 lowercase-hex chars.
func isLowerHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}
