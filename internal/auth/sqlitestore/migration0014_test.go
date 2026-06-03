package sqlitestore

import (
	"context"
	"testing"
)

func TestMigration0014_SchemaPresent(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	var maxVer int
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&maxVer); err != nil {
		t.Fatalf("schema_version: %v", err)
	}
	if maxVer < 14 {
		t.Fatalf("schema_version max = %d, want >= 14", maxVer)
	}
	if _, err := s.db.ExecContext(ctx, `SELECT email FROM users WHERE 1=0`); err != nil {
		t.Fatalf("users.email missing: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `SELECT id, user_id, provider, issuer, subject, email, created_at FROM user_identities WHERE 1=0`); err != nil {
		t.Fatalf("user_identities missing/shape wrong: %v", err)
	}
}
