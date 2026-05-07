package s3compat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestListEmpty(t *testing.T) {
	s, _ := newMockBackend(t)
	page, err := s.List(context.Background(), "anything", nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Objects) != 0 {
		t.Fatalf("len(Objects) = %d, want 0", len(page.Objects))
	}
}

func TestListReturnsKeys(t *testing.T) {
	s, mb := newMockBackend(t)
	mb.put("a/x", []byte("x"), `"e1"`)
	mb.put("a/y", []byte("y"), `"e2"`)
	mb.put("b/z", []byte("z"), `"e3"`)

	page, err := s.List(context.Background(), "a/", nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := make([]string, len(page.Objects))
	for i, o := range page.Objects {
		got[i] = o.Key
	}
	sort.Strings(got)
	want := []string{"a/x", "a/y"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("keys = %v, want %v", got, want)
	}
}

func TestListWithDelimiter(t *testing.T) {
	s, mb := newMockBackend(t)
	mb.put("d/a/1", []byte(""), `"e"`)
	mb.put("d/a/2", []byte(""), `"e"`)
	mb.put("d/b/1", []byte(""), `"e"`)
	mb.put("d/c", []byte(""), `"e"`)

	page, err := s.List(context.Background(), "d/", &storage.ListOptions{Delimiter: "/"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Objects) != 1 || page.Objects[0].Key != "d/c" {
		t.Fatalf("Objects = %v, want exactly d/c", page.Objects)
	}
	cps := append([]string(nil), page.CommonPrefixes...)
	sort.Strings(cps)
	want := []string{"d/a/", "d/b/"}
	if len(cps) != 2 || cps[0] != want[0] || cps[1] != want[1] {
		t.Fatalf("CommonPrefixes = %v, want %v", cps, want)
	}
}

func TestListStripsAdapterPrefix(t *testing.T) {
	mb := &mockBackend{t: t, objects: map[string]mockObject{
		"acme/foo": {body: nil, etag: `"e"`},
		"acme/bar": {body: nil, etag: `"e"`},
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
		t.Fatal(err)
	}

	page, err := s.List(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(page.Objects))
	for i, o := range page.Objects {
		got[i] = o.Key
	}
	sort.Strings(got)
	want := []string{"bar", "foo"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("keys = %v, want %v (adapter prefix should be stripped)", got, want)
	}
}
