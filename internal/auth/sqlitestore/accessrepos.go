package sqlitestore

import (
	"context"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// ListAccessibleRepos returns repos the actor may see, ordered by tenant,name:
//   - nil actor (anonymous): public-read repos only
//   - admin: all repos
//   - otherwise: public-read repos UNION repos the user has any grant on
func (s *Store) ListAccessibleRepos(ctx context.Context, actor *auth.Actor) ([]*Repo, error) {
	if actor != nil && actor.IsAdmin {
		return s.ListRepos(ctx, "")
	}
	if actor == nil {
		rows, err := s.db.QueryContext(ctx,
			`SELECT tenant, name, public_read, created_at FROM repos
			  WHERE public_read = 1 ORDER BY tenant, name`)
		if err != nil {
			return nil, err
		}
		return scanRepos(rows)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT r.tenant, r.name, r.public_read, r.created_at
		   FROM repos r
		   LEFT JOIN repo_permissions p
		     ON p.tenant = r.tenant AND p.repo = r.name AND p.user_id = ?
		  WHERE r.public_read = 1 OR p.user_id IS NOT NULL
		  ORDER BY r.tenant, r.name`, actor.UserID)
	if err != nil {
		return nil, err
	}
	return scanRepos(rows)
}

func scanRepos(rows interface {
	Next() bool
	Scan(...any) error
	Close() error
	Err() error
}) ([]*Repo, error) {
	defer rows.Close()
	out := []*Repo{}
	for rows.Next() {
		r := &Repo{}
		var pub int
		if err := rows.Scan(&r.Tenant, &r.Name, &pub, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.PublicRead = pub != 0
		out = append(out, r)
	}
	return out, rows.Err()
}
