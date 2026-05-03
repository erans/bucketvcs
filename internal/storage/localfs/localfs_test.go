package localfs_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestOpenLockFile(t *testing.T) {
	dir := t.TempDir()

	a, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	if _, err := localfs.Open(dir); !errors.Is(err, localfs.ErrAlreadyLocked) {
		t.Errorf("second Open returned %v, want ErrAlreadyLocked", err)
	}

	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	c, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close (c): %v", err)
	}
}

// TestCloseIdempotentAfterFailure verifies that if the on-disk lockfile
// disappears between Open and Close (simulating an external mutation that
// would have caused os.Remove to fail with anything other than
// ErrNotExist, we tolerate ErrNotExist; for the genuine-failure path the
// retry contract is exercised in TestCloseRetriesAfterRemoveFailure).
//
// This particular case asserts the simpler property: a second Close after
// a successful first Close is a no-op.
func TestCloseIdempotentAfterSuccess(t *testing.T) {
	dir := t.TempDir()
	s, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v (must be no-op after success)", err)
	}
}

// TestCloseToleratesMissingLockfile asserts Close returns nil when the
// lockfile has already been removed out of band (e.g., manual operator
// recovery via package-level Verify(WithForce)). os.ErrNotExist is the
// only os.Remove error we silently absorb; other failures must propagate
// and leave Close retryable.
func TestCloseToleratesMissingLockfile(t *testing.T) {
	dir := t.TempDir()
	s, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Remove the lockfile out of band before Close runs. A real operator
	// would only do this after confirming the holder is dead via Verify.
	if err := os.Remove(filepath.Join(dir, ".lock")); err != nil {
		t.Fatalf("manual lockfile remove: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close after external lockfile removal: %v", err)
	}
}

// TestOpenAcquiresLockBeforeSubdirs asserts that when an Open is refused
// because a .lock already exists, the would-be Open does not create the
// objects/ or uploads/ subdirectories. Initialization that mutates bucket
// state must happen only while holding the lock; otherwise a second
// concurrent caller can scribble on the bucket root before being told it
// cannot proceed.
func TestOpenAcquiresLockBeforeSubdirs(t *testing.T) {
	dir := t.TempDir()
	// Plant a .lock file before the first Open runs.
	if err := os.WriteFile(filepath.Join(dir, ".lock"), []byte(`{"pid":99999,"host":"prelocked","acquired_at":"2026-05-03T12:00:00Z"}`), 0o644); err != nil {
		t.Fatalf("plant lockfile: %v", err)
	}

	s, err := localfs.Open(dir)
	if err == nil {
		_ = s.Close()
		t.Fatalf("Open succeeded with pre-existing .lock, want ErrAlreadyLocked")
	}
	if !errors.Is(err, localfs.ErrAlreadyLocked) {
		t.Fatalf("Open with pre-existing .lock: got %v, want ErrAlreadyLocked", err)
	}

	for _, sub := range []string{"objects", "uploads"} {
		if _, err := os.Stat(filepath.Join(dir, sub)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("subdirectory %q exists after refused Open (err=%v); Open mutated bucket state before acquiring lock", sub, err)
		}
	}
}

// TestOperationsRefuseAfterClose asserts that every public method that
// touches the bucket fails with ErrClosed once Close has fully succeeded.
// A closed instance must NOT be able to mutate or read the bucket because
// another process may now hold the lockfile and own the bucket state.
func TestOperationsRefuseAfterClose(t *testing.T) {
	dir := t.TempDir()
	s, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Stash a value first so reads have something to find if the closed
	// guard were missing.
	if _, err := s.PutIfAbsent(context.Background(), "before-close", bytes.NewReader([]byte("x")), nil); err != nil {
		t.Fatalf("PutIfAbsent before close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ctx := context.Background()
	if _, err := s.PutIfAbsent(ctx, "after-close", bytes.NewReader([]byte("y")), nil); !errors.Is(err, localfs.ErrClosed) {
		t.Errorf("PutIfAbsent after Close: got %v, want ErrClosed", err)
	}
	if _, err := s.Get(ctx, "before-close", nil); !errors.Is(err, localfs.ErrClosed) {
		t.Errorf("Get after Close: got %v, want ErrClosed", err)
	}
	if _, err := s.Head(ctx, "before-close"); !errors.Is(err, localfs.ErrClosed) {
		t.Errorf("Head after Close: got %v, want ErrClosed", err)
	}
	if _, err := s.GetRange(ctx, "before-close", 0, 0); !errors.Is(err, localfs.ErrClosed) {
		t.Errorf("GetRange after Close: got %v, want ErrClosed", err)
	}
}

// TestHeadFailsClosedOnFutureSidecarSchema plants a sidecar with a
// version greater than this binary understands. Head must return
// ErrUnsupportedSidecarSchema and MUST NOT silently downgrade the
// sidecar to the current schema (which would corrupt the on-disk
// format for any future binary that reads it).
func TestHeadFailsClosedOnFutureSidecarSchema(t *testing.T) {
	dir := t.TempDir()
	s, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Write a real object first so the content file exists.
	if _, err := s.PutIfAbsent(context.Background(), "future-key", bytes.NewReader([]byte("payload")), nil); err != nil {
		t.Fatalf("PutIfAbsent: %v", err)
	}

	// Replace the sidecar with a future-schema one.
	metaPath := filepath.Join(dir, "objects", "future-key.meta")
	futureSidecar := []byte(`{"version":2,"sha256":"unknown","size":7,"content_type":"","modified_at":"2030-01-01T00:00:00Z","new_field":"reserved"}`)
	if err := os.WriteFile(metaPath, futureSidecar, 0o644); err != nil {
		t.Fatalf("plant future sidecar: %v", err)
	}

	if _, err := s.Head(context.Background(), "future-key"); !errors.Is(err, localfs.ErrUnsupportedSidecarSchema) {
		t.Errorf("Head against future-schema sidecar: got %v, want ErrUnsupportedSidecarSchema", err)
	}

	// Confirm the on-disk sidecar was NOT overwritten by self-heal.
	got, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("re-read sidecar: %v", err)
	}
	if !bytes.Equal(got, futureSidecar) {
		t.Errorf("sidecar was mutated by failed Head; on-disk overwrite is a downgrade attack")
	}
}

// TestGetRangeFailsClosedOnFutureSidecarSchema mirrors the Head test for
// GetRange: range reads must respect the same schema-version gate Head
// and Get apply, otherwise an older binary could stream bytes from an
// object whose forward-schema sidecar would otherwise be refused.
func TestGetRangeFailsClosedOnFutureSidecarSchema(t *testing.T) {
	dir := t.TempDir()
	s, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if _, err := s.PutIfAbsent(context.Background(), "future-range", bytes.NewReader([]byte("payload")), nil); err != nil {
		t.Fatalf("PutIfAbsent: %v", err)
	}

	metaPath := filepath.Join(dir, "objects", "future-range.meta")
	if err := os.WriteFile(metaPath, []byte(`{"version":2,"sha256":"x","size":7,"content_type":"","modified_at":"2030-01-01T00:00:00Z"}`), 0o644); err != nil {
		t.Fatalf("plant future sidecar: %v", err)
	}

	rc, err := s.GetRange(context.Background(), "future-range", 0, 3)
	if err == nil {
		_ = rc.Close()
		t.Fatal("GetRange against future-schema sidecar succeeded; expected ErrUnsupportedSidecarSchema")
	}
	if !errors.Is(err, localfs.ErrUnsupportedSidecarSchema) {
		t.Errorf("GetRange against future-schema sidecar: got %v, want ErrUnsupportedSidecarSchema", err)
	}
}
