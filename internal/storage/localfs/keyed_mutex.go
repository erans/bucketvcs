package localfs

import "sync"

// keyedMutex is a map of mutexes keyed by string. lock/unlock provide
// per-key serialization without serializing across keys.
//
// The map itself is protected by an RWMutex: lock/unlock take the read
// lock to look up or create the per-key mutex. Map growth (creating a
// new entry) takes the write lock briefly. Entries are never removed in
// M0; if memory pressure becomes an issue in production we revisit
// eviction in M9.
type keyedMutex struct {
	mu    sync.RWMutex
	locks map[string]*sync.Mutex
}

func newKeyedMutex() *keyedMutex {
	return &keyedMutex{locks: make(map[string]*sync.Mutex)}
}

func (km *keyedMutex) get(key string) *sync.Mutex {
	km.mu.RLock()
	m, ok := km.locks[key]
	km.mu.RUnlock()
	if ok {
		return m
	}
	km.mu.Lock()
	defer km.mu.Unlock()
	if m, ok := km.locks[key]; ok {
		return m
	}
	m = &sync.Mutex{}
	km.locks[key] = m
	return m
}

func (km *keyedMutex) lock(key string) {
	km.get(key).Lock()
}

func (km *keyedMutex) unlock(key string) {
	km.get(key).Unlock()
}
