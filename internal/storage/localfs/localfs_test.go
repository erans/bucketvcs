package localfs_test

import (
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
