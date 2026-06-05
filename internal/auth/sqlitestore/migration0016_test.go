package sqlitestore

import (
	"context"
	"testing"
)

// TestMigration0016_StorageBindings verifies that migration 0016 is applied and
// the storage_bindings table is functional.
func TestMigration0016_StorageBindings(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	// Version row must exist.
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_version WHERE version=16`).Scan(&n); err != nil {
		t.Fatalf("schema_version query: %v", err)
	}
	if n != 1 {
		t.Fatalf("migration 0016 not applied: count=%d", n)
	}

	// Table must accept an INSERT.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO storage_bindings (tenant, store_url, creds_json, provider, created_at, updated_at, verified_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"test-tenant", "s3://bucket", []byte(`{}`), "s3compat", 1, 2, 3); err != nil {
		t.Fatalf("INSERT into storage_bindings: %v", err)
	}

	// And DELETE must succeed.
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM storage_bindings WHERE tenant=?`, "test-tenant"); err != nil {
		t.Fatalf("DELETE from storage_bindings: %v", err)
	}
}
