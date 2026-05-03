package localfs_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// TestVerifyClean: Verify on a healthy open bucket is a no-op success.
func TestVerifyClean(t *testing.T) {
	dir := t.TempDir()
	s, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	for i := 0; i < 5; i++ {
		key := "rk/clean-" + string(rune('a'+i))
		if _, err := s.PutIfAbsent(context.Background(), key, bytes.NewReader([]byte("payload-"+key)), nil); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	if err := s.Verify(context.Background()); err != nil {
		t.Errorf("Verify on clean bucket = %v, want nil", err)
	}
}

// TestVerifyAbsentLockNoOp: package-level Verify against a bucket
// with no .lock returns nil immediately and does NOT mutate any
// sidecar. Periodic maintenance on a clean bucket is the
// instance-method's job; package-level Verify is exclusively for
// recovery from unclean shutdown (i.e., when there IS a stale lock).
func TestVerifyAbsentLockNoOp(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "objects", "rk"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	objPath := filepath.Join(dir, "objects", "rk", "stale")
	metaPath := objPath + ".meta"
	if err := os.WriteFile(objPath, []byte("CONTENT"), 0o644); err != nil {
		t.Fatalf("WriteFile content: %v", err)
	}
	staleSidecar := []byte(`{"version":1,"sha256":"deadbeef","size":7,"content_type":"","modified_at":"2026-05-03T12:00:00Z"}`)
	if err := os.WriteFile(metaPath, staleSidecar, 0o644); err != nil {
		t.Fatalf("WriteFile sidecar: %v", err)
	}

	if err := localfs.Verify(context.Background(), dir); err != nil {
		t.Errorf("Verify on absent-lock bucket = %v, want nil", err)
	}

	got, _ := os.ReadFile(metaPath)
	if !bytes.Equal(got, staleSidecar) {
		t.Errorf("Verify mutated the sidecar despite absent lock; got %q, want %q", got, staleSidecar)
	}
}

// TestVerifyReconcilesSameSize: the case Verify exists for. Same-size
// post-crash torn state — sidecar sha256 disagrees with content but the
// sizes match, so the read-path fast-path cannot detect it.
func TestVerifyReconcilesSameSize(t *testing.T) {
	dir := setupBucketAndOrphanLock(t)

	original := []byte("aaaaaaaa")
	objPath := filepath.Join(dir, "objects", "rk", "torn")
	metaPath := objPath + ".meta"
	if err := os.MkdirAll(filepath.Dir(objPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(objPath, original, 0o644); err != nil {
		t.Fatalf("WriteFile content: %v", err)
	}
	originalHash := sha256.Sum256(original)
	originalSidecar := []byte(`{"version":1,"sha256":"` + hex.EncodeToString(originalHash[:]) + `","size":` + itoa(len(original)) + `,"content_type":"","modified_at":"2026-05-03T12:00:00Z"}`)
	if err := os.WriteFile(metaPath, originalSidecar, 0o644); err != nil {
		t.Fatalf("WriteFile sidecar: %v", err)
	}

	tornContent := []byte("BBBBBBBB")
	if len(tornContent) != len(original) {
		t.Fatal("test fixture: content sizes must match")
	}
	if err := os.WriteFile(objPath, tornContent, 0o644); err != nil {
		t.Fatalf("WriteFile torn: %v", err)
	}

	if err := localfs.Verify(context.Background(), dir, localfs.WithForce(true)); err != nil {
		t.Fatalf("package Verify: %v", err)
	}

	s, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("reopen after Verify: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	md, err := s.Head(context.Background(), "rk/torn")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	expected := sha256.Sum256(tornContent)
	want := hex.EncodeToString(expected[:])
	if md.Version.Token != want {
		t.Errorf("token after Verify = %s, want %s (sha256 of NEW content)", md.Version.Token, want)
	}
}

// TestVerifyDifferentSizeTorn: size-mismatched torn sidecar.
func TestVerifyDifferentSizeTorn(t *testing.T) {
	dir := setupBucketAndOrphanLock(t)
	objPath := filepath.Join(dir, "objects", "rk", "diff")
	metaPath := objPath + ".meta"
	if err := os.MkdirAll(filepath.Dir(objPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(objPath, []byte("longer-content-now"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	stale := `{"version":1,"sha256":"deadbeef","size":8,"content_type":"","modified_at":"2026-05-03T12:00:00Z"}`
	if err := os.WriteFile(metaPath, []byte(stale), 0o644); err != nil {
		t.Fatalf("WriteFile meta: %v", err)
	}

	if err := localfs.Verify(context.Background(), dir, localfs.WithForce(true)); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	scBytes, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("ReadFile meta after Verify: %v", err)
	}
	if !strings.Contains(string(scBytes), `"size":18`) {
		t.Errorf("size-mismatched sidecar not reconciled: %s", scBytes)
	}
}

// TestVerifyMissingSidecar.
func TestVerifyMissingSidecar(t *testing.T) {
	dir := setupBucketAndOrphanLock(t)
	objPath := filepath.Join(dir, "objects", "rk", "missing-meta")
	if err := os.MkdirAll(filepath.Dir(objPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(objPath, []byte("payload"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := localfs.Verify(context.Background(), dir, localfs.WithForce(true)); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if _, err := os.Stat(objPath + ".meta"); err != nil {
		t.Errorf("sidecar not created: %v", err)
	}
}

// TestVerifyParseBrokenSidecar.
func TestVerifyParseBrokenSidecar(t *testing.T) {
	dir := setupBucketAndOrphanLock(t)
	objPath := filepath.Join(dir, "objects", "rk", "broken-meta")
	if err := os.MkdirAll(filepath.Dir(objPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(objPath, []byte("payload"), 0o644); err != nil {
		t.Fatalf("WriteFile content: %v", err)
	}
	if err := os.WriteFile(objPath+".meta", []byte("{this is not json"), 0o644); err != nil {
		t.Fatalf("WriteFile broken meta: %v", err)
	}

	if err := localfs.Verify(context.Background(), dir, localfs.WithForce(true)); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	scBytes, _ := os.ReadFile(objPath + ".meta")
	if !strings.Contains(string(scBytes), `"version":1`) {
		t.Errorf("broken sidecar not rewritten: %s", scBytes)
	}
}

// TestVerifyOrphanSidecar: sidecar present but content missing — Verify
// removes the orphan.
func TestVerifyOrphanSidecar(t *testing.T) {
	dir := setupBucketAndOrphanLock(t)
	metaPath := filepath.Join(dir, "objects", "rk", "orphan.meta")
	if err := os.MkdirAll(filepath.Dir(metaPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	stale := `{"version":1,"sha256":"x","size":0,"content_type":"","modified_at":"2026-05-03T12:00:00Z"}`
	if err := os.WriteFile(metaPath, []byte(stale), 0o644); err != nil {
		t.Fatalf("WriteFile orphan: %v", err)
	}

	if err := localfs.Verify(context.Background(), dir, localfs.WithForce(true)); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if _, err := os.Stat(metaPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("orphan sidecar not removed: stat err = %v", err)
	}
}

// TestVerifyRefusesLiveLock.
func TestVerifyRefusesLiveLock(t *testing.T) {
	dir := t.TempDir()
	s, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	err = localfs.Verify(context.Background(), dir)
	if !errors.Is(err, localfs.ErrLockedByLiveProcess) {
		t.Errorf("Verify against live lock = %v, want ErrLockedByLiveProcess", err)
	}
}

// TestVerifyForceOverridesLiveLock.
func TestVerifyForceOverridesLiveLock(t *testing.T) {
	dir := t.TempDir()
	s, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if _, err := s.PutIfAbsent(context.Background(), "rk/x", bytes.NewReader([]byte("y")), nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := localfs.Verify(context.Background(), dir, localfs.WithForce(true)); err != nil {
		t.Errorf("Verify with WithForce on live lock = %v, want nil (operator owns safety)", err)
	}
}

// TestVerifyClearsDeadLock.
func TestVerifyClearsDeadLock(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "objects"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	deadPID := -99999
	host, _ := os.Hostname()
	lockBytes, _ := json.Marshal(map[string]any{
		"pid":         deadPID,
		"host":        host,
		"acquired_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err := os.WriteFile(filepath.Join(dir, ".lock"), lockBytes, 0o644); err != nil {
		t.Fatalf("WriteFile lockfile: %v", err)
	}

	if err := localfs.Verify(context.Background(), dir); err != nil {
		t.Fatalf("Verify against dead lock: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".lock")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lockfile not cleared after Verify: stat err = %v", err)
	}
}

// TestVerifyCancellation.
func TestVerifyCancellation(t *testing.T) {
	dir := t.TempDir()
	s, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	for i := 0; i < 100; i++ {
		key := "rk/cancel/" + string(rune('a'+i%26)) + "/" + string(rune('0'+i%10))
		_, _ = s.PutIfAbsent(context.Background(), key, bytes.NewReader([]byte{byte(i)}), nil)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Verify(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Verify with cancelled ctx = %v, want context.Canceled", err)
	}
}

// TestVerifySymlinkSkipped.
func TestVerifySymlinkSkipped(t *testing.T) {
	dir := setupBucketAndOrphanLock(t)
	target := "/etc/hosts"
	if _, err := os.Stat(target); err != nil {
		t.Skipf("target %s missing: %v", target, err)
	}
	linkPath := filepath.Join(dir, "objects", "rk", "symlinked")
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink(target, linkPath); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	if err := localfs.Verify(context.Background(), dir, localfs.WithForce(true)); err != nil {
		t.Errorf("Verify with symlink: %v", err)
	}
	if _, err := os.Stat(linkPath + ".meta"); err == nil {
		t.Errorf("Verify created a sidecar for a symlink (should have skipped)")
	}
}

// TestVerifyDetectsLockChange (AD13).
func TestVerifyDetectsLockChange(t *testing.T) {
	dir := setupBucketAndOrphanLock(t)
	for i := 0; i < 5; i++ {
		objPath := filepath.Join(dir, "objects", "rk", "lc-"+string(rune('a'+i)))
		if err := os.MkdirAll(filepath.Dir(objPath), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(objPath, []byte("payload"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	lockPath := filepath.Join(dir, ".lock")
	mutateOnce := false
	progressCb := func(processed int) {
		if mutateOnce {
			return
		}
		mutateOnce = true
		host, _ := os.Hostname()
		newLock, _ := json.Marshal(map[string]any{
			"pid":         os.Getpid(),
			"host":        host,
			"acquired_at": time.Now().UTC().Format(time.RFC3339),
		})
		if err := os.WriteFile(lockPath, newLock, 0o644); err != nil {
			t.Fatalf("rewrite lockfile: %v", err)
		}
	}

	err := localfs.Verify(context.Background(), dir,
		localfs.WithForce(true),
		localfs.WithProgress(progressCb),
	)
	if !errors.Is(err, localfs.ErrLockedByLiveProcess) {
		t.Errorf("Verify with lock changed mid-flight = %v, want ErrLockedByLiveProcess", err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Errorf("lockfile removed despite mid-flight change: %v", err)
	}
}

// TestVerifyPIDReusedConservative (AD12).
func TestVerifyPIDReusedConservative(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "objects"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	host, _ := os.Hostname()
	lockBytes, _ := json.Marshal(map[string]any{
		"pid":         os.Getpid(),
		"host":        host,
		"acquired_at": time.Now().UTC().Format(time.RFC3339),
	})
	lockPath := filepath.Join(dir, ".lock")
	if err := os.WriteFile(lockPath, lockBytes, 0o644); err != nil {
		t.Fatalf("WriteFile lockfile: %v", err)
	}

	if err := localfs.Verify(context.Background(), dir); !errors.Is(err, localfs.ErrLockedByLiveProcess) {
		t.Errorf("Verify against live PID without force = %v, want ErrLockedByLiveProcess", err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Errorf("lockfile removed without WithForce: %v", err)
	}

	if err := localfs.Verify(context.Background(), dir, localfs.WithForce(true)); err != nil {
		t.Fatalf("Verify with force = %v, want nil", err)
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lockfile not removed with WithForce: %v", err)
	}
}

// setupBucketAndOrphanLock creates a fresh dir with objects/ and an
// orphan lockfile pointing at a definitely-dead PID.
func setupBucketAndOrphanLock(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "objects"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	host, _ := os.Hostname()
	lockBytes, _ := json.Marshal(map[string]any{
		"pid":         -99999,
		"host":        host,
		"acquired_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err := os.WriteFile(filepath.Join(dir, ".lock"), lockBytes, 0o644); err != nil {
		t.Fatalf("WriteFile lockfile: %v", err)
	}
	return dir
}

// itoa avoids importing strconv just for one call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
