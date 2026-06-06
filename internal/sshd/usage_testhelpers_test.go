package sshd

import (
	"sync"

	"github.com/bucketvcs/bucketvcs/internal/shiplog"
)

// usageSinkRec is a fake UsageSink that records every UsageEvent it receives.
// Used by the SSH usage-instrumentation e2e test. Concurrency-safe because
// emissions happen on the server's per-session goroutines.
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

func (s *usageSinkRec) byKind(kind string) []shiplog.UsageEvent {
	var out []shiplog.UsageEvent
	for _, ev := range s.events() {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}
