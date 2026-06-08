package buildtrigger

import (
	"context"
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
