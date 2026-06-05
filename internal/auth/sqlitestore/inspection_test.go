package sqlitestore

import (
	"context"
	"io/fs"
	"path/filepath"
	"testing"
)

func TestSchemaVersionAndLatest(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "auth.db")
	s, err := Open(path) // applies all migrations
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	v, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if want := LatestMigrationVersion(); v != want {
		t.Fatalf("SchemaVersion=%d, want LatestMigrationVersion=%d", v, want)
	}
	if v < 15 {
		t.Fatalf("SchemaVersion=%d; expected at least 15 (M25 migration)", v)
	}
	s.Close()

	// OpenForInspection must NOT migrate: simulate an older db by deleting
	// the newest version row, reopen for inspection, version must stay stale.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := s2.db.ExecContext(ctx, `DELETE FROM schema_version WHERE version=?`, LatestMigrationVersion()); err != nil {
		t.Fatalf("simulate stale db: %v", err)
	}
	s2.Close()

	insp, err := OpenForInspection(path)
	if err != nil {
		t.Fatalf("OpenForInspection: %v", err)
	}
	defer insp.Close()
	v2, err := insp.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion (inspection): %v", err)
	}
	if v2 != LatestMigrationVersion()-1 {
		t.Fatalf("OpenForInspection migrated the db: version=%d, want %d", v2, LatestMigrationVersion()-1)
	}
}

// TestMigrationSetsInLockstep asserts the sqlite and postgres migration dirs
// carry identical version numbers — LatestMigrationVersion (computed from the
// sqlite set) is only meaningful for both backends under this invariant.
func TestMigrationSetsInLockstep(t *testing.T) {
	versions := func(fsys fs.FS, dir string) map[int]bool {
		t.Helper()
		entries, err := fs.ReadDir(fsys, dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		out := map[int]bool{}
		for _, e := range entries {
			v, err := parseVersion(e.Name())
			if err != nil {
				t.Fatalf("parse %s: %v", e.Name(), err)
			}
			out[v] = true
		}
		return out
	}
	sq := versions(migrationsFS, "migrations")
	pg := versions(postgresMigrations, "migrations_postgres")
	if len(sq) != len(pg) {
		t.Fatalf("migration count differs: sqlite=%d postgres=%d", len(sq), len(pg))
	}
	for v := range sq {
		if !pg[v] {
			t.Errorf("version %d in sqlite set but not postgres", v)
		}
	}
}
