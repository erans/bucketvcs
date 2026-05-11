package deltaindex

import (
	"crypto/sha256"
	"errors"
	"testing"
)

func TestDecode_RejectsBadMagic(t *testing.T) {
	bts, _ := Encode(Delta{})
	bts[0] = 'X'
	_, err := Decode(bts)
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v, want ErrMalformed", err)
	}
}

func TestDecode_RejectsBadVersion(t *testing.T) {
	bts, _ := Encode(Delta{})
	bts[4] = 0xFF // corrupt version
	rebuildTrailer(bts)
	_, err := Decode(bts)
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v, want ErrMalformed", err)
	}
}

func TestDecode_RejectsTruncatedTrailer(t *testing.T) {
	bts, _ := Encode(Delta{})
	_, err := Decode(bts[:len(bts)-1])
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v, want ErrMalformed", err)
	}
}

func TestDecode_RejectsTrailerMismatch(t *testing.T) {
	bts, _ := Encode(Delta{})
	bts[len(bts)-1] ^= 0xFF
	_, err := Decode(bts)
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v, want ErrMalformed", err)
	}
}

func rebuildTrailer(bts []byte) {
	body := bts[:len(bts)-TrailerSize]
	sum := sha256.Sum256(body)
	copy(bts[len(bts)-TrailerSize:], sum[:])
}
