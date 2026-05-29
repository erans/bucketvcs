package quota

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// State is the in-memory shape of one quota row.
type State struct {
	Tenant     string
	LimitBytes int64
	UsedBytes  int64
	UpdatedAt  time.Time
	Exists     bool // false when no row exists (unlimited)
}

// QuotaError is returned by CheckBatch when a batch would push a
// tenant over its limit. Callers use errors.As to recover the
// structured fields for the LFS 507 ObjectError message.
type QuotaError struct {
	Tenant         string
	CurrentBytes   int64
	LimitBytes     int64
	RequestedBytes int64
}

func (e *QuotaError) Error() string {
	return fmt.Sprintf("tenant quota exceeded: %d used / %d limit, %d requested (tenant=%s)",
		e.CurrentBytes, e.LimitBytes, e.RequestedBytes, e.Tenant)
}

// Report is the result of one Reconcile call. (Reconcile itself is
// added in Task 2.)
type Report struct {
	Tenant      string
	BeforeBytes int64
	AfterBytes  int64
	DriftBytes  int64 // signed: positive when actual > counter
	DryRun      bool
}

// Service wraps the quotas table on the authdb. All methods are safe
// for concurrent use; sqlite's single-writer model serializes writes.
type Service struct {
	db     sqlitestore.Querier
	logger *slog.Logger
}

// New constructs a Service. logger may be nil; emissions added in
// later tasks will fall back to slog.Default() at emission time.
func New(db sqlitestore.Querier, logger *slog.Logger) *Service {
	return &Service{
		db:     db,
		logger: logger,
	}
}

// Set creates or updates a quota row.
func (s *Service) Set(ctx context.Context, tenant string, limitBytes int64) error {
	if limitBytes < 0 {
		return fmt.Errorf("quota: limit must be >= 0 (got %d)", limitBytes)
	}
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO quotas (tenant, limit_bytes, used_bytes, updated_at)
		VALUES (?, ?, 0, ?)
		ON CONFLICT(tenant) DO UPDATE SET
			limit_bytes = excluded.limit_bytes,
			updated_at  = excluded.updated_at
	`, tenant, limitBytes, now)
	if err != nil {
		return fmt.Errorf("quota set %q: %w", tenant, err)
	}
	return nil
}

// Get returns the current state. Exists=false when no row exists.
func (s *Service) Get(ctx context.Context, tenant string) (State, error) {
	var (
		limit   int64
		used    int64
		updated int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT limit_bytes, used_bytes, updated_at FROM quotas WHERE tenant = ?`,
		tenant,
	).Scan(&limit, &used, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return State{Tenant: tenant, Exists: false}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("quota get %q: %w", tenant, err)
	}
	return State{
		Tenant:     tenant,
		LimitBytes: limit,
		UsedBytes:  used,
		UpdatedAt:  time.Unix(updated, 0).UTC(),
		Exists:     true,
	}, nil
}

// Clear removes the quota row (back to unlimited).
func (s *Service) Clear(ctx context.Context, tenant string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM quotas WHERE tenant = ?`, tenant)
	if err != nil {
		return fmt.Errorf("quota clear %q: %w", tenant, err)
	}
	return nil
}

// List returns every quota row.
func (s *Service) List(ctx context.Context) ([]State, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tenant, limit_bytes, used_bytes, updated_at FROM quotas ORDER BY tenant`)
	if err != nil {
		return nil, fmt.Errorf("quota list: %w", err)
	}
	defer rows.Close()
	var out []State
	for rows.Next() {
		var (
			tenant  string
			limit   int64
			used    int64
			updated int64
		)
		if err := rows.Scan(&tenant, &limit, &used, &updated); err != nil {
			return nil, fmt.Errorf("quota list scan: %w", err)
		}
		out = append(out, State{
			Tenant:     tenant,
			LimitBytes: limit,
			UsedBytes:  used,
			UpdatedAt:  time.Unix(updated, 0).UTC(),
			Exists:     true,
		})
	}
	return out, rows.Err()
}

// CheckBatch is the Batch-time pre-check. requestedBytes is the SUM
// of every object size in the batch. Returns nil when the tenant has
// no quota row (unlimited) OR when (current + requested) <= limit.
// Returns *QuotaError otherwise.
func (s *Service) CheckBatch(ctx context.Context, tenant string, requestedBytes int64) error {
	if requestedBytes < 0 {
		return fmt.Errorf("quota: requestedBytes must be >= 0 (got %d)", requestedBytes)
	}
	state, err := s.Get(ctx, tenant)
	if err != nil {
		return err
	}
	if !state.Exists {
		return nil
	}
	// Non-overflowing form of `state.UsedBytes + requestedBytes > state.LimitBytes`.
	// LimitBytes >= 0 (validated in Set); requestedBytes >= 0 (validated above);
	// so LimitBytes-requestedBytes cannot overflow positively. If requestedBytes
	// > LimitBytes the subtraction is negative and any non-negative UsedBytes
	// exceeds it — the check correctly rejects.
	if state.UsedBytes > state.LimitBytes-requestedBytes {
		return &QuotaError{
			Tenant:         tenant,
			CurrentBytes:   state.UsedBytes,
			LimitBytes:     state.LimitBytes,
			RequestedBytes: requestedBytes,
		}
	}
	return nil
}

// Add increments used_bytes by the given amount, atomically. No-op
// when no quota row exists for the tenant.
//
// Idempotency is enforced cross-node via the quota_credits table: a
// row is inserted with ON CONFLICT (tenant, oid) DO NOTHING inside the
// same transaction as the used_bytes increment, so the same (tenant,
// oid) — whether replayed on this node or another — increments exactly
// once.
func (s *Service) Add(ctx context.Context, tenant, oid string, bytes int64) error {
	if bytes < 0 {
		return fmt.Errorf("quota: bytes must be >= 0 (got %d)", bytes)
	}
	if bytes == 0 {
		return nil
	}
	now := time.Now().Unix()
	return s.db.RunInTx(ctx, func(tx sqlitestore.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO quota_credits (tenant, oid, bytes, recorded_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (tenant, oid) DO NOTHING`,
			tenant, oid, bytes, now)
		if err != nil {
			return fmt.Errorf("quota add %q oid=%s: credit: %w", tenant, oid, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("quota add %q oid=%s: rows affected: %w", tenant, oid, err)
		}
		if n == 0 {
			return nil // already credited (this node or another) — idempotent no-op
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE quotas SET used_bytes = used_bytes + ?, updated_at = ?
			WHERE tenant = ?`,
			bytes, now, tenant); err != nil {
			return fmt.Errorf("quota add %q oid=%s: increment: %w", tenant, oid, err)
		}
		return nil
	})
}

// Subtract decrements used_bytes, floored at zero via the backend's
// Greatest(used - ?, 0) clamp (the clamp absorbs reconcile-vs-sweep
// drift, §6.4). The decrement is gated on deleting the (tenant, oid)
// credit row in the same transaction, so an upload → GC → re-upload
// cycle on the same OID re-credits correctly on the next Add, and a
// second Subtract for an already-removed credit is a no-op. No-op when
// no quota row exists for the tenant.
func (s *Service) Subtract(ctx context.Context, tenant, oid string, bytes int64) error {
	if bytes < 0 {
		return fmt.Errorf("quota: bytes must be >= 0 (got %d)", bytes)
	}
	if bytes == 0 {
		return nil
	}
	now := time.Now().Unix()
	clamp := s.db.Greatest("used_bytes - ?", "0")
	return s.db.RunInTx(ctx, func(tx sqlitestore.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM quota_credits WHERE tenant = ? AND oid = ?`, tenant, oid)
		if err != nil {
			return fmt.Errorf("quota subtract %q oid=%s: uncredit: %w", tenant, oid, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("quota subtract %q oid=%s: rows affected: %w", tenant, oid, err)
		}
		if n == 0 {
			return nil // not credited — nothing to subtract (idempotent; reconcile is the backstop)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE quotas SET used_bytes = `+clamp+`, updated_at = ? WHERE tenant = ?`,
			bytes, now, tenant); err != nil {
			return fmt.Errorf("quota subtract %q oid=%s: decrement: %w", tenant, oid, err)
		}
		return nil
	})
}

// Reconcile lists the LFS storage prefix for the tenant, sums sizes
// across every repo, and overwrites used_bytes. Returns a Report
// with before/after/drift. dryRun=true reports the drift without
// writing. No-op when no quota row exists for the tenant; in that
// case BeforeBytes and AfterBytes are both zero.
func (s *Service) Reconcile(ctx context.Context, store storage.ObjectStore, tenant string, dryRun bool) (Report, error) {
	state, err := s.Get(ctx, tenant)
	if err != nil {
		return Report{}, err
	}
	rep := Report{Tenant: tenant, BeforeBytes: state.UsedBytes, DryRun: dryRun}
	if !state.Exists {
		return rep, nil
	}

	// Walk tenants/<tenant>/repos/* with a delimiter to discover
	// the repo names, then sum each repo's LFS prefix.
	tenantRepoPrefix := "tenants/" + tenant + "/repos/"
	var sum int64
	var token string
	for {
		page, err := store.List(ctx, tenantRepoPrefix, &storage.ListOptions{
			ContinuationToken: token,
			Delimiter:         "/",
		})
		if err != nil {
			return rep, fmt.Errorf("quota reconcile %q list tenant repos: %w", tenant, err)
		}
		for _, cp := range page.CommonPrefixes {
			repo := strings.TrimSuffix(strings.TrimPrefix(cp, tenantRepoPrefix), "/")
			if repo == "" {
				continue
			}
			repoSum, err := sumLFSPrefix(ctx, store, tenant, repo)
			if err != nil {
				return rep, err
			}
			sum += repoSum
		}
		if page.NextToken == "" {
			break
		}
		token = page.NextToken
	}

	rep.AfterBytes = sum
	rep.DriftBytes = sum - state.UsedBytes

	if !dryRun {
		now := time.Now().Unix()
		if _, err := s.db.ExecContext(ctx, `
			UPDATE quotas SET used_bytes = ?, updated_at = ? WHERE tenant = ?
		`, sum, now, tenant); err != nil {
			return rep, fmt.Errorf("quota reconcile write %q: %w", tenant, err)
		}
	}
	return rep, nil
}

// sumLFSPrefix lists tenants/<tenant>/repos/<repo>/lfs/objects/ and
// returns the total byte sum. Paginates the listing.
func sumLFSPrefix(ctx context.Context, store storage.ObjectStore, tenant, repo string) (int64, error) {
	prefix := lfsRepoPrefix(tenant, repo)
	var sum int64
	var token string
	for {
		page, err := store.List(ctx, prefix, &storage.ListOptions{ContinuationToken: token})
		if err != nil {
			return 0, fmt.Errorf("quota reconcile list lfs %s/%s: %w", tenant, repo, err)
		}
		for _, obj := range page.Objects {
			sum += obj.Size
		}
		if page.NextToken == "" {
			break
		}
		token = page.NextToken
	}
	return sum, nil
}

// lfsRepoPrefix is local to avoid importing internal/lfs (which would
// pull in the gateway-side handler types and create a dependency cycle
// once internal/lfs starts importing quota in Task 3). The path string
// here is the storage contract from internal/lfs/keys.go::RepoLFSPrefix.
func lfsRepoPrefix(tenant, repo string) string {
	return "tenants/" + tenant + "/repos/" + repo + "/lfs/objects/"
}
