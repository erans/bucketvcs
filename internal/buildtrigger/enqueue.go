package buildtrigger

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// PushInfo is the per-push context handed to Enqueue from receive-pack.
type PushInfo struct {
	Tenant     string
	Repo       string
	Actor      string
	TxID       string
	HeadOID    string
	RefUpdates []RefUpdate
}

// DeliveryRow is the test-visible shape of a pending/in_flight delivery row.
// Production worker code uses its own claim/update path; this struct exists
// primarily for assertions in package tests.
type DeliveryRow struct {
	ID        string
	TriggerID string
	Status    string
}

// Enqueue inserts one build_trigger_deliveries row per (active trigger,
// matching ref). One delivery per matching ref keeps the build context
// precise — the trigger knows exactly which ref fired it.
//
// A nil *Service is a no-op (matches the optional-deps pattern elsewhere).
// Insert errors are returned; callers are expected to fail-open (audit the
// failure, continue the originating operation).
func (s *Service) Enqueue(ctx context.Context, push PushInfo) error {
	if s == nil {
		return nil
	}
	triggers, err := s.listActiveForRepo(ctx, push.Tenant, push.Repo)
	if err != nil {
		return fmt.Errorf("buildtrigger: enqueue lookup: %w", err)
	}
	if len(triggers) == 0 {
		return nil
	}
	now := time.Now()
	for _, tr := range triggers {
		for _, ru := range push.RefUpdates {
			ok, merr := RefMatches(tr.RefInclude, tr.RefExclude, ru.Refname)
			if merr != nil || !ok {
				continue
			}
			id, err := newDeliveryID()
			if err != nil {
				return err
			}
			payload := BuildPayload{
				Tenant:    push.Tenant,
				Repo:      push.Repo,
				Actor:     push.Actor,
				TxID:      push.TxID,
				HeadOID:   push.HeadOID,
				RefUpdate: ru,
			}
			body, err := json.Marshal(payload)
			if err != nil {
				return fmt.Errorf("buildtrigger: marshal payload: %w", err)
			}
			if _, err := s.db.ExecContext(ctx,
				`INSERT INTO build_trigger_deliveries
				   (id, trigger_id, payload_json, status, attempts, next_attempt_at, created_at)
				 VALUES (?, ?, ?, 'pending', 0, ?, ?)`,
				id, tr.ID, body, now.Unix(), now.Unix(),
			); err != nil {
				return fmt.Errorf("buildtrigger: insert delivery: %w", err)
			}
		}
	}
	return nil
}

// listActiveForRepo returns all active triggers for (tenant, repo). The rows
// are decoded via the shared scanTrigger helper so Config/RefInclude/RefExclude
// are fully populated for matching.
func (s *Service) listActiveForRepo(ctx context.Context, tenant, repo string) ([]Trigger, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, tenant, repo, name, kind, config_json, ref_include, ref_exclude,
		        token_mode, token_scopes, token_ttl_seconds, active, created_at
		 FROM build_triggers
		 WHERE tenant=? AND repo=? AND active=1`,
		tenant, repo)
	if err != nil {
		return nil, fmt.Errorf("buildtrigger: list active: %w", err)
	}
	defer rows.Close()
	var out []Trigger
	for rows.Next() {
		tr, err := scanTrigger(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, tr)
	}
	return out, rows.Err()
}

// PendingForTest lists all pending + in_flight delivery rows ordered by
// creation time. Test-only accessor; not used by production code paths.
func (s *Service) PendingForTest(ctx context.Context) ([]DeliveryRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, trigger_id, status
		 FROM build_trigger_deliveries
		 WHERE status IN ('pending','in_flight')
		 ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeliveryRow
	for rows.Next() {
		var r DeliveryRow
		if err := rows.Scan(&r.ID, &r.TriggerID, &r.Status); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// newDeliveryID returns a "bvbd_"-prefixed delivery id from 12 random bytes.
func newDeliveryID() (string, error) {
	return generateIDWithPrefix("bvbd_")
}
