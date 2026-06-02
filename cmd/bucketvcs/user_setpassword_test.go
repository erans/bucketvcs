package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

func TestUserSetPassword_Stdin(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "auth.db")
	s, err := sqlitestore.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.CreateUser(context.Background(), "alice", false); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	s.Close()

	var out, errb bytes.Buffer
	code := userSetPassword(context.Background(),
		[]string{"alice", "--auth-db", dbPath, "--password-stdin"},
		&out, &errb, strings.NewReader("s3cret\n"))
	if code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, errb.String())
	}

	// verify it took
	s2, _ := sqlitestore.Open(dbPath)
	defer s2.Close()
	if _, err := s2.VerifyPassword(context.Background(), "alice", "s3cret"); err != nil {
		t.Fatalf("VerifyPassword after set: %v", err)
	}
}
