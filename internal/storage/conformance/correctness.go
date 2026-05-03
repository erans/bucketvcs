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
	t.Run("§29#9_GetRange", func(t *testing.T) { test29_9(t, f) })
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

// §29 #9: Range read returns exact bytes (and truncates to EOF when
// endInclusive exceeds the object size, mirroring HTTP semantics).
func test29_9(t *testing.T, f Factory) {
	s := newStore(t, f)
	const size = 1 << 20 // 1 MiB
	content := DeterministicBytes(size, "29-9")
	if _, err := s.PutIfAbsent(ctx(), "rk/29-9", bytes.NewReader(content), nil); err != nil {
		t.Fatalf("PutIfAbsent: %v", err)
	}

	cases := []struct {
		start, end int64
	}{
		{0, 0},
		{0, 1023},
		{1024, 2047},
		{int64(size) - 1, int64(size) - 1},
		{int64(size) - 1024, int64(size) - 1},
	}
	for _, c := range cases {
		rc, err := s.GetRange(ctx(), "rk/29-9", c.start, c.end)
		if err != nil {
			t.Fatalf("GetRange[%d,%d]: %v", c.start, c.end, err)
		}
		got, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("ReadAll[%d,%d]: %v", c.start, c.end, err)
		}
		want := content[c.start : c.end+1]
		if !bytes.Equal(got, want) {
			t.Errorf("GetRange[%d,%d] mismatch: got len=%d want len=%d", c.start, c.end, len(got), len(want))
		}
	}

	// Off-end: end exceeds content size; expect truncation to EOF.
	rc, err := s.GetRange(ctx(), "rk/29-9", int64(size-10), int64(size+1000))
	if err != nil {
		t.Fatalf("GetRange off-end: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if len(got) != 10 {
		t.Errorf("GetRange off-end returned %d bytes, want 10", len(got))
	}

	// Invalid: negative start.
	if _, err := s.GetRange(ctx(), "rk/29-9", -1, 5); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("GetRange(negative start) = %v, want ErrInvalidArgument", err)
	}

	// Missing key.
	if _, err := s.GetRange(ctx(), "rk/29-9-missing", 0, 0); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("GetRange(missing) = %v, want ErrNotFound", err)
	}
}
