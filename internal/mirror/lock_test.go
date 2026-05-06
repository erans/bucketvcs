package mirror

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

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

	m.Lock()
	var inside atomic.Int32
	done := make(chan struct{})
	go func() {
		m.Lock()
		inside.Add(1)
		m.Unlock()
		close(done)
	}()
	// Yield repeatedly to give the goroutine a chance to be scheduled.
	for i := 0; i < 100; i++ {
		runtime.Gosched()
	}
	if got := inside.Load(); got != 0 {
		t.Fatalf("second Lock acquired before first Unlock (inside=%d)", got)
	}
	m.Unlock()
	<-done
	if got := inside.Load(); got != 1 {
		t.Fatalf("second goroutine never entered: inside=%d", got)
	}
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

func TestMirror_ConcurrentSyncDoesNotRaceOnRebuild(t *testing.T) {
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

	// Cold path: first Open materializes.
	if _, err := mgr.Open(context.Background(), tenant, repoID); err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Force stale by corrupting the sentinel.
	versPath := filepath.Join(root, tenant, repoID, "manifest_version.txt")
	if err := os.WriteFile(versPath, []byte(`{"manifest_version":99999,"latest_tx":"FORCESTALE0000000000000000"}`), 0o644); err != nil {
		t.Fatalf("corrupt sentinel: %v", err)
	}

	// Two concurrent Opens both detect stale and try to rebuild.
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := mgr.Open(context.Background(), tenant, repoID)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Open errored (rebuild race): %v", err)
		}
	}
	// Final state: a clean bare repo with the correct sentinel.
	got, err := os.ReadFile(versPath)
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	if string(got) == `{"manifest_version":99999,"latest_tx":"FORCESTALE0000000000000000"}` {
		t.Fatalf("sentinel not rewritten after rebuild")
	}
}
