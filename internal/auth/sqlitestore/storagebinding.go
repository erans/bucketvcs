package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// StorageBinding holds per-tenant object-store credentials for BYOB mode (M27).
// CredsJSON is AES-256-GCM encrypted at the application layer before storage.
type StorageBinding struct {
	Tenant     string
	StoreURL   string
	CredsJSON  []byte
	Provider   string
	CreatedAt  int64
	UpdatedAt  int64
	VerifiedAt int64
}

// GetStorageBinding returns the binding for tenant, or auth.ErrNoSuchBinding
// if none exists.
func (s *Store) GetStorageBinding(ctx context.Context, tenant string) (*StorageBinding, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT tenant, store_url, creds_json, provider, created_at, updated_at, verified_at
		   FROM storage_bindings WHERE tenant = ?`, tenant)
	var b StorageBinding
	if err := row.Scan(&b.Tenant, &b.StoreURL, &b.CredsJSON, &b.Provider,
		&b.CreatedAt, &b.UpdatedAt, &b.VerifiedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, auth.ErrNoSuchBinding
		}
		return nil, fmt.Errorf("get storage binding: %w", err)
	}
	return &b, nil
}

// UpsertStorageBinding inserts or fully replaces the binding for b.Tenant.
func (s *Store) UpsertStorageBinding(ctx context.Context, b StorageBinding) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO storage_bindings
		   (tenant, store_url, creds_json, provider, created_at, updated_at, verified_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(tenant) DO UPDATE SET
		   store_url   = excluded.store_url,
		   creds_json  = excluded.creds_json,
		   provider    = excluded.provider,
		   updated_at  = excluded.updated_at,
		   verified_at = excluded.verified_at`,
		b.Tenant, b.StoreURL, b.CredsJSON, b.Provider,
		b.CreatedAt, b.UpdatedAt, b.VerifiedAt)
	if err != nil {
		return fmt.Errorf("upsert storage binding: %w", err)
	}
	return nil
}

// DeleteStorageBinding removes the binding for tenant. Returns nil even if the
// tenant had no binding (idempotent).
func (s *Store) DeleteStorageBinding(ctx context.Context, tenant string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM storage_bindings WHERE tenant = ?`, tenant); err != nil {
		return fmt.Errorf("delete storage binding: %w", err)
	}
	return nil
}

// ListStorageBindings returns all bindings ordered by tenant.
func (s *Store) ListStorageBindings(ctx context.Context) ([]*StorageBinding, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tenant, store_url, creds_json, provider, created_at, updated_at, verified_at
		   FROM storage_bindings ORDER BY tenant`)
	if err != nil {
		return nil, fmt.Errorf("list storage bindings: %w", err)
	}
	defer rows.Close()
	var out []*StorageBinding
	for rows.Next() {
		var b StorageBinding
		if err := rows.Scan(&b.Tenant, &b.StoreURL, &b.CredsJSON, &b.Provider,
			&b.CreatedAt, &b.UpdatedAt, &b.VerifiedAt); err != nil {
			return nil, fmt.Errorf("scan storage binding: %w", err)
		}
		out = append(out, &b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list storage bindings: %w", err)
	}
	return out, nil
}
