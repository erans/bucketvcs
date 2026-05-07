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
