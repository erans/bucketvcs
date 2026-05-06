package mirror

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestManager_DoubleStartRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("flock test is unix-only")
	}
	root := t.TempDir()
	storeDir := t.TempDir()
	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	mgr1, err := NewManager(root, store)
	if err != nil {
		t.Fatalf("NewManager 1: %v", err)
	}
	t.Cleanup(func() { _ = mgr1.Close() })
	if _, err := NewManager(root, store); err == nil {
		t.Fatalf("NewManager 2: expected error on double start")
	}
}

func TestManager_SameMirrorAcrossOpens(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	store, tenant, repoID := makeImportedRepo(t)

	root := t.TempDir()
	mgr, err := NewManager(root, store)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	m1, err := mgr.Open(context.Background(), tenant, repoID)
	if err != nil {
		t.Fatalf("Open 1: %v", err)
	}
	m2, err := mgr.Open(context.Background(), tenant, repoID)
	if err != nil {
		t.Fatalf("Open 2: %v", err)
	}
	if m1 != m2 {
		t.Fatalf("Open returned different *Mirror values for same (tenant, repo)")
	}
}

func TestMirror_LockUnlockSerializesWriters(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	store, tenant, repoID := makeImportedRepo(t)

	root := t.TempDir()
	mgr, err := NewManager(root, store)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	m, err := mgr.Open(context.Background(), tenant, repoID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Acquire write lock; a second goroutine attempting Lock must block until
	// we Unlock. Sleep briefly to give the goroutine a chance to be scheduled
	// so the "did NOT progress" assertion is meaningful (otherwise a too-fast
	// poll wins purely by being first to run).
	m.Lock()
	gotSecond := make(chan struct{})
	go func() {
		m.Lock()
		defer m.Unlock()
		close(gotSecond)
	}()
	time.Sleep(10 * time.Millisecond)
	select {
	case <-gotSecond:
		t.Fatalf("second Lock acquired before first Unlock")
	default:
	}
	m.Unlock()
	<-gotSecond
}

func TestMirror_RLockAllowsConcurrentReaders(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	store, tenant, repoID := makeImportedRepo(t)

	root := t.TempDir()
	mgr, err := NewManager(root, store)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	m, err := mgr.Open(context.Background(), tenant, repoID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	m.RLock()
	defer m.RUnlock()
	// Second RLock from another goroutine should NOT block (RWMutex semantics).
	got := make(chan struct{})
	go func() {
		m.RLock()
		defer m.RUnlock()
		close(got)
	}()
	<-got // would deadlock if RLock blocked on first RLock
}
