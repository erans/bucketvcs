package sqlitestore

import (
	"context"
	"testing"
)

func TestMigration0013_SchemaPresent(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	// schema_version reached 13
	var maxVer int
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&maxVer); err != nil {
		t.Fatalf("schema_version: %v", err)
	}
	if maxVer < 13 {
		t.Fatalf("schema_version max = %d, want >= 13", maxVer)
	}

	// sessions table exists and is queryable
	if _, err := s.db.ExecContext(ctx, `SELECT id_hash, user_id, provider, created_at, expires_at, last_seen FROM sessions WHERE 1=0`); err != nil {
		t.Fatalf("sessions table missing/shape wrong: %v", err)
	}

	// users.password_hash column exists
	if _, err := s.db.ExecContext(ctx, `SELECT password_hash FROM users WHERE 1=0`); err != nil {
		t.Fatalf("users.password_hash missing: %v", err)
	}
}
