package webhooks_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

func TestSign_Format(t *testing.T) {
	secret := "test-secret-NQpV4o7e3xR2mJ5tYqH9cL1pZ6wB0aDf"
	body := []byte(`{"event":"push"}`)
	t0 := int64(1716393600)
	got := webhooks.Sign(secret, t0, body)
	if !strings.HasPrefix(got, "t=1716393600,v1=") {
		t.Fatalf("Sign output %q does not start with t=...,v1=", got)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("1716393600."))
	mac.Write(body)
	want := "t=1716393600,v1=" + hex.EncodeToString(mac.Sum(nil))
	if got != want {
		t.Errorf("Sign:\n  got  %q\n  want %q", got, want)
	}
}

func TestSign_Deterministic(t *testing.T) {
	// Sign is a pure function — same inputs MUST produce the same output.
	// Note: this does NOT mean signatures are stable across retries; the
	// worker re-signs with the current `t` per attempt (preserves the 5-min
	// replay window). This test is about Sign() itself, not retry policy.
	secret := "abc"
	body := []byte(`{"x":1}`)
	t0 := int64(1716393600)
	a := webhooks.Sign(secret, t0, body)
	b := webhooks.Sign(secret, t0, body)
	if a != b {
		t.Errorf("Sign is non-deterministic: %q vs %q", a, b)
	}
}
