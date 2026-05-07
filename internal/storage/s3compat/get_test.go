package s3compat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// mockBackend is a minimal in-memory S3 server. Each test uses one.
// The handler matches request method + URL path against a tiny dispatch
// table the test registers via Set/SetGet/etc. before exercising the
// adapter. It exists ONLY for unit tests; live conformance covers real
// S3/R2 behavior.
type mockBackend struct {
	t       *testing.T
	objects map[string]mockObject // key (incl. bucket prefix) -> obj
}

type mockObject struct {
	body []byte
	etag string
}

func newMockBackend(t *testing.T) (*S3Compat, *mockBackend) {
	t.Helper()
	mb := &mockBackend{t: t, objects: map[string]mockObject{}}
	srv := httptest.NewServer(http.HandlerFunc(mb.serve))
	t.Cleanup(srv.Close)
	cfg := Config{
		Bucket:          "test-bucket",
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
	return s, mb
}

func (m *mockBackend) put(key string, body []byte, etag string) {
	m.objects[key] = mockObject{body: body, etag: etag}
}

func (m *mockBackend) keyFromPath(p string) string {
	// Path is "/<bucket>/<key>"; strip the leading "/<bucket>/".
	p = strings.TrimPrefix(p, "/")
	_, key, _ := strings.Cut(p, "/")
	return key
}

func (m *mockBackend) serve(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		key := m.keyFromPath(r.URL.Path)
		obj, ok := m.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("ETag", obj.etag)
		// Honor Range if present.
		if rng := r.Header.Get("Range"); rng != "" {
			start, end, ok := parseSimpleRange(rng, len(obj.body))
			if !ok {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			chunk := obj.body[start : end+1]
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(chunk)))
			w.WriteHeader(http.StatusPartialContent)
			if r.Method == http.MethodGet {
				_, _ = w.Write(chunk)
			}
			return
		}
		// Always set Content-Length so HEAD callers see the object size.
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(obj.body)))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(obj.body)
		}
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

// parseSimpleRange handles "bytes=start-end" only.
func parseSimpleRange(h string, size int) (start, end int, ok bool) {
	const prefix = "bytes="
	if !strings.HasPrefix(h, prefix) {
		return 0, 0, false
	}
	parts := strings.SplitN(h[len(prefix):], "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	var s, e int
	if _, err := bytes2int(parts[0], &s); err != nil {
		return 0, 0, false
	}
	if _, err := bytes2int(parts[1], &e); err != nil {
		return 0, 0, false
	}
	if e >= size {
		e = size - 1
	}
	if s < 0 || s > e {
		return 0, 0, false
	}
	return s, e, true
}

func bytes2int(s string, dst *int) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("not a number")
		}
		n = n*10 + int(c-'0')
	}
	*dst = n
	return n, nil
}

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
