package buildtrigger

import (
	"context"
	"fmt"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

// Reclaim returns rows with status='in_flight' whose last_attempt_at is older
// than the threshold back to status='pending'. Called once at worker startup
// AND periodically from the worker loop (every reclaimEveryNTicks ticks ≈ 1
// minute at the default 1 s tick interval) to recover from crashes that left
// rows mid-delivery, plus in-process failures where recordResult's UPDATE
// failed after a context cancel or sqlite-busy. attempts is preserved.
//
// The threshold MUST be well above the worker's attempt timeout to avoid
// double-delivery: a row currently being delivered by a live worker would
// otherwise be reclaimed and re-attempted.
func Reclaim(ctx context.Context, db sqlitestore.Querier, threshold time.Duration) error {
	cutoff := time.Now().Add(-threshold).Unix()
	_, err := db.ExecContext(ctx,
		`UPDATE build_trigger_deliveries
		   SET status='pending'
		 WHERE status='in_flight' AND last_attempt_at < ?`,
		cutoff,
	)
	if err != nil {
		return fmt.Errorf("buildtrigger: reclaim: %w", err)
	}
	return nil
}

// DeadLetterOrphans dead-letters pending deliveries whose trigger has been
// removed. build_trigger_deliveries deliberately does NOT FK to build_triggers
// (design §2.2), so removing a trigger leaves its deliveries to drain; the claim
// path INNER-JOINs build_triggers and therefore can never claim an orphaned
// row. Without this sweep such a row leaks as permanently pending (design §7:
// "Missing trigger for an orphaned delivery → dead-letter").
//
// Only status='pending' rows are swept; in_flight rows are being worked by a
// live worker and are recovered by Reclaim, not here. attempts is preserved.
// Returns the number of rows dead-lettered. Called at worker startup AND
// periodically from the worker loop, alongside Reclaim.
func DeadLetterOrphans(ctx context.Context, db sqlitestore.Querier) (int64, error) {
	now := time.Now().Unix()
	res, err := db.ExecContext(ctx,
		`UPDATE build_trigger_deliveries
		   SET status='dead_letter', last_error='trigger removed', next_attempt_at=?
		 WHERE status='pending'
		   AND trigger_id NOT IN (SELECT id FROM build_triggers)`,
		now,
	)
	if err != nil {
		return 0, fmt.Errorf("buildtrigger: dead-letter orphans: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("buildtrigger: dead-letter orphans rows affected: %w", err)
	}
	return n, nil
}
