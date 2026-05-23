package webhooks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// DeliveryRow is the test-visible shape of a row in webhook_deliveries.
// Production code paths use the worker's internal claim/update; this struct
// exists primarily for assertions in tests.
type DeliveryRow struct {
	ID            string
	EndpointID    int64
	EventType     string
	PayloadJSON   []byte
	Status        string
	Attempts      int
	NextAttemptAt time.Time
}

// Enqueue inserts one webhook_deliveries row per active endpoint whose
// event_mask matches the given event, for (tenant, repo).
//
// The CommonEnvelope is filled in automatically (delivery_id, timestamp,
// event, tenant, repo, actor); callers supply only the per-event body.
//
// A nil *Service is a no-op (matches the optional-deps pattern elsewhere).
// Insert errors are returned but the caller is expected to fail-open
// (audit the failure, continue the originating operation).
func (s *Service) Enqueue(ctx context.Context, event Event, tenant, repo, actor string, payload any) error {
	if s == nil {
		return nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM webhook_endpoints
		 WHERE tenant=? AND repo=? AND active=1 AND (event_mask & ?) != 0`,
		tenant, repo, int64(event))
	if err != nil {
		return fmt.Errorf("webhooks: enqueue lookup: %w", err)
	}
	var endpointIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("webhooks: enqueue scan: %w", err)
		}
		endpointIDs = append(endpointIDs, id)
	}
	rows.Close()
	if len(endpointIDs) == 0 {
		return nil
	}

	now := time.Now()
	for _, epID := range endpointIDs {
		deliveryID, err := newDeliveryID()
		if err != nil {
			return fmt.Errorf("webhooks: new delivery id: %w", err)
		}
		body, err := wrapEnvelope(deliveryID, now.Unix(), event, tenant, repo, actor, payload)
		if err != nil {
			return fmt.Errorf("webhooks: marshal payload: %w", err)
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO webhook_deliveries
			   (id, endpoint_id, event_type, payload_json, status, attempts,
			    next_attempt_at, created_at)
			 VALUES (?, ?, ?, ?, 'pending', 0, ?, ?)`,
			deliveryID, epID, event.String(), body, now.Unix(), now.Unix(),
		); err != nil {
			return fmt.Errorf("webhooks: insert delivery: %w", err)
		}
	}
	return nil
}

// PendingForTest lists all pending+in_flight rows. Test-only accessor; not
// used by production code paths. Exposed because table-driven tests prefer
// reading via the package's own types over hand-rolled SQL.
func (s *Service) PendingForTest(ctx context.Context) ([]DeliveryRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, endpoint_id, event_type, payload_json, status, attempts, next_attempt_at
		 FROM webhook_deliveries
		 WHERE status IN ('pending','in_flight')
		 ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeliveryRow
	for rows.Next() {
		var r DeliveryRow
		var ts int64
		if err := rows.Scan(&r.ID, &r.EndpointID, &r.EventType, &r.PayloadJSON, &r.Status, &r.Attempts, &ts); err != nil {
			return nil, err
		}
		r.NextAttemptAt = time.Unix(ts, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

func newDeliveryID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	// RFC 4122 v4 layout.
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	h := hex.EncodeToString(buf[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}

// wrapEnvelope marshals the given payload, decorates it with the envelope
// fields, and returns the final JSON bytes that go to the receiver. The
// payload's fields merge into the envelope at the top level — the spec's
// "merged into envelope" semantics. Envelope keys take precedence on collision.
func wrapEnvelope(deliveryID string, t int64, event Event, tenant, repo, actor string, payload any) ([]byte, error) {
	out := map[string]any{
		"delivery_id": deliveryID,
		"timestamp":   t,
		"event":       event.String(),
		"tenant":      tenant,
		"repo":        repo,
		"actor":       actor,
	}
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, err
		}
		for k, v := range fields {
			if _, exists := out[k]; exists {
				continue
			}
			out[k] = v
		}
	}
	return json.Marshal(out)
}
