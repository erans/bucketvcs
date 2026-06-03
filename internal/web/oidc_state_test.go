package web

import (
	"testing"
	"time"
)

func TestOIDCStateRoundTripAndTamper(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	st := oidcState{State: "s1", Nonce: "n1", Verifier: "v1", Next: "/repos", Exp: time.Now().Add(10 * time.Minute).Unix()}

	enc := encodeOIDCState(key, st)
	if enc == "" {
		t.Fatal("empty encoding")
	}
	got, err := decodeOIDCState(key, enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.State != "s1" || got.Nonce != "n1" || got.Verifier != "v1" || got.Next != "/repos" {
		t.Fatalf("roundtrip = %+v", got)
	}

	// tamper: flip first char of the payload → HMAC mismatch
	bad := "A" + enc[1:]
	if _, err := decodeOIDCState(key, bad); err == nil {
		t.Fatal("tampered blob accepted")
	}
	// wrong key → reject
	if _, err := decodeOIDCState([]byte("xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"), enc); err == nil {
		t.Fatal("wrong-key blob accepted")
	}
	// expired → reject
	old := oidcState{State: "s", Nonce: "n", Verifier: "v", Next: "/", Exp: time.Now().Add(-time.Minute).Unix()}
	if _, err := decodeOIDCState(key, encodeOIDCState(key, old)); err == nil {
		t.Fatal("expired blob accepted")
	}
}
