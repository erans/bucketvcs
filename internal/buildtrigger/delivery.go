package buildtrigger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Delivery is the operator-facing view of one row in build_trigger_deliveries.
// PayloadJSON is omitted on list/get rows here (operators read the body via the
// queue tables directly if needed); the CLI surfaces the metadata fields.
type Delivery struct {
	ID             string
	TriggerID      string
	Status         string
	Attempts       int
	NextAttemptAt  time.Time
	LastAttemptAt  *time.Time
	LastStatusCode int
	LastError      string
	CreatedAt      time.Time
	DeliveredAt    *time.Time
}

// ListDeliveries returns deliveries ordered by created_at DESC. triggerID and
// status narrow the result set when non-empty; limit caps the row count when
// >0 (0 means no limit).
func (s *Service) ListDeliveries(ctx context.Context, triggerID, status string, limit int) ([]Delivery, error) {
	q := `SELECT id, trigger_id, status, attempts, next_attempt_at,
	             last_attempt_at, last_status_code, last_error, created_at, delivered_at
	      FROM build_trigger_deliveries WHERE 1=1`
	var args []any
	if triggerID != "" {
		q += " AND trigger_id=?"
		args = append(args, triggerID)
	}
	if status != "" {
		q += " AND status=?"
		args = append(args, status)
	}
	q += " ORDER BY created_at DESC"
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("buildtrigger: list deliveries: %w", err)
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetDelivery returns one delivery by id. Returns ErrNotFound if absent.
func (s *Service) GetDelivery(ctx context.Context, id string) (Delivery, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, trigger_id, status, attempts, next_attempt_at,
		        last_attempt_at, last_status_code, last_error, created_at, delivered_at
		 FROM build_trigger_deliveries WHERE id=?`, id)
	d, err := scanDelivery(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Delivery{}, ErrNotFound
		}
		return Delivery{}, fmt.Errorf("buildtrigger: get delivery %s: %w", id, err)
	}
	return d, nil
}

// ReplayDelivery transitions a terminal row (dead_letter or delivered) back to
// pending with next_attempt_at=now and last_error cleared. Attempts are NOT
// reset (matches webhooks.ReplayDelivery's idempotent-recovery semantics minus
// the attempt counter, which the build worker uses as its retry budget). A row
// currently in_flight returns ErrReplayInFlight; a missing row returns
// ErrNotFound.
func (s *Service) ReplayDelivery(ctx context.Context, id string) error {
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`UPDATE build_trigger_deliveries
		   SET status='pending', next_attempt_at=?, last_error=NULL
		 WHERE id=? AND status IN ('pending','delivered','dead_letter')`,
		now, id)
	if err != nil {
		return fmt.Errorf("buildtrigger: replay %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("buildtrigger: replay %s rows affected: %w", id, err)
	}
	if n == 0 {
		// Either the id doesn't exist OR the row is currently in_flight.
		// Distinguish the two by reading the row.
		var status string
		err := s.db.QueryRowContext(ctx,
			`SELECT status FROM build_trigger_deliveries WHERE id=?`, id).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("buildtrigger: replay %s post-check: %w", id, err)
		}
		return fmt.Errorf("%w (id=%s)", ErrReplayInFlight, id)
	}
	return nil
}

// scanDelivery decodes one row into a Delivery. Satisfied by both *sql.Row and
// *sql.Rows via the shared rowScanner interface.
func scanDelivery(sc rowScanner) (Delivery, error) {
	var d Delivery
	var lastAttemptAt, deliveredAt sql.NullInt64
	var lastStatusCode sql.NullInt64
	var lastError sql.NullString
	var nextAttemptAt, createdAt int64
	if err := sc.Scan(&d.ID, &d.TriggerID, &d.Status, &d.Attempts,
		&nextAttemptAt, &lastAttemptAt, &lastStatusCode, &lastError, &createdAt, &deliveredAt); err != nil {
		return Delivery{}, err
	}
	d.NextAttemptAt = time.Unix(nextAttemptAt, 0)
	d.CreatedAt = time.Unix(createdAt, 0)
	if lastAttemptAt.Valid {
		t := time.Unix(lastAttemptAt.Int64, 0)
		d.LastAttemptAt = &t
	}
	if deliveredAt.Valid {
		t := time.Unix(deliveredAt.Int64, 0)
		d.DeliveredAt = &t
	}
	if lastStatusCode.Valid {
		d.LastStatusCode = int(lastStatusCode.Int64)
	}
	if lastError.Valid {
		d.LastError = lastError.String
	}
	return d, nil
}
