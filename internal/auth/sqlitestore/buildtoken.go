package sqlitestore

import (
	"context"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// MintBuildParams is the input to MintBuildToken.
type MintBuildParams struct {
	Tenant     string
	Repo       string
	Scopes     auth.TokenScope
	TTLSeconds int64
	Label      string // "build:<tenant>/<repo>:<trigger-name>"
}

// MintBuildToken creates a short-lived, single-repo, read-only bvts token under
// the _build system user and returns the wire-format token string. The scope_perm
// is always "read" — write access is not granted to CI build tokens by design.
func (s *Store) MintBuildToken(ctx context.Context, p MintBuildParams) (string, error) {
	token, id, secret, err := auth.GenerateToken()
	if err != nil {
		return "", err
	}
	hash, err := auth.HashSecret(secret)
	if err != nil {
		return "", err
	}
	exp := time.Now().Unix() + p.TTLSeconds
	if err := s.CreateToken(ctx, id, buildSystemUserID, hash, p.Label, &exp,
		p.Scopes, p.Tenant, p.Repo, "read"); err != nil {
		return "", err
	}
	return token, nil
}

// SweepExpiredBuildTokens deletes expired tokens owned by the _build system
// user and returns the number removed. Scoped to _build so it never touches
// operator-managed user tokens or OIDC-minted tokens.
func (s *Store) SweepExpiredBuildTokens(ctx context.Context) (int64, error) {
	return s.sweepExpiredTokensForUser(ctx, buildSystemUserID)
}

// sweepExpiredTokensForUser deletes expired tokens owned by one reserved system
// user. Shared by the _oidc (M22) and _build (M30) sweeps so they never diverge.
func (s *Store) sweepExpiredTokensForUser(ctx context.Context, userID string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM tokens WHERE user_id = ? AND expires_at IS NOT NULL AND expires_at < ?`,
		userID, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
