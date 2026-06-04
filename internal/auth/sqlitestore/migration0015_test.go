package sqlitestore

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestMigration0015SqliteNoOp asserts 0015 applies on sqlite (version row
// present) and that the decorative webhook_endpoints FK is left in place —
// sqlite suppresses it via PRAGMA foreign_keys=OFF in DeleteRepoCascade, so
// the schema does not change.
func TestMigration0015SqliteNoOp(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	var n int
	if err := s.db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM schema_version WHERE version=15`).Scan(&n); err != nil {
		t.Fatalf("schema_version query: %v", err)
	}
	if n != 1 {
		t.Fatalf("migration 0015 not applied: count=%d", n)
	}

	var ddl string
	if err := s.db.QueryRowContext(t.Context(),
		`SELECT sql FROM sqlite_master WHERE name='webhook_endpoints'`).Scan(&ddl); err != nil {
		t.Fatalf("sqlite_master query: %v", err)
	}
	if !strings.Contains(ddl, "FOREIGN KEY") {
		t.Fatalf("sqlite webhook_endpoints lost its (decorative) FK:\n%s", ddl)
	}
}
