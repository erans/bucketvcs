package pack

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func newTestStore(t *testing.T) storage.ObjectStore {
	t.Helper()
	s, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStoreSource_ReadsRange(t *testing.T) {
	store := newTestStore(t)
	body := []byte("0123456789abcdef")
	if _, err := store.PutIfAbsent(context.Background(), "k", bytes.NewReader(body), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	src := NewStoreSource(context.Background(), store, "k", int64(len(body)))
	buf := make([]byte, 4)
	n, err := src.ReadAt(buf, 6)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != 4 {
		t.Fatalf("ReadAt n: got %d, want 4", n)
	}
	if !bytes.Equal(buf, []byte("6789")) {
		t.Fatalf("ReadAt got %q", buf)
	}
}

func TestStoreSource_ReadAtTail_ReturnsEOF(t *testing.T) {
	store := newTestStore(t)
	body := []byte("hello")
	if _, err := store.PutIfAbsent(context.Background(), "k", bytes.NewReader(body), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	src := NewStoreSource(context.Background(), store, "k", int64(len(body)))
	buf := make([]byte, 8)
	n, err := src.ReadAt(buf, 0)
	if n != 5 {
		t.Fatalf("ReadAt n: got %d, want 5", n)
	}
	if err != io.EOF {
		t.Fatalf("ReadAt err: got %v, want io.EOF", err)
	}
	if string(buf[:5]) != "hello" {
		t.Fatalf("ReadAt got %q", buf[:5])
	}
}

func TestStoreSource_PastEOF_ReturnsEOF(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "k", strings.NewReader("x"), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	src := NewStoreSource(context.Background(), store, "k", 1)
	buf := make([]byte, 1)
	if _, err := src.ReadAt(buf, 5); err != io.EOF {
		t.Fatalf("got %v, want io.EOF", err)
	}
}

func TestStoreSource_NegativeOffset_Errors(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "k", strings.NewReader("hi"), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	src := NewStoreSource(context.Background(), store, "k", 2)
	buf := make([]byte, 1)
	if _, err := src.ReadAt(buf, -1); err == nil {
		t.Fatalf("expected error for negative offset")
	}
}

func TestStoreSource_Size(t *testing.T) {
	store := newTestStore(t)
	src := NewStoreSource(context.Background(), store, "k", 1234)
	if got := src.Size(); got != 1234 {
		t.Fatalf("Size: got %d, want 1234", got)
	}
}

// shortStore wraps a store but returns a body shorter than the
// requested range, simulating a misbehaving backend.
type shortStore struct {
	storage.ObjectStore
	missing int64
}

func (s *shortStore) GetRange(ctx context.Context, key string, start, end int64) (io.ReadCloser, error) {
	rc, err := s.ObjectStore.GetRange(ctx, key, start, end)
	if err != nil {
		return nil, err
	}
	want := end - start + 1 - s.missing
	if want < 0 {
		want = 0
	}
	all, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return nil, err
	}
	if int64(len(all)) > want {
		all = all[:want]
	}
	return io.NopCloser(bytes.NewReader(all)), nil
}

func TestStoreSource_ShortReadNotAtEOF_Errors(t *testing.T) {
	base := newTestStore(t)
	body := []byte("0123456789abcdef")
	if _, err := base.PutIfAbsent(context.Background(), "k", bytes.NewReader(body), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	short := &shortStore{ObjectStore: base, missing: 2}
	src := NewStoreSource(context.Background(), short, "k", int64(len(body)))
	buf := make([]byte, 4)
	// Not at EOF: read at offset 0, 4 bytes, but the backend returns 2.
	if _, err := src.ReadAt(buf, 0); err == nil {
		t.Fatalf("expected short-read error, got nil")
	}
}
