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
	t.Run("§29#1_ConcurrentPutIfAbsent", func(t *testing.T) { test29_1(t, f) })
	t.Run("§29#14_PutIfAbsentIdempotentRetry", func(t *testing.T) { test29_14(t, f) })
	t.Run("§29#2_ConcurrentPutIfVersionMatches", func(t *testing.T) { test29_2(t, f) })
	t.Run("§29#3_FailedConditionalDoesNotAlter", func(t *testing.T) { test29_3(t, f) })
	t.Run("§29#5_OverwriteThenRead", func(t *testing.T) { test29_5(t, f) })
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

// §29 #1: Concurrent putIfAbsent same key — exactly one succeeds.
func test29_1(t *testing.T, f Factory) {
	s := newStore(t, f)
	const n = 64
	content := []byte("payload-29-1")
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := s.PutIfAbsent(ctx(), "rk/29-1", bytes.NewReader(content), nil)
			results <- err
		}()
	}
	successes, conflicts, others := 0, 0, 0
	for i := 0; i < n; i++ {
		err := <-results
		switch {
		case err == nil:
			successes++
		case errors.Is(err, storage.ErrAlreadyExists):
			conflicts++
		default:
			others++
			t.Errorf("unexpected error: %v", err)
		}
	}
	if successes != 1 {
		t.Errorf("successes = %d, want 1", successes)
	}
	if conflicts != n-1 {
		t.Errorf("conflicts = %d, want %d", conflicts, n-1)
	}
}

// §29 #14 (recast per AD8): PutIfAbsent twice with the same args returns
// ErrAlreadyExists cleanly without corrupting state on the second call.
// See M0 design doc Architectural Decision 8.
func test29_14(t *testing.T, f Factory) {
	s := newStore(t, f)
	content := []byte("payload-29-14")
	v1, err := s.PutIfAbsent(ctx(), "rk/29-14", bytes.NewReader(content), nil)
	if err != nil {
		t.Fatalf("first PutIfAbsent: %v", err)
	}
	if _, err := s.PutIfAbsent(ctx(), "rk/29-14", bytes.NewReader(content), nil); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Errorf("second PutIfAbsent = %v, want ErrAlreadyExists", err)
	}

	md, err := s.Head(ctx(), "rk/29-14")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if md.Version != v1 {
		t.Errorf("version mutated by failed second PutIfAbsent: got %+v, want %+v", md.Version, v1)
	}
}

// §29 #2: Concurrent putIfVersionMatches same key — exactly one succeeds.
func test29_2(t *testing.T, f Factory) {
	s := newStore(t, f)
	v0, err := s.PutIfAbsent(ctx(), "rk/29-2", bytes.NewReader([]byte("v0")), nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	const n = 64
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := s.PutIfVersionMatches(ctx(), "rk/29-2", v0, bytes.NewReader([]byte("v1")), nil)
			results <- err
		}()
	}
	successes, conflicts, others := 0, 0, 0
	for i := 0; i < n; i++ {
		err := <-results
		switch {
		case err == nil:
			successes++
		case errors.Is(err, storage.ErrVersionMismatch):
			conflicts++
		default:
			others++
			t.Errorf("unexpected error: %v", err)
		}
	}
	if successes != 1 {
		t.Errorf("successes = %d, want 1", successes)
	}
	if conflicts != n-1 {
		t.Errorf("conflicts = %d, want %d", conflicts, n-1)
	}
}

// §29 #3: Failed conditional write does not alter object.
func test29_3(t *testing.T, f Factory) {
	s := newStore(t, f)
	want := []byte("original")
	v0, err := s.PutIfAbsent(ctx(), "rk/29-3", bytes.NewReader(want), nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	bogus := storage.ObjectVersion{Provider: v0.Provider, Token: "deadbeef", Kind: v0.Kind}
	if _, err := s.PutIfVersionMatches(ctx(), "rk/29-3", bogus, bytes.NewReader([]byte("DROP")), nil); !errors.Is(err, storage.ErrVersionMismatch) {
		t.Errorf("PutIfVersionMatches(bogus) = %v, want ErrVersionMismatch", err)
	}

	obj, err := s.Get(ctx(), "rk/29-3", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer obj.Body.Close()
	got, _ := io.ReadAll(obj.Body)
	if !bytes.Equal(got, want) {
		t.Errorf("content mutated by failed conditional: got %q, want %q", got, want)
	}
	if obj.Metadata.Version != v0 {
		t.Errorf("version mutated by failed conditional: got %+v, want %+v", obj.Metadata.Version, v0)
	}
}

// §29 #5: Read after overwrite sees the latest object.
func test29_5(t *testing.T, f Factory) {
	s := newStore(t, f)
	v0, err := s.PutIfAbsent(ctx(), "rk/29-5", bytes.NewReader([]byte("v0")), nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	v1, err := s.PutIfVersionMatches(ctx(), "rk/29-5", v0, bytes.NewReader([]byte("v1-content")), nil)
	if err != nil {
		t.Fatalf("PutIfVersionMatches: %v", err)
	}
	if v1 == v0 {
		t.Error("version did not change after overwrite")
	}
	obj, err := s.Get(ctx(), "rk/29-5", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer obj.Body.Close()
	got, _ := io.ReadAll(obj.Body)
	if string(got) != "v1-content" {
		t.Errorf("after overwrite content = %q, want %q", got, "v1-content")
	}
	if obj.Metadata.Version != v1 {
		t.Errorf("Metadata.Version = %+v, want %+v", obj.Metadata.Version, v1)
	}
}
