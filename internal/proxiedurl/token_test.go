package proxiedurl

import (
	"errors"
	"testing"
	"time"
)

var testKey = []byte("0123456789abcdef0123456789abcdef")

func TestMint_Verify_Roundtrip(t *testing.T) {
	tok, err := Mint(testKey, "bundle", "sha256-abc", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	got, err := Verify(testKey, tok, "bundle", "sha256-abc", time.Now())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Kind != "bundle" || got.Hash != "sha256-abc" {
		t.Fatalf("got %+v", got)
	}
}

func TestVerify_Expired(t *testing.T) {
	tok, _ := Mint(testKey, "bundle", "sha256-abc", time.Now().Add(-time.Minute))
	_, err := Verify(testKey, tok, "bundle", "sha256-abc", time.Now())
	if !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("err = %v, want ErrTokenExpired", err)
	}
}

func TestVerify_TamperedSignature(t *testing.T) {
	tok, _ := Mint(testKey, "bundle", "sha256-abc", time.Now().Add(time.Minute))
	// Flip a character in the token.
	bad := tok[:len(tok)-1] + "A"
	if bad == tok {
		bad = tok[:len(tok)-1] + "B"
	}
	_, err := Verify(testKey, bad, "bundle", "sha256-abc", time.Now())
	// HMAC compare runs BEFORE expiry decode, so any tamper that survives
	// base64 must produce ErrTokenInvalid. We don't fall through to
	// ErrTokenExpired because the signature won't match.
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want ErrTokenInvalid", err)
	}
}

func TestVerify_Base64Garbage(t *testing.T) {
	// "!!!" is not a valid base64url character; should reject with
	// ErrTokenInvalid wrapped around the base64 decode error.
	_, err := Verify(testKey, "!!!", "bundle", "sha256-abc", time.Now())
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want ErrTokenInvalid (base64)", err)
	}
}

func TestVerify_KindMismatch(t *testing.T) {
	tok, _ := Mint(testKey, "bundle", "sha256-abc", time.Now().Add(time.Minute))
	_, err := Verify(testKey, tok, "pack", "sha256-abc", time.Now())
	if !errors.Is(err, ErrKindMismatch) {
		t.Fatalf("err = %v, want ErrKindMismatch", err)
	}
}

func TestVerify_HashMismatch(t *testing.T) {
	tok, _ := Mint(testKey, "bundle", "sha256-abc", time.Now().Add(time.Minute))
	_, err := Verify(testKey, tok, "bundle", "sha256-zzz", time.Now())
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want ErrTokenInvalid", err)
	}
}

func TestVerify_DifferentKey_Rejected(t *testing.T) {
	tok, _ := Mint(testKey, "bundle", "sha256-abc", time.Now().Add(time.Minute))
	other := []byte("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz")
	_, err := Verify(other, tok, "bundle", "sha256-abc", time.Now())
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want ErrTokenInvalid", err)
	}
}
