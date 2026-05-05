package pack

import (
	"container/list"
	"sync"
)

// objectCache is a tiny LRU keyed by pack offset, holding decoded
// non-delta objects that may serve as delta bases. Bounded by entry
// count, a per-entry byte threshold (objects larger than maxObjBytes
// are never inserted), and a total byte budget (LRU eviction keeps
// the aggregate under maxTotal).
type objectCache struct {
	mu          sync.Mutex
	maxEntries  int
	maxObjBytes int64 // skip caching objects larger than this; 0 = no limit
	maxTotal    int64 // total bytes budget across all entries; 0 = no limit
	totalBytes  int64
	entries     map[uint64]*list.Element
	order       *list.List
}

type cacheEntry struct {
	off uint64
	obj *Object
}

func newObjectCache(maxEntries int, maxObjBytes, maxTotal int64) *objectCache {
	return &objectCache{
		maxEntries:  maxEntries,
		maxObjBytes: maxObjBytes,
		maxTotal:    maxTotal,
		entries:     make(map[uint64]*list.Element, maxEntries),
		order:       list.New(),
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
	if c.maxEntries <= 0 {
		return
	}
	if c.maxObjBytes > 0 && obj.Size > c.maxObjBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// If already present, update size delta.
	if el, ok := c.entries[off]; ok {
		old := el.Value.(*cacheEntry).obj
		c.totalBytes -= old.Size
		el.Value.(*cacheEntry).obj = obj
		c.totalBytes += obj.Size
		c.order.MoveToFront(el)
		c.evictUntilFits()
		return
	}
	// Evict oldest until both entry-count and byte-budget limits would be satisfied
	// after adding this new entry.
	for c.order.Len() >= c.maxEntries ||
		(c.maxTotal > 0 && c.totalBytes+obj.Size > c.maxTotal) {
		back := c.order.Back()
		if back == nil {
			break
		}
		evicted := back.Value.(*cacheEntry).obj
		c.totalBytes -= evicted.Size
		c.order.Remove(back)
		delete(c.entries, back.Value.(*cacheEntry).off)
	}
	el := c.order.PushFront(&cacheEntry{off: off, obj: obj})
	c.entries[off] = el
	c.totalBytes += obj.Size
}

// evictUntilFits drops LRU entries until the byte budget is satisfied.
// Used when an existing entry is updated to a larger object.
func (c *objectCache) evictUntilFits() {
	for c.maxTotal > 0 && c.totalBytes > c.maxTotal {
		back := c.order.Back()
		if back == nil {
			return
		}
		evicted := back.Value.(*cacheEntry).obj
		c.totalBytes -= evicted.Size
		c.order.Remove(back)
		delete(c.entries, back.Value.(*cacheEntry).off)
	}
}
