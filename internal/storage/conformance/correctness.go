package conformance

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// runCorrectness is the entry point for the §29 correctness tests. Each
// test corresponds to one numbered item in §29.
func runCorrectness(t *testing.T, f Factory) {
	t.Helper()
	t.Run("§29#4_PutThenGet_RAW", func(t *testing.T) { test29_4(t, f) })
}

// §29 #4: Read after write sees latest object.
func test29_4(t *testing.T, f Factory) {
	s := newStore(t, f)
	want := []byte("hello world")
	v, err := s.PutIfAbsent(ctx(), "rk/29-4", bytes.NewReader(want), nil)
	if err != nil {
		t.Fatalf("PutIfAbsent: %v", err)
	}
	if v.Token == "" {
		t.Fatal("PutIfAbsent returned empty version token")
	}

	md, err := s.Head(ctx(), "rk/29-4")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if md.Size != int64(len(want)) {
		t.Errorf("Head Size = %d, want %d", md.Size, len(want))
	}
	if md.Version != v {
		t.Errorf("Head Version = %+v, want %+v", md.Version, v)
	}

	obj, err := s.Get(ctx(), "rk/29-4", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer obj.Body.Close()
	got, err := io.ReadAll(obj.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Get content = %q, want %q", got, want)
	}

	if _, err := s.Get(ctx(), "rk/missing", nil); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Get(missing) = %v, want ErrNotFound", err)
	}
}
