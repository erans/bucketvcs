package storage

import (
	"testing"
	"time"
)

func TestSignedURLOptions_ExpectedHash_Field(t *testing.T) {
	opts := SignedURLOptions{
		Expires:      5 * time.Minute,
		Method:       "GET",
		ExpectedHash: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	if opts.ExpectedHash != "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" {
		t.Fatalf("ExpectedHash field missing or not stored")
	}
}
