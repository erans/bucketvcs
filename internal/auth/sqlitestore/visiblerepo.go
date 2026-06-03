package sqlitestore

import (
	"context"
	"database/sql"
	"errors"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// ErrRepoNotVisible means the repo does not exist or the actor may not see it.
// Callers MUST NOT distinguish the two (anti-enumeration).
var ErrRepoNotVisible = errors.New("sqlitestore: repo not visible")

// GetVisibleRepo returns the repo if the actor may see it under the same rules
// as ListAccessibleRepos (anon → public only; user → public + granted; admin →
// all). Both "absent" and "not authorized" return ErrRepoNotVisible.
func (s *Store) GetVisibleRepo(ctx context.Context, actor *auth.Actor, tenant, name string) (*Repo, error) {
	r := &Repo{}
	var pub int
	err := s.db.QueryRowContext(ctx,
		`SELECT tenant, name, public_read, created_at FROM repos WHERE tenant = ? AND name = ?`,
		tenant, name).Scan(&r.Tenant, &r.Name, &pub, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRepoNotVisible
	}
	if err != nil {
		return nil, err
	}
	r.PublicRead = pub != 0

	if actor != nil && actor.IsAdmin {
		return r, nil
	}
	if r.PublicRead {
		return r, nil
	}
	if actor == nil {
		return nil, ErrRepoNotVisible
	}
	var one int
	err = s.db.QueryRowContext(ctx,
		`SELECT 1 FROM repo_permissions WHERE tenant = ? AND repo = ? AND user_id = ? LIMIT 1`,
		tenant, name, actor.UserID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRepoNotVisible
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}
