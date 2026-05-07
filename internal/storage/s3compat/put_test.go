package s3compat

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestPutIfAbsentNew(t *testing.T) {
	s, _ := newMockBackend(t)
	v, err := s.PutIfAbsent(context.Background(), "k", strings.NewReader("hi"), nil)
	if err != nil {
		t.Fatalf("PutIfAbsent: %v", err)
	}
	if v.Token == "" {
		t.Fatalf("returned ObjectVersion has empty Token")
	}
}

func TestPutIfAbsentConflict(t *testing.T) {
	s, mb := newMockBackend(t)
	mb.put("k", []byte("existing"), `"v0"`)

	_, err := s.PutIfAbsent(context.Background(), "k", strings.NewReader("new"), nil)
	if !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("err = %v, want ErrAlreadyExists", err)
	}
}

func TestPutIfVersionMatchesSuccess(t *testing.T) {
	s, mb := newMockBackend(t)
	mb.put("k", []byte("v0"), `"v0"`)
	expected := storage.ObjectVersion{Token: `"v0"`, Kind: storage.VersionEtag}

	v, err := s.PutIfVersionMatches(context.Background(), "k", expected, bytes.NewReader([]byte("v1")), nil)
	if err != nil {
		t.Fatalf("PutIfVersionMatches: %v", err)
	}
	if v.Token == expected.Token {
		t.Fatalf("returned token unchanged; expected new etag")
	}
}

func TestPutIfVersionMatchesMismatch(t *testing.T) {
	s, mb := newMockBackend(t)
	mb.put("k", []byte("v0"), `"v0"`)
	expected := storage.ObjectVersion{Token: `"WRONG"`, Kind: storage.VersionEtag}

	_, err := s.PutIfVersionMatches(context.Background(), "k", expected, bytes.NewReader([]byte("nope")), nil)
	if !errors.Is(err, storage.ErrVersionMismatch) {
		t.Fatalf("err = %v, want ErrVersionMismatch", err)
	}
}
