package pack

import (
	"container/list"
	"sync"
)

// objectCache is a tiny LRU keyed by pack offset, holding decoded
// non-delta objects that may serve as delta bases. Bounded by entry
// count and a per-entry byte threshold: objects larger than maxBytes
// are never inserted, preventing large blobs from exhausting heap.
type objectCache struct {
	mu         sync.Mutex
	maxEntries int
	maxBytes   int64 // skip caching objects larger than this; 0 = no limit
	entries    map[uint64]*list.Element
	order      *list.List
}

type cacheEntry struct {
	off uint64
	obj *Object
}

func newObjectCache(maxEntries int, maxBytes int64) *objectCache {
	return &objectCache{
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
		entries:    make(map[uint64]*list.Element, maxEntries),
		order:      list.New(),
	}
}

func (c *objectCache) get(off uint64) (*Object, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[off]; ok {
		c.order.MoveToFront(el)
		return el.Value.(*cacheEntry).obj, true
	}
	return nil, false
}

func (c *objectCache) put(off uint64, obj *Object) {
	if c.maxBytes > 0 && obj.Size > c.maxBytes {
		// Object too large — don't cache (size-based DoS guard).
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[off]; ok {
		c.order.MoveToFront(el)
		el.Value.(*cacheEntry).obj = obj
		return
	}
	if c.order.Len() >= c.maxEntries {
		back := c.order.Back()
		if back != nil {
			c.order.Remove(back)
			delete(c.entries, back.Value.(*cacheEntry).off)
		}
	}
	el := c.order.PushFront(&cacheEntry{off: off, obj: obj})
	c.entries[off] = el
}
