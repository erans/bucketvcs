package webhooks

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
// The threshold MUST be well above the worker's POST timeout + drain window
// to avoid double-delivery: a row currently being POSTed by a live worker
// would otherwise be reclaimed and re-attempted.
func Reclaim(ctx context.Context, db sqlitestore.Querier, threshold time.Duration) error {
	cutoff := time.Now().Add(-threshold).Unix()
	_, err := db.ExecContext(ctx,
		`UPDATE webhook_deliveries
		   SET status='pending'
		 WHERE status='in_flight' AND last_attempt_at < ?`,
		cutoff,
	)
	if err != nil {
		return fmt.Errorf("webhooks: reclaim: %w", err)
	}
	return nil
}
