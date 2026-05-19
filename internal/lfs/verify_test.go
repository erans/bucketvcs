package lfs

import (
	"context"
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestVerify_OK_SizeMatches(t *testing.T) {
	oid := "1111111111111111111111111111111111111111111111111111111111111111"
	s := newBatchStore(map[string]int64{oid: 42}, signedFn())
	if err := Verify(context.Background(), s, oid, 42); err != nil {
		t.Fatalf("Verify: %v, want nil", err)
	}
}

func TestVerify_SizeMismatch(t *testing.T) {
	oid := "1111111111111111111111111111111111111111111111111111111111111111"
	s := newBatchStore(map[string]int64{oid: 50}, signedFn())
	err := Verify(context.Background(), s, oid, 42)
	if !errors.Is(err, ErrVerifySizeMismatch) {
		t.Fatalf("Verify: %v, want ErrVerifySizeMismatch", err)
	}
}

func TestVerify_NotFound(t *testing.T) {
	oid := "1111111111111111111111111111111111111111111111111111111111111111"
	s := newBatchStore(nil, signedFn())
	err := Verify(context.Background(), s, oid, 42)
	if !errors.Is(err, ErrVerifyNotFound) {
		t.Fatalf("Verify: %v, want ErrVerifyNotFound", err)
	}
}

func TestVerify_BackendError(t *testing.T) {
	boom := errors.New("backend boom")
	fake := &fakeBatchStore{
		objects: map[string]int64{},
		signFn:  signedFn(),
	}
	// Override Head to return boom.
	fake.headOverride = func(ctx context.Context, key string) (*storage.ObjectMetadata, error) {
		return nil, boom
	}
	s := NewStore(fake, "p/")
	err := Verify(context.Background(), s, "1111111111111111111111111111111111111111111111111111111111111111", 42)
	if !errors.Is(err, boom) {
		t.Fatalf("Verify: %v, want backend error wrapping boom", err)
	}
}
