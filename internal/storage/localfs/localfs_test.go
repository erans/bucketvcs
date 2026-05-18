package localfs_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage"
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

// TestListPrefixDoesNotNarrowOnDirectoryBoundary asserts that List("foo")
// surfaces keys whose names extend past "foo" without a slash, e.g.
// "foo2/bar". Earlier versions narrowed the walk to <root>/objects/foo/
// when that directory existed, which would silently omit "foo2/...".
func TestListPrefixDoesNotNarrowOnDirectoryBoundary(t *testing.T) {
	dir := t.TempDir()
	s, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	keys := []string{"foo/x", "foo/y", "foo2/bar"}
	for _, k := range keys {
		if _, err := s.PutIfAbsent(context.Background(), k, bytes.NewReader([]byte("v")), nil); err != nil {
			t.Fatalf("PutIfAbsent(%q): %v", k, err)
		}
	}

	page, err := s.List(context.Background(), "foo", nil)
	if err != nil {
		t.Fatalf("List(\"foo\"): %v", err)
	}
	got := map[string]bool{}
	for _, md := range page.Objects {
		got[md.Key] = true
	}
	for _, k := range keys {
		if !got[k] {
			t.Errorf("List(\"foo\") missing %q (got %v)", k, page.Objects)
		}
	}
}

// TestListRejectsEscapingPrefix asserts List validates the prefix and
// refuses to walk paths that could escape the bucket.
func TestListRejectsEscapingPrefix(t *testing.T) {
	dir := t.TempDir()
	s, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	bad := []string{"../etc/passwd", "/abs", "a/../b", "a/./b"}
	for _, p := range bad {
		if _, err := s.List(context.Background(), p, nil); err == nil {
			t.Errorf("List(%q) returned nil, want error", p)
		}
	}
}

func TestSidecarSelfHealMissing(t *testing.T) {
	dir := t.TempDir()
	s, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	want := []byte("self-heal-missing")
	v, err := s.PutIfAbsent(context.Background(), "rk/self-heal", bytes.NewReader(want), nil)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Delete the sidecar out of band.
	metaPath := filepath.Join(dir, "objects", "rk", "self-heal.meta")
	if err := os.Remove(metaPath); err != nil {
		t.Fatalf("remove sidecar: %v", err)
	}

	md, err := s.Head(context.Background(), "rk/self-heal")
	if err != nil {
		t.Fatalf("Head after sidecar removal: %v", err)
	}
	if md.Version.Token != v.Token {
		t.Errorf("self-heal recovered version = %s, want %s", md.Version.Token, v.Token)
	}
	if md.Size != int64(len(want)) {
		t.Errorf("self-heal recovered size = %d, want %d", md.Size, len(want))
	}

	// Sidecar should now exist again.
	if _, err := os.Stat(metaPath); err != nil {
		t.Errorf("sidecar not recreated: %v", err)
	}
}

// TestSidecarSelfHealSizeMismatch simulates the post-crash "content
// (new) + sidecar (old)" window: rewrite content out of band so its
// size differs from what the sidecar records, then call Head and
// assert the size-mismatch fast-path detects the staleness and
// regenerates the sidecar with sha256 of the new content.
func TestSidecarSelfHealSizeMismatch(t *testing.T) {
	dir := t.TempDir()
	s, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if _, err := s.PutIfAbsent(context.Background(), "rk/torn", bytes.NewReader([]byte("aaa")), nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Out-of-band rewrite content with a different size; sidecar still
	// records the original size. This simulates a crash mid-rewrite.
	objPath := filepath.Join(dir, "objects", "rk", "torn")
	newContent := []byte("BBBBBBBBBB")
	if err := os.WriteFile(objPath, newContent, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	md, err := s.Head(context.Background(), "rk/torn")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if md.Size != int64(len(newContent)) {
		t.Errorf("size after self-heal = %d, want %d", md.Size, len(newContent))
	}
	expectedHash := sha256.Sum256(newContent)
	want := hex.EncodeToString(expectedHash[:])
	if md.Version.Token != want {
		t.Errorf("token after self-heal = %s, want %s (sha256 of new content)", md.Version.Token, want)
	}
}

func TestSymlinkRejection(t *testing.T) {
	dir := t.TempDir()
	s, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Seed a normal object so the bucket has at least one valid entry.
	if _, err := s.PutIfAbsent(context.Background(), "rk/normal", bytes.NewReader([]byte("ok")), nil); err != nil {
		t.Fatalf("seed normal: %v", err)
	}

	// Place a symlink at <root>/objects/rk/symlinked pointing to /etc/hosts.
	target := "/etc/hosts"
	if _, err := os.Stat(target); err != nil {
		t.Skipf("test target %s not present: %v", target, err)
	}
	linkPath := filepath.Join(dir, "objects", "rk", "symlinked")
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink(target, linkPath); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	// Get/Head/GetRange must reject the symlinked key.
	if _, err := s.Get(context.Background(), "rk/symlinked", nil); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("Get(symlink) = %v, want ErrInvalidArgument", err)
	}
	if _, err := s.Head(context.Background(), "rk/symlinked"); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("Head(symlink) = %v, want ErrInvalidArgument", err)
	}
	if _, err := s.GetRange(context.Background(), "rk/symlinked", 0, 0); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("GetRange(symlink) = %v, want ErrInvalidArgument", err)
	}

	// List must skip the symlinked entry but still return the normal one.
	page, err := s.List(context.Background(), "rk/", nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, md := range page.Objects {
		if md.Key == "rk/symlinked" {
			t.Error("List returned a symlinked key; expected it to be skipped")
		}
	}
	foundNormal := false
	for _, md := range page.Objects {
		if md.Key == "rk/normal" {
			foundNormal = true
		}
	}
	if !foundNormal {
		t.Error("List did not return the normal entry alongside the skipped symlink")
	}
}

// TestCASRejectsSymlink asserts PutIfVersionMatches and
// DeleteIfVersionMatches refuse a key whose on-disk path is a
// symlink — without this guard, headLocked would follow the symlink
// via os.Stat/os.Open and hash an external file (e.g. /etc/hosts),
// leaking that hash through the eventual mismatch error.
func TestCASRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	s, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	target := "/etc/hosts"
	if _, err := os.Stat(target); err != nil {
		t.Skipf("test target %s not present: %v", target, err)
	}
	linkPath := filepath.Join(dir, "objects", "rk", "linked")
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink(target, linkPath); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	bogus := storage.ObjectVersion{Provider: "localfs", Token: "deadbeef", Kind: storage.VersionEtag}
	if _, err := s.PutIfVersionMatches(context.Background(), "rk/linked", bogus, bytes.NewReader([]byte("DROP")), nil); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("PutIfVersionMatches(symlink) = %v, want ErrInvalidArgument", err)
	}
	if err := s.DeleteIfVersionMatches(context.Background(), "rk/linked", bogus); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("DeleteIfVersionMatches(symlink) = %v, want ErrInvalidArgument", err)
	}
}

// TestVersionEqualityIsFullStruct asserts that CAS and Get
// preconditions compare the entire ObjectVersion value, not just
// Token. A version with the right token but wrong Provider/Kind must
// still be rejected.
func TestVersionEqualityIsFullStruct(t *testing.T) {
	dir := t.TempDir()
	s, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	v0, err := s.PutIfAbsent(context.Background(), "rk/version-id", bytes.NewReader([]byte("payload")), nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	wrongProvider := storage.ObjectVersion{Provider: "s3", Token: v0.Token, Kind: v0.Kind}
	if _, err := s.PutIfVersionMatches(context.Background(), "rk/version-id", wrongProvider, bytes.NewReader([]byte("X")), nil); !errors.Is(err, storage.ErrVersionMismatch) {
		t.Errorf("PutIfVersionMatches(wrongProvider) = %v, want ErrVersionMismatch", err)
	}

	wrongKind := storage.ObjectVersion{Provider: v0.Provider, Token: v0.Token, Kind: storage.VersionGeneration}
	if err := s.DeleteIfVersionMatches(context.Background(), "rk/version-id", wrongKind); !errors.Is(err, storage.ErrVersionMismatch) {
		t.Errorf("DeleteIfVersionMatches(wrongKind) = %v, want ErrVersionMismatch", err)
	}
}

func TestLocalfs_SignedGetURL_PUT_NotSupported(t *testing.T) {
	dir := t.TempDir()
	l, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	_, err = l.SignedGetURL(context.Background(), "k", storage.SignedURLOptions{Method: "PUT"})
	if !errors.Is(err, storage.ErrNotSupported) {
		t.Fatalf("err = %v, want ErrNotSupported", err)
	}
}

func TestLocalfs_SignedGetURL_RejectsUnknownMethod(t *testing.T) {
	dir := t.TempDir()
	l, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	_, err = l.SignedGetURL(context.Background(), "k", storage.SignedURLOptions{Method: "DELETE"})
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("err = %v, want ErrInvalidArgument", err)
	}
}
