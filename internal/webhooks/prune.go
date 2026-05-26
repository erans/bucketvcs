package webhooks

import (
	"context"
	"fmt"
	"time"
)

// PruneConfig parameterizes a single prune sweep.
type PruneConfig struct {
	// DeliveredCutoff: rows with status='delivered' AND delivered_at < this
	// are deleted.
	DeliveredCutoff time.Time
	// DeadLetterCutoff: rows with status='dead_letter' AND last_attempt_at <
	// this are deleted.
	DeadLetterCutoff time.Time
	// DryRun: if true, count rows that WOULD be deleted but DO NOT delete.
	// Implemented as two SELECT COUNT queries instead of DELETE statements.
	DryRun bool
}

// PruneReport summarizes a Prune call.
type PruneReport struct {
	DeliveredDeleted  int64
	DeadLetterDeleted int64
}

// Prune sweeps terminal-state webhook delivery rows past their retention
// window. Never touches `pending` or `in_flight`.
//
// Two separate statements per call (one for delivered, one for dead_letter)
// so the per-status counts are exact via RowsAffected. SQLite serializes
// the DELETEs at the connection level; partial commit is not possible
// within either statement.
func (s *Service) Prune(ctx context.Context, cfg PruneConfig) (PruneReport, error) {
	var r PruneReport
	if cfg.DryRun {
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM webhook_deliveries
			 WHERE status='delivered' AND delivered_at < ?`,
			cfg.DeliveredCutoff.Unix(),
		).Scan(&r.DeliveredDeleted); err != nil {
			return r, fmt.Errorf("webhooks.Prune dry-run delivered: %w", err)
		}
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM webhook_deliveries
			 WHERE status='dead_letter' AND last_attempt_at < ?`,
			cfg.DeadLetterCutoff.Unix(),
		).Scan(&r.DeadLetterDeleted); err != nil {
			return r, fmt.Errorf("webhooks.Prune dry-run dead_letter: %w", err)
		}
		return r, nil
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM webhook_deliveries
		 WHERE status='delivered' AND delivered_at < ?`,
		cfg.DeliveredCutoff.Unix())
	if err != nil {
		return r, fmt.Errorf("webhooks.Prune delivered: %w", err)
	}
	r.DeliveredDeleted, _ = res.RowsAffected()

	res, err = s.db.ExecContext(ctx,
		`DELETE FROM webhook_deliveries
		 WHERE status='dead_letter' AND last_attempt_at < ?`,
		cfg.DeadLetterCutoff.Unix())
	if err != nil {
		return r, fmt.Errorf("webhooks.Prune dead_letter: %w", err)
	}
	r.DeadLetterDeleted, _ = res.RowsAffected()
	return r, nil
}
