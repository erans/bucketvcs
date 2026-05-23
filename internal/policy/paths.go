package policy

import (
	"context"
	"fmt"
	"path"
	"time"
)

// ProtectedPath is one row in protected_paths (migration 0007).
type ProtectedPath struct {
	Tenant         string
	Repo           string
	RefnamePattern string
	PathPattern    string
	CreatedAt      time.Time
}

// AddPathRule inserts a path rule. Validates path_pattern via
// ValidatePathPattern and refname_pattern via stdlib path.Match before
// insert. Returns ErrInvalidInput on bad pattern. Idempotent via
// INSERT ... ON CONFLICT DO NOTHING; re-adding the same (tenant, repo,
// refname_pattern, path_pattern) returns nil without modifying the
// existing row's created_at.
func (s *Service) AddPathRule(ctx context.Context, in ProtectedPath) error {
	if in.Tenant == "" || in.Repo == "" || in.RefnamePattern == "" || in.PathPattern == "" {
		return fmt.Errorf("%w: tenant, repo, refname_pattern, path_pattern all required",
			ErrInvalidInput)
	}
	if err := ValidatePathPattern(in.PathPattern); err != nil {
		return fmt.Errorf("%w: invalid path_pattern: %s", ErrInvalidInput, err.Error())
	}
	// Validate refname_pattern via stdlib path.Match (refname patterns
	// use path.Match semantics per M14). path.Match returns
	// ErrBadPattern for e.g. unclosed character classes. Without this
	// gate, a malformed refname_pattern silently no-ops in CheckPaths:
	// the rule is inert with no operator-visible signal.
	if _, err := path.Match(in.RefnamePattern, ""); err != nil {
		return fmt.Errorf("%w: invalid refname_pattern: %s", ErrInvalidInput, err.Error())
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO protected_paths
		   (tenant, repo, refname_pattern, path_pattern, created_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(tenant, repo, refname_pattern, path_pattern) DO NOTHING`,
		in.Tenant, in.Repo, in.RefnamePattern, in.PathPattern, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("policy: add path rule: %w", err)
	}
	return nil
}

// ListPathRules returns all path rules for (tenant, repo) ordered by
// (refname_pattern, path_pattern) ascending.
func (s *Service) ListPathRules(ctx context.Context, tenant, repo string) ([]ProtectedPath, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tenant, repo, refname_pattern, path_pattern, created_at
		 FROM protected_paths
		 WHERE tenant=? AND repo=?
		 ORDER BY refname_pattern, path_pattern`,
		tenant, repo)
	if err != nil {
		return nil, fmt.Errorf("policy: list path rules: %w", err)
	}
	defer rows.Close()
	var out []ProtectedPath
	for rows.Next() {
		var r ProtectedPath
		var createdAt int64
		if err := rows.Scan(&r.Tenant, &r.Repo, &r.RefnamePattern, &r.PathPattern, &createdAt); err != nil {
			return nil, fmt.Errorf("policy: scan path rule: %w", err)
		}
		r.CreatedAt = time.Unix(createdAt, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// RemovePathRule deletes the row matching the four-tuple. Returns
// ErrNotFound if no row matches.
func (s *Service) RemovePathRule(ctx context.Context, tenant, repo, refnamePattern, pathPattern string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM protected_paths
		 WHERE tenant=? AND repo=? AND refname_pattern=? AND path_pattern=?`,
		tenant, repo, refnamePattern, pathPattern,
	)
	if err != nil {
		return fmt.Errorf("policy: remove path rule: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("policy: remove path rule rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// CheckPaths walks the protected_paths rules for (tenant, repo). For each
// rule whose refname_pattern matches refname (via path.Match per M14), checks
// every entry in changedPaths against path_pattern (via MatchPath in this
// package). First-match-rejects with rules iterated alphabetically by
// (refname_pattern, path_pattern) for deterministic MatchedPath.
//
// Returns nil if no rule fires. Returns *PolicyError with Reason="blocked_path"
// on rejection. Sqlite read errors are returned as plain errors (NOT wrapped
// in PolicyError) so the receivepack caller can distinguish operator-facing
// policy decisions from internal failures.
func (s *Service) CheckPaths(ctx context.Context, tenant, repo, refname string,
	changedPaths []string) error {
	if len(changedPaths) == 0 {
		return nil
	}
	rules, err := s.ListPathRules(ctx, tenant, repo)
	if err != nil {
		return fmt.Errorf("policy: check paths: %w", err)
	}
	for _, rule := range rules {
		refOK, perr := path.Match(rule.RefnamePattern, refname)
		if perr != nil || !refOK {
			continue
		}
		for _, changed := range changedPaths {
			pathOK, perr := MatchPath(rule.PathPattern, changed)
			if perr != nil {
				return fmt.Errorf("policy: check paths: bad pattern %q: %w",
					rule.PathPattern, perr)
			}
			if pathOK {
				return &PolicyError{
					Refname:        refname,
					MatchedPattern: rule.PathPattern,
					Reason:         "blocked_path",
					MatchedPath:    changed,
				}
			}
		}
	}
	return nil
}
