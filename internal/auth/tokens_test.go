package auth

import (
	"strings"
	"testing"
)

func TestGenerateToken_FormatAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok, id, secret, err := GenerateToken()
		if err != nil {
			t.Fatalf("GenerateToken: %v", err)
		}
		if !strings.HasPrefix(tok, "bvts_") {
			t.Fatalf("missing prefix: %q", tok)
		}
		parts := strings.Split(tok, "_")
		if len(parts) != 3 {
			t.Fatalf("want 3 segments, got %d: %q", len(parts), tok)
		}
		if len(parts[1]) != 24 {
			t.Fatalf("id length = %d, want 24", len(parts[1]))
		}
		if len(parts[2]) != 52 {
			t.Fatalf("secret length = %d, want 52", len(parts[2]))
		}
		if parts[1] != id {
			t.Fatalf("returned id %q != token id %q", id, parts[1])
		}
		if parts[2] != secret {
			t.Fatalf("returned secret %q != token secret %q", secret, parts[2])
		}
		if seen[tok] {
			t.Fatalf("duplicate token: %q", tok)
		}
		seen[tok] = true
	}
}

func TestParseToken(t *testing.T) {
	tok, id, secret, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	gotID, gotSecret, err := ParseToken(tok)
	if err != nil {
		t.Fatalf("ParseToken: %v", err)
	}
	if gotID != id || gotSecret != secret {
		t.Fatalf("parse mismatch: got (%q,%q) want (%q,%q)", gotID, gotSecret, id, secret)
	}
}

func TestParseToken_Invalid(t *testing.T) {
	bad := []string{
		"",
		"bvts_",
		"bvts_only",
		"bvts__",
		"wrong_AAAAAAAAAAAAAAAAAAAAAAAA_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",
		"bvts_TOOSHORT_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",
		"bvts_AAAAAAAAAAAAAAAAAAAAAAAA_TOOSHORT",
		// lowercase letters reject (Crockford uppercase-only canonical):
		"bvts_aaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		// excluded letters (I, L, O, U) reject:
		"bvts_IIIIIIIIIIIIIIIIIIIIIIII_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",
	}
	for _, b := range bad {
		if _, _, err := ParseToken(b); err == nil {
			t.Errorf("ParseToken(%q): want error, got nil", b)
		}
	}
}

func TestHashAndVerify_Roundtrip(t *testing.T) {
	secret := "ABCDEFGHJKMNPQRSTVWXYZ0123456789ABCDEFGHJKMNPQRSTVWX"
	enc, err := HashSecret(secret)
	if err != nil {
		t.Fatalf("HashSecret: %v", err)
	}
	if !strings.HasPrefix(enc, "$argon2id$") {
		t.Fatalf("encoded form missing prefix: %q", enc)
	}
	if err := VerifyHash(secret, enc); err != nil {
		t.Fatalf("VerifyHash same secret: %v", err)
	}
	if err := VerifyHash(secret+"X", enc); err == nil {
		t.Fatal("VerifyHash mismatched secret: expected error")
	}
}

func TestHashSecret_DifferentSaltsDifferentEncodings(t *testing.T) {
	a, err := HashSecret("same-secret")
	if err != nil {
		t.Fatalf("HashSecret a: %v", err)
	}
	b, err := HashSecret("same-secret")
	if err != nil {
		t.Fatalf("HashSecret b: %v", err)
	}
	if a == b {
		t.Fatal("two HashSecret calls produced identical encoding (salt should differ)")
	}
	if VerifyHash("same-secret", a) != nil || VerifyHash("same-secret", b) != nil {
		t.Fatal("both encodings should verify")
	}
}

func TestVerifyHash_Malformed(t *testing.T) {
	bad := []string{
		"",
		"plaintext",
		"$argon2id$",
		"$argon2id$v=19$m=65536,t=3,p=4$bad-base64$bad-base64",
	}
	for _, b := range bad {
		if err := VerifyHash("secret", b); err == nil {
			t.Errorf("VerifyHash(_, %q): expected error", b)
		}
	}
}
