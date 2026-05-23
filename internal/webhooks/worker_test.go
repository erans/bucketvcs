package webhooks_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

func TestWorker_DeliversOn2xx(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := webhooks.New(db)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		mu        sync.Mutex
		received  [][]byte
		sigHeader []string
		evtHeader []string
		delivIDs  []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		received = append(received, body)
		sigHeader = append(sigHeader, r.Header.Get("BucketVCS-Signature"))
		evtHeader = append(evtHeader, r.Header.Get("X-BucketVCS-Event"))
		delivIDs = append(delivIDs, r.Header.Get("X-BucketVCS-Delivery-ID"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if _, err := svc.Create(ctx, webhooks.EndpointInput{
		Tenant: "acme", Repo: "site",
		URL: srv.URL, EventMask: webhooks.EventPush,
	}); err != nil {
		t.Fatalf("Create endpoint: %v", err)
	}
	if err := svc.Enqueue(ctx, webhooks.EventPush, "acme", "site", "alice",
		webhooks.PushPayload{TxID: "tx-1", ManifestVersion: 42, StorageBackend: "localfs"},
	); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	cfg := webhooks.WorkerConfig{
		TickInterval:   50 * time.Millisecond,
		ClaimBatchSize: 32,
		Concurrency:    8,
		HTTPTimeout:    1 * time.Second,
	}
	go webhooks.StartWorker(ctx, svc, cfg)

	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(received) == 1
	}, "receiver did not get the delivery")

	mu.Lock()
	defer mu.Unlock()
	if !strings.HasPrefix(sigHeader[0], "t=") {
		t.Errorf("BucketVCS-Signature header missing t= prefix: %q", sigHeader[0])
	}
	if evtHeader[0] != "push" {
		t.Errorf("X-BucketVCS-Event=%q, want push", evtHeader[0])
	}
	if delivIDs[0] == "" {
		t.Errorf("X-BucketVCS-Delivery-ID is empty")
	}
	if !strings.Contains(string(received[0]), `"tx_id":"tx-1"`) {
		t.Errorf("body missing tx_id field: %s", received[0])
	}
}

func TestWorker_RetriesOn5xxThenDeliveredOn2xx(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := webhooks.New(db)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if _, err := svc.Create(ctx, webhooks.EndpointInput{
		Tenant: "acme", Repo: "site",
		URL: srv.URL, EventMask: webhooks.EventPush,
	}); err != nil {
		t.Fatalf("Create endpoint: %v", err)
	}
	if err := svc.Enqueue(ctx, webhooks.EventPush, "acme", "site", "alice",
		webhooks.PushPayload{TxID: "tx-1"},
	); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	cfg := webhooks.WorkerConfig{
		TickInterval:   25 * time.Millisecond,
		ClaimBatchSize: 32,
		Concurrency:    4,
		HTTPTimeout:    1 * time.Second,
		BackoffSchedule: []time.Duration{
			10 * time.Millisecond, 10 * time.Millisecond,
			10 * time.Millisecond, 10 * time.Millisecond,
		},
		BackoffJitterFrac: 0,
	}
	go webhooks.StartWorker(ctx, svc, cfg)

	waitFor(t, 3*time.Second, func() bool {
		return attempts.Load() >= 3
	}, "receiver did not get 3 attempts")

	waitFor(t, 2*time.Second, func() bool {
		var status string
		row := db.QueryRowContext(ctx, `SELECT status FROM webhook_deliveries LIMIT 1`)
		_ = row.Scan(&status)
		return status == "delivered"
	}, "row did not become delivered")
}

func TestWorker_DeadLettersAfter5Attempts(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := webhooks.New(db)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := svc.Create(ctx, webhooks.EndpointInput{
		Tenant: "acme", Repo: "site",
		URL: srv.URL, EventMask: webhooks.EventPush,
	}); err != nil {
		t.Fatalf("Create endpoint: %v", err)
	}
	if err := svc.Enqueue(ctx, webhooks.EventPush, "acme", "site", "alice",
		webhooks.PushPayload{TxID: "tx-1"},
	); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	cfg := webhooks.WorkerConfig{
		TickInterval:   25 * time.Millisecond,
		ClaimBatchSize: 32,
		Concurrency:    4,
		HTTPTimeout:    1 * time.Second,
		BackoffSchedule: []time.Duration{
			10 * time.Millisecond, 10 * time.Millisecond,
			10 * time.Millisecond, 10 * time.Millisecond,
		},
		BackoffJitterFrac: 0,
	}
	go webhooks.StartWorker(ctx, svc, cfg)

	waitFor(t, 3*time.Second, func() bool {
		var status string
		var attempts int
		row := db.QueryRowContext(ctx, `SELECT status, attempts FROM webhook_deliveries LIMIT 1`)
		_ = row.Scan(&status, &attempts)
		return status == "dead_letter" && attempts == 5
	}, "row did not become dead_letter with attempts=5")
}

func TestWorker_StableDeliveryIDAndBodyAcrossRetries(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := webhooks.New(db)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		mu        sync.Mutex
		delivIDs  []string
		gotBody   [][]byte
		failCount atomic.Int32
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		delivIDs = append(delivIDs, r.Header.Get("X-BucketVCS-Delivery-ID"))
		gotBody = append(gotBody, body)
		mu.Unlock()
		if failCount.Add(1) < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if _, err := svc.Create(ctx, webhooks.EndpointInput{
		Tenant: "acme", Repo: "site",
		URL: srv.URL, EventMask: webhooks.EventPush,
	}); err != nil {
		t.Fatalf("Create endpoint: %v", err)
	}
	if err := svc.Enqueue(ctx, webhooks.EventPush, "acme", "site", "alice",
		webhooks.PushPayload{TxID: "tx-1"},
	); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	cfg := webhooks.WorkerConfig{
		TickInterval:   25 * time.Millisecond,
		ClaimBatchSize: 32,
		Concurrency:    4,
		HTTPTimeout:    1 * time.Second,
		BackoffSchedule: []time.Duration{
			10 * time.Millisecond, 10 * time.Millisecond,
			10 * time.Millisecond, 10 * time.Millisecond,
		},
		BackoffJitterFrac: 0,
	}
	go webhooks.StartWorker(ctx, svc, cfg)

	waitFor(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(delivIDs) == 2
	}, "did not get 2 attempts")

	mu.Lock()
	defer mu.Unlock()
	if delivIDs[0] != delivIDs[1] {
		t.Errorf("delivery IDs differ across retries: %q vs %q", delivIDs[0], delivIDs[1])
	}
	if string(gotBody[0]) != string(gotBody[1]) {
		t.Errorf("body changed across retries:\n  attempt 1: %s\n  attempt 2: %s", gotBody[0], gotBody[1])
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting: %s", msg)
}
