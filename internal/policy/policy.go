package policy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/repo/oidconst"
)

// ProtectedRef is one row in the protected_refs table.
type ProtectedRef struct {
	Tenant         string
	Repo           string
	RefnamePattern string
	BlockDeletion  bool
	BlockForcePush bool
	CreatedAt      time.Time
}

// Service wraps the protected_refs table on the authdb. All methods
// are safe for concurrent use; sqlite's single-writer model serializes
// writes.
type Service struct {
	db sqlitestore.Querier
}

// New constructs a Service.
func New(db sqlitestore.Querier) *Service {
	return &Service{db: db}
}

// Add creates or updates a protected-ref rule. Validates the glob
// pattern via path.Match before INSERT — malformed patterns reject
// at Add time so they can't silently break receive-pack later.
func (s *Service) Add(ctx context.Context, r ProtectedRef) error {
	if r.RefnamePattern == "" {
		return fmt.Errorf("policy: refname_pattern must not be empty")
	}
	if _, err := path.Match(r.RefnamePattern, ""); err != nil {
		return fmt.Errorf("policy: invalid refname_pattern %q: %w", r.RefnamePattern, err)
	}
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO protected_refs
			(tenant, repo, refname_pattern, block_deletion, block_force_push, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(tenant, repo, refname_pattern) DO UPDATE SET
			block_deletion   = excluded.block_deletion,
			block_force_push = excluded.block_force_push
	`, r.Tenant, r.Repo, r.RefnamePattern, boolToInt(r.BlockDeletion), boolToInt(r.BlockForcePush), now)
	if err != nil {
		return fmt.Errorf("policy add %q/%q %q: %w", r.Tenant, r.Repo, r.RefnamePattern, err)
	}
	return nil
}

// List returns every rule for (tenant, repo) ordered by pattern.
func (s *Service) List(ctx context.Context, tenant, repo string) ([]ProtectedRef, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT tenant, repo, refname_pattern, block_deletion, block_force_push, created_at
		FROM protected_refs
		WHERE tenant = ? AND repo = ?
		ORDER BY refname_pattern
	`, tenant, repo)
	if err != nil {
		return nil, fmt.Errorf("policy list %q/%q: %w", tenant, repo, err)
	}
	defer rows.Close()
	var out []ProtectedRef
	for rows.Next() {
		var (
			r         ProtectedRef
			blockDel  int
			blockFP   int
			createdAt int64
		)
		if err := rows.Scan(&r.Tenant, &r.Repo, &r.RefnamePattern, &blockDel, &blockFP, &createdAt); err != nil {
			return nil, fmt.Errorf("policy list scan: %w", err)
		}
		r.BlockDeletion = blockDel != 0
		r.BlockForcePush = blockFP != 0
		r.CreatedAt = time.Unix(createdAt, 0).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// Remove deletes the rule whose pattern matches exactly (no glob
// expansion of the pattern itself). Removing a non-existent pattern
// is a no-op.
func (s *Service) Remove(ctx context.Context, tenant, repo, pattern string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM protected_refs WHERE tenant = ? AND repo = ? AND refname_pattern = ?`,
		tenant, repo, pattern,
	)
	if err != nil {
		return fmt.Errorf("policy remove %q/%q %q: %w", tenant, repo, pattern, err)
	}
	return nil
}

// PolicyError is returned by CheckUpdate when a ref update is rejected.
// Callers (receive-pack step 8b) use errors.As to recover the structured
// fields for the `ng <refname> protected-branch: <reason>` report-status
// line and for the policy.ref.rejected audit event.
type PolicyError struct {
	Refname        string
	MatchedPattern string
	Reason         string // "deletion blocked" | "non-fast-forward push blocked" | "blocked_path"
	OldOID         string
	NewOID         string
	MatchedPath    string // M16: populated only when Reason == "blocked_path"
}

func (e *PolicyError) Error() string {
	base := fmt.Sprintf("protected-branch: %s by pattern %s (refname=%s)",
		e.Reason, e.MatchedPattern, e.Refname)
	if e.MatchedPath != "" {
		base += " path=" + e.MatchedPath
	}
	return base
}

// MetricOutcome returns the value used as the {outcome} label on
// policy_refs_check_total when this error is the cause of rejection.
// Stable across rule changes — operators rely on this for alerts.
func (e *PolicyError) MetricOutcome() string {
	switch e.Reason {
	case "deletion blocked":
		return "blocked_deletion"
	case "non-fast-forward push blocked":
		return "blocked_force_push"
	case "blocked_path":
		return "blocked_path"
	default:
		return "blocked_other"
	}
}

// CheckUpdate runs all matching rules against one ref update.
// bareDir is the local bare repository directory used for fast-forward
// detection via `git merge-base --is-ancestor`. oldOID and newOID use
// the receivepack convention: a 40-zero hex string ("0000...") means
// "absent" (new ref creation when in oldOID; ref deletion when in
// newOID). Returns *PolicyError on rejection, or nil to accept. ANY
// matching rule that blocks the operation triggers rejection.
//
// On non-policy errors (sqlite read failure, git subprocess failure),
// returns the underlying error wrapped — caller surfaces these as
// `internal-error` rather than `protected-branch` status lines.
func (s *Service) CheckUpdate(ctx context.Context, tenant, repo, bareDir string,
	refname, oldOID, newOID string) error {

	rules, err := s.List(ctx, tenant, repo)
	if err != nil {
		return err
	}
	if len(rules) == 0 {
		return nil
	}

	isDeletion := newOID == oidconst.NullOIDHex
	isCreation := oldOID == oidconst.NullOIDHex
	isUpdate := !isDeletion && !isCreation

	for _, r := range rules {
		matched, merr := path.Match(r.RefnamePattern, refname)
		if merr != nil {
			// Malformed pattern at lookup time (the Add-time guard
			// should have caught it, but a future direct-SQL edit
			// might bypass that). Treat as internal error.
			return fmt.Errorf("policy: pattern %q invalid: %w", r.RefnamePattern, merr)
		}
		if !matched {
			continue
		}
		if isDeletion && r.BlockDeletion {
			return &PolicyError{
				Refname: refname, MatchedPattern: r.RefnamePattern,
				Reason: "deletion blocked",
				OldOID: oldOID, NewOID: newOID,
			}
		}
		if isUpdate && r.BlockForcePush {
			isFF, ferr := isFastForward(ctx, bareDir, oldOID, newOID)
			if ferr != nil {
				return fmt.Errorf("policy: merge-base check for %s: %w", refname, ferr)
			}
			if !isFF {
				return &PolicyError{
					Refname: refname, MatchedPattern: r.RefnamePattern,
					Reason: "non-fast-forward push blocked",
					OldOID: oldOID, NewOID: newOID,
				}
			}
		}
		// New-ref creation is never rejected by Tier 1 rules.
	}
	return nil
}

// isFastForward reports whether oldOID is an ancestor of newOID in
// the local bare. Calls `git merge-base --is-ancestor <old> <new>`:
//
//	exit 0 -> ancestor (fast-forward; ok to update)
//	exit 1 -> not ancestor (non-FF; reject)
//	exit 2 or other -> error (corrupt bare, missing OID, etc.)
func isFastForward(ctx context.Context, bareDir, oldOID, newOID string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "--no-replace-objects", "-C", bareDir,
		"merge-base", "--is-ancestor", oldOID, newOID)
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		switch ee.ExitCode() {
		case 1:
			return false, nil
		default:
			return false, fmt.Errorf("merge-base --is-ancestor exit=%d: %s",
				ee.ExitCode(), stderr.String())
		}
	}
	return false, fmt.Errorf("merge-base --is-ancestor: %w (stderr: %s)", err, stderr.String())
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ErrNotFound is reserved for future callers that need to distinguish
// "no rule" from "no rows". Currently unused but exported so the API
// shape is stable.
var ErrNotFound = errors.New("policy: not found")

// ErrInvalidInput is returned when CRUD inputs fail validation
// (empty required fields, malformed patterns, etc.).
var ErrInvalidInput = errors.New("policy: invalid input")

// ErrConflict is reserved for future strict-mode CRUD entry points that
// reject duplicate inserts instead of upserting. Idempotent-mode CRUD
// (current Add* methods) uses INSERT ... ON CONFLICT DO NOTHING and
// returns nil on duplicates, so this sentinel is currently a
// forward-declaration with no production return site.
var ErrConflict = errors.New("policy: conflict")
