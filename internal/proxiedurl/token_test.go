package proxiedurl

import (
	"bytes"
	"errors"
	"strings"
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

func TestMintVerify_LFSPut(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	hash := "acme/foo/" + strings.Repeat("a", 64)
	tok, err := Mint(key, "lfs-put", hash, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Mint(lfs-put): %v", err)
	}
	decoded, err := Verify(key, tok, "lfs-put", hash, time.Now())
	if err != nil {
		t.Fatalf("Verify(lfs-put): %v", err)
	}
	if decoded.Kind != "lfs-put" {
		t.Errorf("Kind=%q", decoded.Kind)
	}
}

func TestMintVerify_LFSGet(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	hash := "acme/foo/" + strings.Repeat("a", 64)
	tok, err := Mint(key, "lfs-get", hash, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Mint(lfs-get): %v", err)
	}
	decoded, err := Verify(key, tok, "lfs-get", hash, time.Now())
	if err != nil {
		t.Fatalf("Verify(lfs-get): %v", err)
	}
	if decoded.Kind != "lfs-get" {
		t.Errorf("Kind=%q", decoded.Kind)
	}
}

func TestVerify_LFSKindMismatch(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	hash := "acme/foo/" + strings.Repeat("a", 64)
	tok, err := Mint(key, "lfs-put", hash, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// Token minted as lfs-put cannot be used as lfs-get.
	if _, err := Verify(key, tok, "lfs-get", hash, time.Now()); !errors.Is(err, ErrKindMismatch) {
		t.Fatalf("expected ErrKindMismatch; got %v", err)
	}
	// Or as bundle.
	if _, err := Verify(key, tok, "bundle", hash, time.Now()); !errors.Is(err, ErrKindMismatch) {
		t.Fatalf("expected ErrKindMismatch; got %v", err)
	}
}

func TestMint_RejectsUnknownKind(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	_, err := Mint(key, "frobnicate", "hash", time.Now().Add(time.Minute))
	if err == nil {
		t.Fatal("expected error for unknown kind")
	}
}

func TestMint_Verify_LFSVerifyRoundtrip(t *testing.T) {
	tok, err := Mint(testKey, "lfs-verify", "acme/foo/abc123", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	got, err := Verify(testKey, tok, "lfs-verify", "acme/foo/abc123", time.Now())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Kind != "lfs-verify" || got.Hash != "acme/foo/abc123" {
		t.Errorf("got %+v", got)
	}
}

func TestVerify_LFSVerifyRejectsCrossKind(t *testing.T) {
	// A kind=3 lfs-put token must NOT verify as kind=5 lfs-verify.
	tok, _ := Mint(testKey, "lfs-put", "acme/foo/abc", time.Now().Add(time.Minute))
	_, err := Verify(testKey, tok, "lfs-verify", "acme/foo/abc", time.Now())
	if err == nil {
		t.Fatal("expected kind-mismatch error")
	}
}

// TestMintVerify_CompositeHashWithSlashes pins that the hash field round-trips
// when it carries embedded slashes — the format M19 uses for kind=1/2
// bundle/pack tokens, where hash = "<tenant>/<repo>/<sha>". The mint+verify
// pair must be byte-exact on the composite string; any difference (extra
// slash, different tenant, swapped order) must fail Verify.
func TestMintVerify_CompositeHashWithSlashes(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	exp := time.Now().Add(time.Hour)
	for _, kind := range []string{"bundle", "pack"} {
		composite := "acme/site/sha256-" + strings.Repeat("ab", 32)
		tok, err := Mint(key, kind, composite, exp)
		if err != nil {
			t.Fatalf("mint %s: %v", kind, err)
		}
		if _, err := Verify(key, tok, kind, composite, time.Now()); err != nil {
			t.Errorf("verify %s with same composite: %v", kind, err)
		}
		// Tampered tenant
		if _, err := Verify(key, tok, kind, "other/site/sha256-"+strings.Repeat("ab", 32), time.Now()); err == nil {
			t.Errorf("verify %s with swapped tenant: expected error, got nil", kind)
		}
		// Tampered repo
		if _, err := Verify(key, tok, kind, "acme/elsewhere/sha256-"+strings.Repeat("ab", 32), time.Now()); err == nil {
			t.Errorf("verify %s with swapped repo: expected error, got nil", kind)
		}
		// Tampered hash
		if _, err := Verify(key, tok, kind, "acme/site/sha256-"+strings.Repeat("cd", 32), time.Now()); err == nil {
			t.Errorf("verify %s with swapped hash: expected error, got nil", kind)
		}
	}
}
