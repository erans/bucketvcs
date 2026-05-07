package sqlitestore

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpen_CreatesFileAndAppliesMigrations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	var pragma string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&pragma); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if pragma != "wal" {
		t.Errorf("journal_mode = %q, want wal", pragma)
	}

	var fk int
	if err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}

	var v int
	if err := s.db.QueryRow("SELECT MAX(version) FROM schema_version").Scan(&v); err != nil {
		t.Fatalf("schema_version: %v", err)
	}
	if v != 1 {
		t.Errorf("schema_version = %d, want 1", v)
	}
}

func TestOpen_ReopenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.db")
	a, err := Open(path)
	if err != nil {
		t.Fatalf("Open a: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close a: %v", err)
	}
	b, err := Open(path)
	if err != nil {
		t.Fatalf("Open b: %v", err)
	}
	defer b.Close()
	if err := b.db.PingContext(context.Background()); err != nil {
		t.Fatalf("Ping after reopen: %v", err)
	}
}
