package webhooks

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// ErrNotFound is returned by Get / Remove / Enable / Disable when the
// endpoint id does not exist. Matches the sentinel pattern used by
// internal/lfs/locks and internal/policy.
var ErrNotFound = errors.New("webhooks: not found")

// ErrConflict is returned by Create when a (tenant, repo, url) endpoint
// already exists. The UNIQUE constraint on (tenant, repo, url) enforces
// this at the DB layer.
var ErrConflict = errors.New("webhooks: endpoint already exists")

// ErrInvalidInput is returned by Create when an EndpointInput field is
// missing or malformed (empty tenant/repo/URL, bad URL scheme, zero
// event mask). CLI callers distinguish this from operational errors via
// errors.Is to set exit code 2.
var ErrInvalidInput = errors.New("webhooks: invalid input")

// Service exposes webhook endpoint management against the M4 authdb.
type Service struct {
	db *sql.DB
}

// New constructs a Service backed by the given authdb handle.
func New(db *sql.DB) *Service {
	return &Service{db: db}
}

// Endpoint is the canonical view returned to operators. Secret is populated
// ONLY by Create (returned once); List/Get return empty Secret and a 6-char
// SecretPreview.
type Endpoint struct {
	ID            int64
	Tenant        string
	Repo          string
	URL           string
	Secret        string
	SecretPreview string
	EventMask     Event
	Active        bool
	CreatedAt     time.Time
}

// EndpointInput is the operator-supplied data for Create. Secret is generated
// server-side; Active defaults to true.
type EndpointInput struct {
	Tenant    string
	Repo      string
	URL       string
	EventMask Event
}

// Create inserts a new endpoint with a server-generated secret. Returns the
// endpoint with Secret populated. Subsequent reads of this row return empty
// Secret + non-empty SecretPreview.
func (s *Service) Create(ctx context.Context, in EndpointInput) (Endpoint, error) {
	if in.Tenant == "" {
		return Endpoint{}, fmt.Errorf("%w: tenant must not be empty", ErrInvalidInput)
	}
	if in.Repo == "" {
		return Endpoint{}, fmt.Errorf("%w: repo must not be empty", ErrInvalidInput)
	}
	if in.URL == "" {
		return Endpoint{}, fmt.Errorf("%w: url must not be empty", ErrInvalidInput)
	}
	if err := validateURL(in.URL); err != nil {
		return Endpoint{}, fmt.Errorf("%w: invalid url: %s", ErrInvalidInput, err.Error())
	}
	if in.EventMask == 0 {
		return Endpoint{}, fmt.Errorf("%w: event mask must not be zero", ErrInvalidInput)
	}
	secret, err := generateSecret()
	if err != nil {
		return Endpoint{}, fmt.Errorf("webhooks: generate secret: %w", err)
	}
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO webhook_endpoints
		   (tenant, repo, url, secret, event_mask, active, created_at)
		 VALUES (?, ?, ?, ?, ?, 1, ?)`,
		in.Tenant, in.Repo, in.URL, secret, int64(in.EventMask), now,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return Endpoint{}, ErrConflict
		}
		return Endpoint{}, fmt.Errorf("webhooks: insert endpoint: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Endpoint{}, fmt.Errorf("webhooks: last insert id: %w", err)
	}
	return Endpoint{
		ID:        id,
		Tenant:    in.Tenant,
		Repo:      in.Repo,
		URL:       in.URL,
		Secret:    secret,
		EventMask: in.EventMask,
		Active:    true,
		CreatedAt: time.Unix(now, 0),
	}, nil
}

// List returns all endpoints for (tenant, repo) ordered by id ascending.
// Secret is empty; SecretPreview is populated.
func (s *Service) List(ctx context.Context, tenant, repo string) ([]Endpoint, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, tenant, repo, url, secret, event_mask, active, created_at
		 FROM webhook_endpoints
		 WHERE tenant=? AND repo=?
		 ORDER BY id ASC`,
		tenant, repo)
	if err != nil {
		return nil, fmt.Errorf("webhooks: list: %w", err)
	}
	defer rows.Close()
	var out []Endpoint
	for rows.Next() {
		var ep Endpoint
		var rawSecret string
		var mask int64
		var active int
		var createdAt int64
		if err := rows.Scan(&ep.ID, &ep.Tenant, &ep.Repo, &ep.URL, &rawSecret, &mask, &active, &createdAt); err != nil {
			return nil, fmt.Errorf("webhooks: scan: %w", err)
		}
		ep.EventMask = Event(mask)
		ep.Active = active == 1
		ep.CreatedAt = time.Unix(createdAt, 0)
		ep.SecretPreview = secretPreview(rawSecret)
		out = append(out, ep)
	}
	return out, rows.Err()
}

// Get returns one endpoint by id (no secret).
func (s *Service) Get(ctx context.Context, id int64) (Endpoint, error) {
	return s.getOne(ctx, id, false)
}

// GetWithSecret returns one endpoint by id WITH the raw secret. Used by the
// worker to sign payloads. Callers MUST NOT expose the result to operators.
func (s *Service) GetWithSecret(ctx context.Context, id int64) (Endpoint, error) {
	return s.getOne(ctx, id, true)
}

func (s *Service) getOne(ctx context.Context, id int64, withSecret bool) (Endpoint, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, tenant, repo, url, secret, event_mask, active, created_at
		 FROM webhook_endpoints WHERE id=?`, id)
	var ep Endpoint
	var rawSecret string
	var mask int64
	var active int
	var createdAt int64
	if err := row.Scan(&ep.ID, &ep.Tenant, &ep.Repo, &ep.URL, &rawSecret, &mask, &active, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Endpoint{}, ErrNotFound
		}
		return Endpoint{}, fmt.Errorf("webhooks: get %d: %w", id, err)
	}
	ep.EventMask = Event(mask)
	ep.Active = active == 1
	ep.CreatedAt = time.Unix(createdAt, 0)
	if withSecret {
		ep.Secret = rawSecret
	} else {
		ep.SecretPreview = secretPreview(rawSecret)
	}
	return ep, nil
}

// Remove deletes an endpoint by id. Cascades to webhook_deliveries.
func (s *Service) Remove(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM webhook_endpoints WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("webhooks: remove %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("webhooks: remove %d rows affected: %w", id, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Enable flips active=1.
func (s *Service) Enable(ctx context.Context, id int64) error {
	return s.setActive(ctx, id, true)
}

// Disable flips active=0. Existing pending deliveries for this endpoint
// stop draining immediately — they remain in the queue with status='pending'
// and resume only if the endpoint is re-enabled. Use Remove to also delete
// the rows.
func (s *Service) Disable(ctx context.Context, id int64) error {
	return s.setActive(ctx, id, false)
}

func (s *Service) setActive(ctx context.Context, id int64, active bool) error {
	v := 0
	if active {
		v = 1
	}
	res, err := s.db.ExecContext(ctx, `UPDATE webhook_endpoints SET active=? WHERE id=?`, v, id)
	if err != nil {
		return fmt.Errorf("webhooks: set active %d=%v: %w", id, active, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("webhooks: set active %d=%v rows affected: %w", id, active, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func validateURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https (got %q)", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("host must not be empty")
	}
	return nil
}

// generateSecret returns 32 random bytes encoded as base64-url-no-padding (43 chars).
func generateSecret() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf[:]), nil
}

func secretPreview(secret string) string {
	if len(secret) < 6 {
		return secret
	}
	return secret[:6] + "..."
}

// RotateSecret generates a new 32-byte random secret for the endpoint and
// returns it. Existing in_flight deliveries continue with the secret they
// captured at claim time; pending deliveries pick up the new secret on
// next claim. Returns ErrNotFound if no endpoint matches.
//
// Operators who need strict cutover (no overlap window where both old and
// new secrets validate) should Disable the endpoint first, wait for queue
// drain, then call RotateSecret and Enable.
func (s *Service) RotateSecret(ctx context.Context, id int64) (string, error) {
	secret, err := generateSecret()
	if err != nil {
		return "", fmt.Errorf("webhooks: generate secret: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE webhook_endpoints SET secret=? WHERE id=?`, secret, id)
	if err != nil {
		return "", fmt.Errorf("webhooks: rotate secret %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("webhooks: rotate secret %d rows affected: %w", id, err)
	}
	if n == 0 {
		return "", ErrNotFound
	}
	return secret, nil
}

// Delivery is the operator-facing view of one row in webhook_deliveries.
type Delivery struct {
	ID             string
	EndpointID     int64
	EventType      string
	PayloadJSON    []byte
	Status         string
	Attempts       int
	NextAttemptAt  time.Time
	LastAttemptAt  *time.Time
	LastStatusCode int
	LastError      string
	CreatedAt      time.Time
	DeliveredAt    *time.Time
}

// ListDeliveriesFilter narrows the result set. Zero values mean "no filter".
type ListDeliveriesFilter struct {
	EndpointID int64
	Status     string
	SinceUnix  int64
	Limit      int // 0 → default 500; capped at 10000
}

// ListDeliveries returns deliveries matching filter, ordered by created_at desc.
// PayloadJSON is omitted on list rows (use ShowDelivery for the full body).
func (s *Service) ListDeliveries(ctx context.Context, f ListDeliveriesFilter) ([]Delivery, error) {
	q := `SELECT id, endpoint_id, event_type, status, attempts, next_attempt_at,
	             last_attempt_at, last_status_code, last_error, created_at, delivered_at
	      FROM webhook_deliveries WHERE 1=1`
	var args []any
	if f.EndpointID != 0 {
		q += " AND endpoint_id=?"
		args = append(args, f.EndpointID)
	}
	if f.Status != "" {
		q += " AND status=?"
		args = append(args, f.Status)
	}
	if f.SinceUnix != 0 {
		q += " AND created_at >= ?"
		args = append(args, f.SinceUnix)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 500
	}
	if limit > 10000 {
		limit = 10000
	}
	q += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("webhooks: list deliveries: %w", err)
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

// ShowDelivery returns one delivery by id, including the payload bytes.
func (s *Service) ShowDelivery(ctx context.Context, id string) (Delivery, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, endpoint_id, event_type, payload_json, status, attempts, next_attempt_at,
		        last_attempt_at, last_status_code, last_error, created_at, delivered_at
		 FROM webhook_deliveries WHERE id=?`, id)
	var d Delivery
	var lastAttemptAt, deliveredAt sql.NullInt64
	var lastStatusCode sql.NullInt64
	var lastError sql.NullString
	var nextAttemptAt, createdAt int64
	if err := row.Scan(&d.ID, &d.EndpointID, &d.EventType, &d.PayloadJSON, &d.Status, &d.Attempts,
		&nextAttemptAt, &lastAttemptAt, &lastStatusCode, &lastError, &createdAt, &deliveredAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Delivery{}, ErrNotFound
		}
		return Delivery{}, fmt.Errorf("webhooks: show delivery %s: %w", id, err)
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

// ReplayDelivery transitions any non-in_flight row (typically dead_letter)
// back to pending with attempts=0 and next_attempt_at=now. Idempotent on
// already-pending rows. Returns ErrNotFound if no row matches; returns an
// error if the row is currently in_flight (a worker is mid-delivery).
//
// NOTE: replaying a row that is mid-retry chain (status=pending,
// attempts>0) resets attempts to 0 and clears the backoff state — the
// retry budget restarts from scratch. Operators should generally only
// replay rows in terminal `dead_letter` status; replaying live retrying
// rows is supported (idempotent) but undoes accumulated backoff.
func (s *Service) ReplayDelivery(ctx context.Context, id string) error {
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`UPDATE webhook_deliveries
		   SET status='pending', attempts=0, next_attempt_at=?,
		       last_error=NULL, last_status_code=NULL,
		       last_attempt_at=NULL, delivered_at=NULL
		 WHERE id=? AND status IN ('pending','delivered','dead_letter')`,
		now, id)
	if err != nil {
		return fmt.Errorf("webhooks: replay %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("webhooks: replay %s rows affected: %w", id, err)
	}
	if n == 0 {
		// Either the id doesn't exist OR the row is currently in_flight.
		// Distinguish the two by reading the row.
		var status string
		err := s.db.QueryRowContext(ctx,
			`SELECT status FROM webhook_deliveries WHERE id=?`, id).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("webhooks: replay %s post-check: %w", id, err)
		}
		return fmt.Errorf("webhooks: cannot replay %s: row is in_flight", id)
	}
	return nil
}

// scanDelivery decodes one *sql.Rows row into a Delivery (without payload_json).
func scanDelivery(rows *sql.Rows) (Delivery, error) {
	var d Delivery
	var lastAttemptAt, deliveredAt sql.NullInt64
	var lastStatusCode sql.NullInt64
	var lastError sql.NullString
	var nextAttemptAt, createdAt int64
	if err := rows.Scan(&d.ID, &d.EndpointID, &d.EventType, &d.Status, &d.Attempts,
		&nextAttemptAt, &lastAttemptAt, &lastStatusCode, &lastError, &createdAt, &deliveredAt); err != nil {
		return Delivery{}, fmt.Errorf("webhooks: scan delivery: %w", err)
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

// isUniqueViolation reports whether err looks like a SQLite UNIQUE
// constraint failure. Mirrors the pattern used by internal/lfs/locks.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "constraint failed: UNIQUE")
}
