package localfs_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// TestCompleteRejectsTokenMismatch asserts CompleteMultipartIfAbsent
// recomputes each part's hash and refuses to assemble when the on-disk
// content does not match the caller-supplied MultipartPart.Token.
func TestCompleteRejectsTokenMismatch(t *testing.T) {
	dir := t.TempDir()
	s, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	mp, err := s.CreateMultipart(context.Background(), "obj/token-mismatch", nil)
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	p, err := mp.UploadPart(context.Background(), 1, bytes.NewReader([]byte("real-bytes")))
	if err != nil {
		t.Fatalf("UploadPart: %v", err)
	}

	// Mutate the caller-side token; the on-disk content remains "real-bytes".
	p.Token = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

	_, err = s.CompleteMultipartIfAbsent(context.Background(), mp, []storage.MultipartPart{p})
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("CompleteMultipartIfAbsent with bogus token = %v, want ErrInvalidArgument", err)
	}
	if err == nil || !strings.Contains(err.Error(), "token mismatch") {
		t.Errorf("error message %q does not mention token mismatch", err)
	}

	// Target key must remain absent.
	if _, err := s.Head(context.Background(), "obj/token-mismatch"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Head after rejected complete = %v, want ErrNotFound", err)
	}
}

// TestStaleUploadHandleRefusedAfterAbort asserts that calling UploadPart
// or CompleteMultipartIfAbsent on an upload handle that has been Abort'd
// returns ErrInvalidArgument rather than reviving the upload.
func TestStaleUploadHandleRefusedAfterAbort(t *testing.T) {
	dir := t.TempDir()
	s, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	mp, err := s.CreateMultipart(context.Background(), "obj/stale-abort", nil)
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	p, err := mp.UploadPart(context.Background(), 1, bytes.NewReader([]byte("first")))
	if err != nil {
		t.Fatalf("UploadPart: %v", err)
	}
	if err := mp.Abort(context.Background()); err != nil {
		t.Fatalf("Abort: %v", err)
	}

	if _, err := mp.UploadPart(context.Background(), 2, bytes.NewReader([]byte("after-abort"))); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("UploadPart after Abort = %v, want ErrInvalidArgument", err)
	}
	if _, err := s.CompleteMultipartIfAbsent(context.Background(), mp, []storage.MultipartPart{p}); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("Complete after Abort = %v, want ErrInvalidArgument", err)
	}
}

// TestStaleUploadHandleRefusedAfterComplete asserts that calling
// UploadPart or CompleteMultipartIfAbsent on a handle whose Complete has
// already succeeded returns ErrInvalidArgument and does not corrupt the
// committed object.
func TestStaleUploadHandleRefusedAfterComplete(t *testing.T) {
	dir := t.TempDir()
	s, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	mp, err := s.CreateMultipart(context.Background(), "obj/stale-complete", nil)
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	p, err := mp.UploadPart(context.Background(), 1, bytes.NewReader([]byte("payload")))
	if err != nil {
		t.Fatalf("UploadPart: %v", err)
	}
	if _, err := s.CompleteMultipartIfAbsent(context.Background(), mp, []storage.MultipartPart{p}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if _, err := mp.UploadPart(context.Background(), 2, bytes.NewReader([]byte("after-complete"))); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("UploadPart after Complete = %v, want ErrInvalidArgument", err)
	}
	if _, err := s.CompleteMultipartIfAbsent(context.Background(), mp, []storage.MultipartPart{p}); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("second Complete = %v, want ErrInvalidArgument", err)
	}
}

// TestStaleUploadHandleRefusedAfterManifestRemoval asserts that an
// upload whose on-disk manifest has been removed out-of-band cannot
// service further calls (the active-state check uses the manifest as
// its cross-process witness).
func TestStaleUploadHandleRefusedAfterManifestRemoval(t *testing.T) {
	dir := t.TempDir()
	s, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	mp, err := s.CreateMultipart(context.Background(), "obj/manifest-gone", nil)
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}

	manifestPath := filepath.Join(dir, "uploads", mp.UploadID(), "manifest.json")
	if err := os.Remove(manifestPath); err != nil {
		t.Fatalf("remove manifest: %v", err)
	}

	if _, err := mp.UploadPart(context.Background(), 1, bytes.NewReader([]byte("x"))); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("UploadPart with manifest gone = %v, want ErrInvalidArgument", err)
	}
}
