package sqlitestore

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
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

func TestCreateUser_AndGetByName(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	id, err := s.CreateUser(ctx, "alice", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if id == "" {
		t.Fatal("CreateUser returned empty id")
	}
	got, err := s.GetUserByName(ctx, "alice")
	if err != nil {
		t.Fatalf("GetUserByName: %v", err)
	}
	if got.ID != id || got.Name != "alice" || got.IsAdmin {
		t.Fatalf("got %+v", got)
	}
}

func TestCreateUser_DuplicateName(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "alice", false); err != nil {
		t.Fatalf("first: %v", err)
	}
	_, err := s.CreateUser(ctx, "alice", false)
	if !errors.Is(err, auth.ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
}

func TestSetUserDisabled_AndDelete(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	id, _ := s.CreateUser(ctx, "alice", false)

	if err := s.SetUserDisabled(ctx, "alice", true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	u, _ := s.GetUserByName(ctx, "alice")
	if u.DisabledAt == nil {
		t.Fatal("expected DisabledAt set")
	}
	if err := s.SetUserDisabled(ctx, "alice", false); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	u, _ = s.GetUserByName(ctx, "alice")
	if u.DisabledAt != nil {
		t.Fatal("expected DisabledAt cleared")
	}
	if err := s.DeleteUser(ctx, "alice"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if _, err := s.GetUserByName(ctx, "alice"); !errors.Is(err, auth.ErrNoSuchUser) {
		t.Fatalf("want ErrNoSuchUser, got %v", err)
	}
	_ = id
}

func TestDeleteUser_RefusesLastAdmin(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "root", true); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	err := s.DeleteUser(ctx, "root")
	if err == nil || !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("want ErrLastAdmin, got %v", err)
	}
}

func TestListUsers(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	_, _ = s.CreateUser(ctx, "alice", false)
	_, _ = s.CreateUser(ctx, "bob", true)
	got, err := s.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
}

// mustOpen is a tiny test helper.
func mustOpen(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "auth.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}
