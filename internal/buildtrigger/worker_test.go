package buildtrigger

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type flakyDeliverer struct {
	failuresLeft int32
	calls        int32
}

func (f *flakyDeliverer) Deliver(ctx context.Context, tr Trigger, p BuildPayload) (int, error) {
	atomic.AddInt32(&f.calls, 1)
	if atomic.AddInt32(&f.failuresLeft, -1) >= 0 {
		return 500, context.DeadlineExceeded
	}
	return 200, nil
}

// waitUntil polls cond every few ms and fails the test if it does not become
// true before timeout elapses.
func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(3 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition not met within %v", timeout)
	}
}

// countByStatus returns the number of build_trigger_deliveries rows in the
// given status. Test-only accessor.
func (s *Service) countByStatus(ctx context.Context, status string) int {
	var n int
	row := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM build_trigger_deliveries WHERE status=?`, status)
	if err := row.Scan(&n); err != nil {
		return -1
	}
	return n
}

func TestWorker_RetriesThenDelivers(t *testing.T) {
	svc, _ := newTestSvc(t)
	ctx := context.Background()
	tr, err := svc.Create(ctx, TriggerInput{Tenant: "acme", Repo: "app", Name: "n",
		Kind: KindGeneric, Config: Config{URL: "https://x"}, RefInclude: []string{"refs/heads/main"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Enqueue(ctx, PushInfo{Tenant: "acme", Repo: "app", HeadOID: "a",
		RefUpdates: []RefUpdate{{Refname: "refs/heads/main", NewOID: "a"}}}); err != nil {
		t.Fatal(err)
	}
	_ = tr
	d := &flakyDeliverer{failuresLeft: 1}
	cfg := WorkerConfig{
		TickInterval: 5 * time.Millisecond, ClaimBatchSize: 16, Concurrency: 2,
		BackoffSchedule: []time.Duration{5 * time.Millisecond, 5 * time.Millisecond, 5 * time.Millisecond},
		Deliverers:      map[Kind]Deliverer{KindGeneric: d},
	}
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	go StartWorker(wctx, svc, cfg)
	waitUntil(t, 3*time.Second, func() bool { return svc.countByStatus(ctx, "delivered") == 1 })
	if c := atomic.LoadInt32(&d.calls); c < 2 {
		t.Fatalf("expected >=2 attempts, got %d", c)
	}
}

func TestWorker_DeadLetterAfterExhaustion(t *testing.T) {
	svc, _ := newTestSvc(t)
	ctx := context.Background()
	if _, err := svc.Create(ctx, TriggerInput{Tenant: "acme", Repo: "app", Name: "n",
		Kind: KindGeneric, Config: Config{URL: "https://x"}, RefInclude: []string{"refs/heads/main"}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Enqueue(ctx, PushInfo{Tenant: "acme", Repo: "app",
		RefUpdates: []RefUpdate{{Refname: "refs/heads/main", NewOID: "a"}}}); err != nil {
		t.Fatal(err)
	}
	d := &flakyDeliverer{failuresLeft: 1 << 30}
	cfg := WorkerConfig{
		TickInterval:    5 * time.Millisecond,
		BackoffSchedule: []time.Duration{2 * time.Millisecond, 2 * time.Millisecond},
		Deliverers:      map[Kind]Deliverer{KindGeneric: d},
	}
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	go StartWorker(wctx, svc, cfg)
	waitUntil(t, 3*time.Second, func() bool { return svc.countByStatus(ctx, "dead_letter") == 1 })
}

// permanentDeliverer always returns a permanent error, recording call count.
type permanentDeliverer struct{ calls int32 }

func (d *permanentDeliverer) Deliver(ctx context.Context, tr Trigger, p BuildPayload) (int, error) {
	atomic.AddInt32(&d.calls, 1)
	return 404, permanentf("HTTP 404")
}

// captureHandler is a concurrency-safe slog.Handler that records emitted records.
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// hasMetricReason reports whether any captured record is a metric with the
// given metric_name and reason attrs.
func (h *captureHandler) hasMetricReason(metricName, reason string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		var name, rsn string
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "metric_name":
				name = a.Value.String()
			case "reason":
				rsn = a.Value.String()
			}
			return true
		})
		if name == metricName && rsn == reason {
			return true
		}
	}
	return false
}

func TestWorker_PermanentErrorDeadLettersImmediately(t *testing.T) {
	svc, _ := newTestSvc(t)
	ctx := context.Background()
	if _, err := svc.Create(ctx, TriggerInput{Tenant: "acme", Repo: "app", Name: "n",
		Kind: KindGeneric, Config: Config{URL: "https://x"}, RefInclude: []string{"refs/heads/main"}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Enqueue(ctx, PushInfo{Tenant: "acme", Repo: "app", HeadOID: "a",
		RefUpdates: []RefUpdate{{Refname: "refs/heads/main", NewOID: "a"}}}); err != nil {
		t.Fatal(err)
	}

	capH := &captureHandler{}
	d := &permanentDeliverer{}
	cfg := WorkerConfig{
		TickInterval: 5 * time.Millisecond, ClaimBatchSize: 16,
		// A long schedule would allow many retries — a permanent error must
		// dead-letter on the first attempt anyway.
		BackoffSchedule: []time.Duration{time.Hour, time.Hour, time.Hour},
		Deliverers:      map[Kind]Deliverer{KindGeneric: d},
		Logger:          slog.New(capH),
	}
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	go StartWorker(wctx, svc, cfg)

	waitUntil(t, 3*time.Second, func() bool { return svc.countByStatus(ctx, "dead_letter") == 1 })
	if c := atomic.LoadInt32(&d.calls); c != 1 {
		t.Fatalf("permanent error must dead-letter after 1 attempt, got %d calls", c)
	}
	if svc.countByStatus(ctx, "pending") != 0 {
		t.Errorf("no row should remain pending after a permanent failure")
	}
	if !capH.hasMetricReason("build_trigger_deadletter_total", "permanent") {
		t.Error("expected build_trigger_deadletter_total with reason=permanent")
	}
}
