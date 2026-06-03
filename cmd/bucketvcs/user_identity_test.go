package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

func TestUserIdentityListRemove(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "auth.db")
	s, _ := sqlitestore.Open(dbPath)
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "alice", false)
	if err := s.LinkIdentity(ctx, uid, "https://idp", "sub-9", "alice@corp.com"); err != nil {
		t.Fatalf("LinkIdentity: %v", err)
	}
	s.Close()

	var out, errb bytes.Buffer
	if code := runUserIdentity(ctx, []string{"list", "alice", "--auth-db", dbPath}, &out, &errb); code != 0 {
		t.Fatalf("list exit %d; stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "sub-9") || !strings.Contains(out.String(), "https://idp") {
		t.Fatalf("list output missing identity:\n%s", out.String())
	}

	out.Reset()
	errb.Reset()
	if code := runUserIdentity(ctx, []string{"remove", "https://idp", "sub-9", "--auth-db", dbPath}, &out, &errb); code != 0 {
		t.Fatalf("remove exit %d; stderr=%s", code, errb.String())
	}
	s2, _ := sqlitestore.Open(dbPath)
	defer s2.Close()
	ids, _ := s2.ListIdentities(ctx, "alice")
	if len(ids) != 0 {
		t.Fatalf("identity not removed: %+v", ids)
	}
}
