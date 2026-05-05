package pack

import (
	"container/list"
	"sync"
)

// objectCache is a tiny LRU keyed by pack offset, holding decoded
// non-delta objects that may serve as delta bases. Bounded by entry
// count, not bytes; M9 is when this gets serious sizing.
type objectCache struct {
	mu      sync.Mutex
	max     int
	entries map[uint64]*list.Element
	order   *list.List
}

type cacheEntry struct {
	off uint64
	obj *Object
}

func newObjectCache(max int) *objectCache {
	return &objectCache{
		max:     max,
		entries: make(map[uint64]*list.Element, max),
		order:   list.New(),
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
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[off]; ok {
		c.order.MoveToFront(el)
		el.Value.(*cacheEntry).obj = obj
		return
	}
	if c.order.Len() >= c.max {
		back := c.order.Back()
		if back != nil {
			c.order.Remove(back)
			delete(c.entries, back.Value.(*cacheEntry).off)
		}
	}
	el := c.order.PushFront(&cacheEntry{off: off, obj: obj})
	c.entries[off] = el
}
