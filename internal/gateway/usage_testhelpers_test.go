package gateway

import (
	"sync"

	"github.com/bucketvcs/bucketvcs/internal/shiplog"
)

// sinkRec is a fake UsageSink that records every UsageEvent it receives.
// Shared across the gateway usage-instrumentation tests (upload-pack,
// receive-pack, proxied bundle/pack). Concurrency-safe because the
// upload-pack/receive-pack engine paths may emit from goroutines under
// httptest.
type sinkRec struct {
	mu  sync.Mutex
	evs []shiplog.UsageEvent
}

func (s *sinkRec) Usage(ev shiplog.UsageEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evs = append(s.evs, ev)
}

// events returns a copy of the recorded events under lock.
func (s *sinkRec) events() []shiplog.UsageEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]shiplog.UsageEvent(nil), s.evs...)
}
