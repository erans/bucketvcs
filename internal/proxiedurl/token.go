package proxiedurl

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"time"
)

// Token is the decoded form of a verified token.
type Token struct {
	Kind string
	Hash string
	Exp  time.Time
}

// Mint constructs a base64url-encoded token bound to (kind, hash, exp).
// kind must be "bundle" or "pack". hash is the URL-path hash (sha256-...
// for bundles, 40-hex sha1 for packs). The signing key MUST be at least
// 16 bytes; 32 bytes is recommended.
func Mint(key []byte, kind, hash string, exp time.Time) (string, error) {
	if len(key) < 16 {
		return "", fmt.Errorf("proxiedurl: signing key too short (%d bytes); need >= 16", len(key))
	}
	if kind != "bundle" && kind != "pack" {
		return "", fmt.Errorf("proxiedurl: invalid kind %q", kind)
	}
	if hash == "" {
		return "", fmt.Errorf("proxiedurl: empty hash")
	}
	payload := encodePayload(kind, hash, exp)
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	sig := mac.Sum(nil)
	body := append(payload, sig...)
	return base64.RawURLEncoding.EncodeToString(body), nil
}

// Verify decodes and verifies a token. Returns the parsed Token if all
// of (signature, kind, hash, expiry) match. Errors are sentinel-typed
// so callers can distinguish "expired" (don't log loudly) from
// "tampered" (log + metric).
//
// now is parameterised for testability.
func Verify(key []byte, token, expectKind, expectHash string, now time.Time) (Token, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return Token{}, fmt.Errorf("%w: base64: %v", ErrTokenInvalid, err)
	}
	// Minimum well-formed token: sha256.Size (32B HMAC) + minimum payload
	// (1B kind + 8B exp + at least 1B hash = 10B). Rejecting obviously
	// truncated tokens up front keeps the "too short" error consistent —
	// otherwise some sub-minimum tokens would reach decodePayload and
	// surface as "payload too short" wrapped in ErrTokenInvalid, which is
	// harmless but inconsistent with the early gate.
	const minPayload = 1 + 8 + 1
	if len(raw) < sha256.Size+minPayload {
		return Token{}, fmt.Errorf("%w: too short", ErrTokenInvalid)
	}
	payloadLen := len(raw) - sha256.Size
	payload := raw[:payloadLen]
	sig := raw[payloadLen:]

	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	want := mac.Sum(nil)
	if !hmac.Equal(want, sig) {
		return Token{}, ErrTokenInvalid
	}

	tk, err := decodePayload(payload)
	if err != nil {
		return Token{}, fmt.Errorf("%w: decode: %v", ErrTokenInvalid, err)
	}
	// Expiry semantics: Exp is INCLUSIVE — a token is valid at its exp
	// timestamp and expires strictly after. This matches JWT RFC 7519
	// §4.1.4 ("current date/time MUST be before the expiration"). Our
	// resolution is one second (encodePayload writes exp.Unix()), so the
	// one-second equality window is intentional, not a bug.
	if now.After(tk.Exp) {
		return Token{}, ErrTokenExpired
	}
	if tk.Kind != expectKind {
		return Token{}, ErrKindMismatch
	}
	if tk.Hash != expectHash {
		return Token{}, fmt.Errorf("%w: hash mismatch", ErrTokenInvalid)
	}
	return tk, nil
}

// payload layout: [kind(1B)] [exp_unix(8B BE)] [hash(rest)]
//
//	kind: 1 = bundle, 2 = pack
//
// Compact, fixed-size for the prefix so we can reject malformed tokens
// before the HMAC compare without leaking timing.
//
// encodePayload panics on an unknown kind. Mint's public contract already
// rejects non-bundle/pack kinds, so a panic here would only fire if a
// future caller bypassed Mint — a programmer error, not a runtime input
// failure. Panic loud rather than emit a token with a zero kind byte
// that decodePayload would later reject with a generic "unknown kind".
func encodePayload(kind, hash string, exp time.Time) []byte {
	var k byte
	switch kind {
	case "bundle":
		k = 1
	case "pack":
		k = 2
	default:
		panic(fmt.Sprintf("proxiedurl.encodePayload: invalid kind %q (caller must validate)", kind))
	}
	buf := make([]byte, 1+8+len(hash))
	buf[0] = k
	binary.BigEndian.PutUint64(buf[1:9], uint64(exp.Unix()))
	copy(buf[9:], []byte(hash))
	return buf
}

func decodePayload(p []byte) (Token, error) {
	if len(p) < 10 {
		return Token{}, fmt.Errorf("payload too short (%d)", len(p))
	}
	var kind string
	switch p[0] {
	case 1:
		kind = "bundle"
	case 2:
		kind = "pack"
	default:
		return Token{}, fmt.Errorf("unknown kind byte %d", p[0])
	}
	exp := time.Unix(int64(binary.BigEndian.Uint64(p[1:9])), 0).UTC()
	hash := string(p[9:])
	return Token{Kind: kind, Hash: hash, Exp: exp}, nil
}
