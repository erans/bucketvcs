package lfs

import (
	"sync"

	"github.com/bucketvcs/bucketvcs/internal/shiplog"
)

// usageSinkRec is a fake UsageSink that records every UsageEvent it receives.
// Shared across the LFS usage-instrumentation tests (batch download, verify
// upload). Concurrency-safe.
type usageSinkRec struct {
	mu  sync.Mutex
	evs []shiplog.UsageEvent
}

func (s *usageSinkRec) Usage(ev shiplog.UsageEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evs = append(s.evs, ev)
}

func (s *usageSinkRec) events() []shiplog.UsageEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]shiplog.UsageEvent(nil), s.evs...)
}
