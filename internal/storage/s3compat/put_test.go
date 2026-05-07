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
	if !strings.HasPrefix(v.Token, `"`) || !strings.HasSuffix(v.Token, `"`) {
		t.Fatalf("returned Token %q not in S3 quoted-ETag format", v.Token)
	}
	if v.Kind != storage.VersionEtag {
		t.Fatalf("returned Kind = %v, want VersionEtag", v.Kind)
	}
	if v.Provider != "s3compat" {
		t.Fatalf("returned Provider = %q, want s3compat", v.Provider)
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

func TestPutMethodsRejectInvalidKey(t *testing.T) {
	s, _ := newMockBackend(t)
	bad := []string{"", "/foo", "foo/", "foo\x00bar"}

	for _, k := range bad {
		t.Run("PutIfAbsent_"+k, func(t *testing.T) {
			_, err := s.PutIfAbsent(context.Background(), k, strings.NewReader("x"), nil)
			if !errors.Is(err, storage.ErrInvalidArgument) {
				t.Fatalf("PutIfAbsent(%q) err = %v, want ErrInvalidArgument", k, err)
			}
		})
		t.Run("PutIfVersionMatches_"+k, func(t *testing.T) {
			_, err := s.PutIfVersionMatches(context.Background(), k, storage.ObjectVersion{Token: `"v0"`}, strings.NewReader("x"), nil)
			if !errors.Is(err, storage.ErrInvalidArgument) {
				t.Fatalf("PutIfVersionMatches(%q) err = %v, want ErrInvalidArgument", k, err)
			}
		})
	}
}

func TestPutIfAbsentHonorsContentType(t *testing.T) {
	s, mb := newMockBackend(t)
	_, err := s.PutIfAbsent(context.Background(), "k", strings.NewReader("hi"),
		&storage.PutOptions{ContentType: "application/json"})
	if err != nil {
		t.Fatalf("PutIfAbsent: %v", err)
	}
	if mb.lastContentType != "application/json" {
		t.Fatalf("lastContentType = %q, want application/json", mb.lastContentType)
	}
}

func TestPutIfAbsentDefaultContentType(t *testing.T) {
	s, mb := newMockBackend(t)
	_, err := s.PutIfAbsent(context.Background(), "k", strings.NewReader("hi"), nil)
	if err != nil {
		t.Fatalf("PutIfAbsent: %v", err)
	}
	// When PutOptions is nil, Content-Type should not be set by the
	// adapter. SDK still sends a default Content-Type (typically
	// "application/octet-stream") because Go's http stack injects one
	// when a body is present. We assert only that nothing exotic
	// arrived from our PutOptions path.
	if mb.lastContentType == "application/json" {
		t.Fatalf("default ContentType leaked from a previous call")
	}
}

func TestPutIfVersionMatchesNonexistentKey(t *testing.T) {
	s, _ := newMockBackend(t)
	expected := storage.ObjectVersion{Token: `"v0"`, Kind: storage.VersionEtag}
	_, err := s.PutIfVersionMatches(context.Background(), "ghost", expected,
		strings.NewReader("nope"), nil)
	if !errors.Is(err, storage.ErrVersionMismatch) {
		t.Fatalf("PutIfVersionMatches against missing key: err = %v, want ErrVersionMismatch", err)
	}
}
