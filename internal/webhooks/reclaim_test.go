package webhooks_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

func TestReclaim_StuckInFlightRowsBecomePending(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := webhooks.New(db)
	ctx := context.Background()
	mustCreateEndpoint(t, svc, ctx, "acme", "site", webhooks.EventPush)

	for i := 0; i < 3; i++ {
		if err := svc.Enqueue(ctx, webhooks.EventPush, "acme", "site", "alice",
			webhooks.PushPayload{TxID: "tx"},
		); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}

	old := time.Now().Add(-5 * time.Minute).Unix()
	young := time.Now().Add(-10 * time.Second).Unix()
	mustExec(t, db, `UPDATE webhook_deliveries SET status='in_flight', last_attempt_at=?, attempts=1
		WHERE id IN (SELECT id FROM webhook_deliveries WHERE status='pending' LIMIT 2)`, old)
	mustExec(t, db, `UPDATE webhook_deliveries SET status='in_flight', last_attempt_at=?, attempts=1
		WHERE status='pending'`, young)

	if err := webhooks.Reclaim(ctx, db, 60*time.Second); err != nil {
		t.Fatalf("Reclaim: %v", err)
	}

	pending := countByStatus(t, db, "pending")
	inFlight := countByStatus(t, db, "in_flight")
	if pending != 2 {
		t.Errorf("pending after Reclaim: %d, want 2", pending)
	}
	if inFlight != 1 {
		t.Errorf("in_flight after Reclaim: %d, want 1", inFlight)
	}

	row := db.QueryRowContext(ctx,
		`SELECT attempts FROM webhook_deliveries WHERE status='pending' LIMIT 1`)
	var attempts int
	if err := row.Scan(&attempts); err != nil {
		t.Fatalf("scan attempts: %v", err)
	}
	if attempts != 1 {
		t.Errorf("reclaimed row attempts=%d, want 1 (preserved)", attempts)
	}
}

func mustCreateEndpoint(t *testing.T, svc *webhooks.Service, ctx context.Context, tenant, repo string, mask webhooks.Event) webhooks.Endpoint {
	t.Helper()
	ep, err := svc.Create(ctx, webhooks.EndpointInput{
		Tenant: tenant, Repo: repo,
		URL: "https://hooks.test/" + tenant + "/" + repo, EventMask: mask,
	})
	if err != nil {
		t.Fatalf("mustCreateEndpoint: %v", err)
	}
	return ep
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func countByStatus(t *testing.T, db *sql.DB, status string) int {
	t.Helper()
	var n int
	row := db.QueryRow(`SELECT COUNT(*) FROM webhook_deliveries WHERE status=?`, status)
	if err := row.Scan(&n); err != nil {
		t.Fatalf("count %s: %v", status, err)
	}
	return n
}
