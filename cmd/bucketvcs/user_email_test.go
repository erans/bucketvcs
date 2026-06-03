package main

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

func TestUserSetEmail(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "auth.db")
	s, _ := sqlitestore.Open(dbPath)
	if _, err := s.CreateUser(context.Background(), "alice", false); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	s.Close()

	var out, errb bytes.Buffer
	code := userSetEmail(context.Background(), []string{"alice", "alice@corp.com", "--auth-db", dbPath}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, errb.String())
	}
	s2, _ := sqlitestore.Open(dbPath)
	defer s2.Close()
	if _, err := s2.FindUserByEmail(context.Background(), "alice@corp.com"); err != nil {
		t.Fatalf("FindUserByEmail after set: %v", err)
	}
}
