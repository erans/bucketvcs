package webhooks_test

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

// newPruneTestService spins up an in-memory authdb + Service. Returns
// (svc, db) so tests can both call Prune and seed/inspect rows.
func newPruneTestService(t *testing.T) (*webhooks.Service, *sql.DB) {
	t.Helper()
	store, err := sqlitestore.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.RegisterRepo(context.Background(), "acme", "site"); err != nil {
		t.Fatal(err)
	}
	svc := webhooks.New(store.DB())
	// Register one endpoint so webhook_deliveries.endpoint_id=1 FK is satisfied.
	if _, err := svc.Create(context.Background(), webhooks.EndpointInput{
		Tenant:    "acme",
		Repo:      "site",
		URL:       "https://example.com/hook",
		EventMask: webhooks.EventMaskAll,
	}); err != nil {
		t.Fatal(err)
	}
	return svc, store.DB()
}

// seedSeq disambiguates seedDelivery ids when multiple rows share status+
// timestamp.
var seedSeq atomic.Uint64

// seedDelivery inserts one row. createdAt is mandatory; deliveredAt and
// lastAttemptAt are 0 for "NULL" semantics in the helper.
func seedDelivery(t *testing.T, db *sql.DB, status string, createdAt, deliveredAt, lastAttemptAt int64) string {
	t.Helper()
	id := fmt.Sprintf("%s-%d-%d", status, createdAt, seedSeq.Add(1))
	var delivered, lastAttempt any
	if deliveredAt != 0 {
		delivered = deliveredAt
	}
	if lastAttemptAt != 0 {
		lastAttempt = lastAttemptAt
	}
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO webhook_deliveries
		    (id, endpoint_id, event_type, payload_json, status, attempts,
		     next_attempt_at, last_attempt_at, last_status_code, last_error,
		     created_at, delivered_at)
		VALUES (?, 1, 'test.event', X'7B7D', ?, 1, ?, ?, NULL, NULL, ?, ?)`,
		id, status, createdAt, lastAttempt, createdAt, delivered)
	if err != nil {
		t.Fatalf("seed %s: %v", status, err)
	}
	return id
}

func TestPrune_OnlyTerminalStatesPastCutoff(t *testing.T) {
	svc, db := newPruneTestService(t)
	now := time.Now().Unix()
	veryOld := now - 10*86400
	recent := now - 60

	// 8 rows: 4 states × {past-cutoff, within-cutoff}.
	seedDelivery(t, db, "pending", veryOld, 0, 0)
	seedDelivery(t, db, "pending", recent, 0, 0)
	seedDelivery(t, db, "in_flight", veryOld, 0, veryOld)
	seedDelivery(t, db, "in_flight", recent, 0, recent)
	seedDelivery(t, db, "delivered", veryOld, veryOld, veryOld)
	seedDelivery(t, db, "delivered", recent, recent, recent)
	seedDelivery(t, db, "dead_letter", veryOld, 0, veryOld)
	seedDelivery(t, db, "dead_letter", recent, 0, recent)

	cfg := webhooks.PruneConfig{
		DeliveredCutoff:  time.Unix(now-86400, 0),
		DeadLetterCutoff: time.Unix(now-86400, 0),
	}
	report, err := svc.Prune(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if report.DeliveredDeleted != 1 {
		t.Errorf("DeliveredDeleted = %d, want 1", report.DeliveredDeleted)
	}
	if report.DeadLetterDeleted != 1 {
		t.Errorf("DeadLetterDeleted = %d, want 1", report.DeadLetterDeleted)
	}

	var pendingInFlight int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM webhook_deliveries WHERE status IN ('pending','in_flight')`,
	).Scan(&pendingInFlight); err != nil {
		t.Fatal(err)
	}
	if pendingInFlight != 4 {
		t.Errorf("active rows after prune = %d, want 4 (never pruned)", pendingInFlight)
	}
}

func TestPrune_DryRunMatchesCountWithoutDeleting(t *testing.T) {
	svc, db := newPruneTestService(t)
	now := time.Now().Unix()
	veryOld := now - 10*86400
	seedDelivery(t, db, "delivered", veryOld, veryOld, veryOld)
	seedDelivery(t, db, "delivered", veryOld, veryOld, veryOld)
	seedDelivery(t, db, "dead_letter", veryOld, 0, veryOld)

	cfg := webhooks.PruneConfig{
		DeliveredCutoff:  time.Unix(now-86400, 0),
		DeadLetterCutoff: time.Unix(now-86400, 0),
		DryRun:           true,
	}
	report, err := svc.Prune(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Prune dry-run: %v", err)
	}
	if report.DeliveredDeleted != 2 || report.DeadLetterDeleted != 1 {
		t.Errorf("dry-run counts = (%d, %d), want (2, 1)",
			report.DeliveredDeleted, report.DeadLetterDeleted)
	}
	var total int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM webhook_deliveries`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Errorf("after dry-run, table has %d rows; want 3 (no deletes)", total)
	}
}

func TestPrune_EmptyTableNoOp(t *testing.T) {
	svc, _ := newPruneTestService(t)
	report, err := svc.Prune(context.Background(), webhooks.PruneConfig{
		DeliveredCutoff:  time.Now(),
		DeadLetterCutoff: time.Now(),
	})
	if err != nil {
		t.Fatalf("Prune empty: %v", err)
	}
	if report.DeliveredDeleted != 0 || report.DeadLetterDeleted != 0 {
		t.Errorf("empty-table counts = (%d, %d), want (0, 0)",
			report.DeliveredDeleted, report.DeadLetterDeleted)
	}
}
