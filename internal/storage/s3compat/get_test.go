package s3compat

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestGetReturnsBody(t *testing.T) {
	s, mb := newMockBackend(t)
	mb.put("foo", []byte("hello"), `"abc"`)

	obj, err := s.Get(context.Background(), "foo", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer obj.Body.Close()
	body, err := io.ReadAll(obj.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(body) != "hello" {
		t.Fatalf("body = %q, want %q", body, "hello")
	}
	if obj.Metadata.Version.Token == "" {
		t.Fatalf("Version.Token empty; want ETag")
	}
}

func TestGetNotFound(t *testing.T) {
	s, _ := newMockBackend(t)
	_, err := s.Get(context.Background(), "missing", nil)
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestHeadReturnsMetadataOnly(t *testing.T) {
	s, mb := newMockBackend(t)
	mb.put("foo", []byte("hello"), `"abc"`)
	md, err := s.Head(context.Background(), "foo")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if md.Size != int64(len("hello")) {
		t.Fatalf("Size = %d, want %d", md.Size, len("hello"))
	}
}

func TestGetRangePartial(t *testing.T) {
	s, mb := newMockBackend(t)
	mb.put("foo", []byte("0123456789"), `"abc"`)

	rc, err := s.GetRange(context.Background(), "foo", 2, 5)
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(body) != "2345" {
		t.Fatalf("range = %q, want \"2345\"", body)
	}
}

func TestGetRangeRejectsNegative(t *testing.T) {
	s, _ := newMockBackend(t)
	_, err := s.GetRange(context.Background(), "foo", -1, 5)
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("err = %v, want ErrInvalidArgument", err)
	}
}

func TestGetWithPrefix(t *testing.T) {
	// Verify that the configured prefix is applied on the wire.
	mb := &mockBackend{t: t, objects: map[string]mockObject{
		"acme/foo": {body: []byte("ok"), etag: `"x"`},
	}}
	srv := httptest.NewServer(http.HandlerFunc(mb.serve))
	t.Cleanup(srv.Close)
	cfg := Config{
		Bucket:          "test-bucket",
		Prefix:          "acme/",
		Region:          "us-east-1",
		Endpoint:        srv.URL,
		ForcePathStyle:  true,
		AccessKeyID:     "AKID",
		SecretAccessKey: "SECRET",
	}
	s, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	obj, err := s.Get(context.Background(), "foo", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	body, _ := io.ReadAll(obj.Body)
	_ = obj.Body.Close()
	if !bytes.Equal(body, []byte("ok")) {
		t.Fatalf("body = %q, want \"ok\"", body)
	}
}

func TestGetWithIfVersionMatchesMismatch(t *testing.T) {
	s, mb := newMockBackend(t)
	mb.put("foo", []byte("hello"), `"v0"`)

	expected := storage.ObjectVersion{Token: `"WRONG"`, Kind: storage.VersionEtag}
	_, err := s.Get(context.Background(), "foo", &storage.GetOptions{IfVersionMatches: &expected})
	if !errors.Is(err, storage.ErrVersionMismatch) {
		t.Fatalf("err = %v, want ErrVersionMismatch (412 should classify as VersionMismatch on If-Match)", err)
	}
}
