package quota

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
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
	ring   *addRing
}

// New constructs a Service. logger may be nil; emissions added in
// later tasks will fall back to slog.Default() at emission time.
func New(db sqlitestore.Querier, logger *slog.Logger) *Service {
	return &Service{
		db:     db,
		logger: logger,
		ring:   newAddRing(1024),
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
// when no quota row exists for the tenant. Idempotent within the
// dedupe ring's TTL window (see addRing).
//
// Concurrency: holds the ring lock across the DB UPDATE. SQLite's
// single-writer model already serializes concurrent UPDATEs, so the
// additional in-process serialization is essentially free. The
// lock-across-DB model gives us TWO properties:
//
//  1. If UPDATE succeeds, only one caller for a given (tenant, oid)
//     ever does so (the next caller observes Seen=true and short-
//     circuits). True idempotency under concurrent same-OID retries
//     — the verify-replay scenario the ring exists for.
//  2. If UPDATE fails, we never reach Record, so the ring is not
//     polluted with an OID that didn't actually increment. A retry
//     from the caller will succeed.
func (s *Service) Add(ctx context.Context, tenant, oid string, bytes int64) error {
	if bytes < 0 {
		return fmt.Errorf("quota: bytes must be >= 0 (got %d)", bytes)
	}
	if bytes == 0 {
		return nil
	}
	s.ring.Lock()
	defer s.ring.Unlock()

	if s.ring.Seen(tenant, oid) {
		return nil
	}

	now := time.Now().Unix()
	if _, err := s.db.ExecContext(ctx, `
		UPDATE quotas
		SET used_bytes = used_bytes + ?, updated_at = ?
		WHERE tenant = ?
	`, bytes, now, tenant); err != nil {
		return fmt.Errorf("quota add %q oid=%s: %w", tenant, oid, err)
	}
	s.ring.Record(tenant, oid)
	return nil
}

// Subtract decrements used_bytes, floored at zero via MAX(used - ?, 0).
// The clamp absorbs reconcile-vs-sweep drift (§6.4). Also forgets the
// OID from the dedupe ring so an upload → GC → re-upload cycle on
// the same OID within the ring's capacity isn't silently deduped on
// the second Add. No-op when no quota row exists for the tenant.
func (s *Service) Subtract(ctx context.Context, tenant, oid string, bytes int64) error {
	if bytes < 0 {
		return fmt.Errorf("quota: bytes must be >= 0 (got %d)", bytes)
	}
	if bytes == 0 {
		return nil
	}
	now := time.Now().Unix()
	q := `UPDATE quotas SET used_bytes = ` + s.db.Greatest("used_bytes - ?", "0") +
		`, updated_at = ? WHERE tenant = ?`
	_, err := s.db.ExecContext(ctx, q, bytes, now, tenant)
	if err != nil {
		return fmt.Errorf("quota subtract %q oid=%s: %w", tenant, oid, err)
	}
	s.ring.Lock()
	s.ring.Forget(tenant, oid)
	s.ring.Unlock()
	return nil
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

// addRing is a fixed-size FIFO dedupe over (tenant, oid) pairs to keep
// Add idempotent against verify-replay within the kind=5 token's TTL.
// Capacity is fixed at construction; eviction is FIFO (the oldest
// recorded entry is dropped when a new entry arrives at a full ring —
// this is NOT true LRU, but the difference is academic for the
// verify-replay use case where any duplicate within the TTL is likely
// still inside the recent slots regardless of policy). Safe for
// concurrent use; the mutex is internal to the ring.
type addRing struct {
	cap   int
	mu    sync.Mutex
	idx   map[string]int // (tenant + "\x00" + oid) -> slot
	order []string
	head  int // next slot to (re)use
}

func newAddRing(capacity int) *addRing {
	return &addRing{
		cap:   capacity,
		idx:   make(map[string]int, capacity),
		order: make([]string, capacity),
	}
}

// Lock and Unlock expose the internal mutex so callers can perform a
// check-then-act under the same critical section if needed.
func (r *addRing) Lock()   { r.mu.Lock() }
func (r *addRing) Unlock() { r.mu.Unlock() }

// Seen reports whether (tenant, oid) is currently in the ring.
// Caller must hold the ring's lock.
func (r *addRing) Seen(tenant, oid string) bool {
	_, ok := r.idx[ringKey(tenant, oid)]
	return ok
}

// Record inserts (tenant, oid) into the ring, evicting the oldest
// entry if at capacity. Caller must hold the ring's lock.
func (r *addRing) Record(tenant, oid string) {
	k := ringKey(tenant, oid)
	if _, exists := r.idx[k]; exists {
		return
	}
	if old := r.order[r.head]; old != "" {
		delete(r.idx, old)
	}
	r.order[r.head] = k
	r.idx[k] = r.head
	r.head = (r.head + 1) % r.cap
}

// Forget removes (tenant, oid) from the ring if present. Used by
// Subtract so an OID can be re-Added after a GC sweep (the upload →
// GC → re-upload cycle within 1024 unique OIDs would otherwise be
// silently deduped). The vacated slot is reused on the next Record
// rotation. Caller must hold the ring's lock.
func (r *addRing) Forget(tenant, oid string) {
	k := ringKey(tenant, oid)
	slot, ok := r.idx[k]
	if !ok {
		return
	}
	delete(r.idx, k)
	r.order[slot] = ""
}

func ringKey(tenant, oid string) string {
	return tenant + "\x00" + oid
}
