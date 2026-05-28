package sqlitestore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// AddOIDCIssuer registers a trusted issuer. Returns auth.ErrConflict if the
// alias or issuer_url already exists.
func (s *Store) AddOIDCIssuer(ctx context.Context, alias, issuerURL string) error {
	if alias == "" || issuerURL == "" {
		return fmt.Errorf("oidc: alias and issuer_url required")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO oidc_issuers (alias, issuer_url, created_at) VALUES (?, ?, `+s.backend.NowSeconds()+`)`,
		alias, issuerURL)
	if err != nil {
		if s.backend.IsUniqueViolation(err) {
			return auth.ErrConflict
		}
		return fmt.Errorf("add oidc issuer: %w", err)
	}
	return nil
}

// ListOIDCIssuers returns all registered issuers ordered by alias.
func (s *Store) ListOIDCIssuers(ctx context.Context) ([]auth.OIDCIssuer, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT alias, issuer_url, created_at FROM oidc_issuers ORDER BY alias`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []auth.OIDCIssuer
	for rows.Next() {
		var i auth.OIDCIssuer
		if err := rows.Scan(&i.Alias, &i.IssuerURL, &i.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// RemoveOIDCIssuer deletes an issuer, cascading to its rules. Returns
// ErrNoSuchOIDCIssuer if the alias does not exist.
func (s *Store) RemoveOIDCIssuer(ctx context.Context, alias string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM oidc_issuers WHERE alias = ?`, alias)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoSuchOIDCIssuer
	}
	return nil
}

// FindOIDCIssuerByURL resolves an issuer by its exact URL.
func (s *Store) FindOIDCIssuerByURL(ctx context.Context, issuerURL string) (auth.OIDCIssuer, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT alias, issuer_url, created_at FROM oidc_issuers WHERE issuer_url = ?`, issuerURL)
	var i auth.OIDCIssuer
	if err := row.Scan(&i.Alias, &i.IssuerURL, &i.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return auth.OIDCIssuer{}, ErrNoSuchOIDCIssuer
		}
		return auth.OIDCIssuer{}, err
	}
	return i, nil
}

// AddOIDCRule inserts a trust rule and its claim constraints in one tx.
// Returns the generated rule id. Validates audience non-empty and ttl > 0.
func (s *Store) AddOIDCRule(ctx context.Context, r auth.OIDCTrustRule) (string, error) {
	if r.Audience == "" {
		return "", fmt.Errorf("oidc: audience required")
	}
	if r.TTLSeconds <= 0 {
		return "", fmt.Errorf("oidc: ttl must be > 0")
	}
	if r.TTLSeconds > OIDCMaxTTLSeconds {
		return "", fmt.Errorf("oidc: ttl %ds exceeds maximum %ds", r.TTLSeconds, OIDCMaxTTLSeconds)
	}
	rnd, err := randomHex(12)
	if err != nil {
		return "", fmt.Errorf("oidc: generate rule id: %w", err)
	}
	id := "bvor_" + rnd
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback() //nolint:errcheck
	_, err = tx.ExecContext(ctx,
		`INSERT INTO oidc_trust_rules
		   (id, issuer_alias, audience, tenant, repo, scopes, ttl_seconds, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, `+s.backend.NowSeconds()+`)`,
		id, r.IssuerAlias, r.Audience, r.Tenant, r.Repo, int64(r.Scopes), r.TTLSeconds)
	if err != nil {
		return "", fmt.Errorf("insert rule: %w", err)
	}
	for name, val := range r.Claims {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO oidc_rule_claims (rule_id, claim_name, claim_value) VALUES (?, ?, ?)`,
			id, name, val); err != nil {
			return "", fmt.Errorf("insert claim: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

// ListOIDCRulesForIssuer returns rules (with claims loaded) for one issuer.
func (s *Store) ListOIDCRulesForIssuer(ctx context.Context, alias string) ([]auth.OIDCTrustRule, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, issuer_alias, audience, tenant, repo, scopes, ttl_seconds, created_at
		   FROM oidc_trust_rules WHERE issuer_alias = ?`, alias)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []auth.OIDCTrustRule
	for rows.Next() {
		var r auth.OIDCTrustRule
		var scopes int64
		if err := rows.Scan(&r.ID, &r.IssuerAlias, &r.Audience, &r.Tenant, &r.Repo,
			&scopes, &r.TTLSeconds, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.Scopes = auth.TokenScope(scopes)
		r.Claims = map[string]string{}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		cl, err := s.loadRuleClaims(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Claims = cl
	}
	return out, nil
}

// ListOIDCRulesForRepo returns rules scoped to (tenant, repo) for CLI listing.
func (s *Store) ListOIDCRulesForRepo(ctx context.Context, tenant, repo string) ([]auth.OIDCTrustRule, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, issuer_alias, audience, tenant, repo, scopes, ttl_seconds, created_at
		   FROM oidc_trust_rules WHERE tenant = ? AND repo = ? ORDER BY id`, tenant, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []auth.OIDCTrustRule
	for rows.Next() {
		var r auth.OIDCTrustRule
		var scopes int64
		if err := rows.Scan(&r.ID, &r.IssuerAlias, &r.Audience, &r.Tenant, &r.Repo,
			&scopes, &r.TTLSeconds, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.Scopes = auth.TokenScope(scopes)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		cl, err := s.loadRuleClaims(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Claims = cl
	}
	return out, nil
}

func (s *Store) loadRuleClaims(ctx context.Context, ruleID string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT claim_name, claim_value FROM oidc_rule_claims WHERE rule_id = ?`, ruleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, rows.Err()
}

// RemoveOIDCRule deletes a rule (claims cascade). Returns ErrNoSuchOIDCRule
// if the id does not exist.
func (s *Store) RemoveOIDCRule(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM oidc_trust_rules WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoSuchOIDCRule
	}
	return nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// oidcSystemUserID is the reserved user inserted by migration 0010.
const oidcSystemUserID = "_oidc"

// OIDCMaxTTLSeconds is the hard ceiling on an OIDC trust rule's token TTL.
// Enforced at rule creation (the store is the chokepoint all minting flows
// through) so the short-lived-token blast-radius control cannot be bypassed
// by a non-CLI caller. Mirrors design §4.3 (≤ 1h).
const OIDCMaxTTLSeconds int64 = 3600

// MintOIDCParams describes a token to mint from a matched trust rule.
type MintOIDCParams struct {
	Tenant     string
	Repo       string
	Perm       auth.Perm // PermRead or PermWrite
	Scopes     auth.TokenScope
	TTLSeconds int64
	Label      string // "oidc:<alias>:<sub>"
}

// MintOIDCToken creates a short-lived repo-bound bvts token under the _oidc
// system user and returns the wire-format token string. The secret is shown
// only here (it is stored hashed).
//
// NOTE: the "scope_tenant/scope_repo/scope_perm are all-set-or-all-empty"
// invariant is NOT enforced by a table-level CHECK constraint because SQLite's
// ALTER TABLE cannot add a multi-column CHECK without a full table rebuild.
// The invariant is upheld by MintOIDCToken being the sole writer of these
// columns for OIDC tokens; any future writer must maintain it explicitly.
func (s *Store) MintOIDCToken(ctx context.Context, p MintOIDCParams) (string, error) {
	token, id, secret, err := auth.GenerateToken()
	if err != nil {
		return "", err
	}
	hash, err := auth.HashSecret(secret)
	if err != nil {
		return "", err
	}
	exp := time.Now().Unix() + p.TTLSeconds
	permStr := "read"
	if p.Perm == auth.PermWrite {
		permStr = "write"
	}
	if err := s.CreateToken(ctx, id, oidcSystemUserID, hash, p.Label, &exp,
		p.Scopes, p.Tenant, p.Repo, permStr); err != nil {
		return "", err
	}
	return token, nil
}

// SweepExpiredOIDCTokens deletes expired tokens owned by the _oidc system
// user and returns the number removed. Scoped to _oidc so it never touches
// operator-managed user tokens.
func (s *Store) SweepExpiredOIDCTokens(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM tokens WHERE user_id = ? AND expires_at IS NOT NULL AND expires_at < ?`,
		oidcSystemUserID, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ErrNoSuchOIDCIssuer and ErrNoSuchOIDCRule are not-found sentinels.
var (
	ErrNoSuchOIDCIssuer = errors.New("sqlitestore: no such oidc issuer")
	ErrNoSuchOIDCRule   = errors.New("sqlitestore: no such oidc rule")
)
