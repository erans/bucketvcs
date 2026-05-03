package localfs

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestKeyedMutexSerializesPerKey(t *testing.T) {
	km := newKeyedMutex()
	var counter int64
	var wg sync.WaitGroup
	const n = 32
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			km.lock("k1")
			defer km.unlock("k1")
			cur := atomic.AddInt64(&counter, 1)
			if cur != 1 {
				t.Errorf("concurrent holders for the same key: counter=%d", cur)
			}
			atomic.AddInt64(&counter, -1)
		}()
	}
	wg.Wait()
}

func TestKeyedMutexDifferentKeysIndependent(t *testing.T) {
	km := newKeyedMutex()
	km.lock("a")
	defer km.unlock("a")

	done := make(chan struct{})
	go func() {
		km.lock("b")
		km.unlock("b")
		close(done)
	}()
	select {
	case <-done:
		// ok — different keys did not block each other
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for lock on different key (would have been a deadlock)")
	}
}
