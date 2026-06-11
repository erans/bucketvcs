package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

// seedSessionDB creates an auth DB with two users and three sessions
// (2x alice, 1x bob) and returns its path plus alice's raw session ids.
func seedSessionDB(t *testing.T) (dbPath string, aliceRaws []string) {
	t.Helper()
	dbPath = filepath.Join(t.TempDir(), "auth.db")
	s, err := sqlitestore.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	aliceID, err := s.CreateUser(ctx, "alice", false)
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	bobID, err := s.CreateUser(ctx, "bob", false)
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}
	for i := 0; i < 2; i++ {
		raw, err := s.CreateSession(ctx, aliceID, "password", time.Hour)
		if err != nil {
			t.Fatalf("alice session %d: %v", i, err)
		}
		aliceRaws = append(aliceRaws, raw)
	}
	if _, err := s.CreateSession(ctx, bobID, "oidc", time.Hour); err != nil {
		t.Fatalf("bob session: %v", err)
	}
	return dbPath, aliceRaws
}

func TestSessionList_NDJSON(t *testing.T) {
	db, _ := seedSessionDB(t)
	var out, errb bytes.Buffer
	code := runSession(context.Background(), []string{"list", "--auth-db", db}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3:\n%s", len(lines), out.String())
	}
	users := map[string]int{}
	for _, ln := range lines {
		var row map[string]any
		if err := json.Unmarshal([]byte(ln), &row); err != nil {
			t.Fatalf("bad NDJSON line %q: %v", ln, err)
		}
		for _, k := range []string{"id_hash", "user_id", "user", "provider", "created_at", "expires_at", "last_seen"} {
			if _, ok := row[k]; !ok {
				t.Fatalf("line missing %q: %s", k, ln)
			}
		}
		users[row["user"].(string)]++
	}
	if users["alice"] != 2 || users["bob"] != 1 {
		t.Fatalf("user counts = %v, want alice:2 bob:1", users)
	}
}

func TestSessionList_UserFilter(t *testing.T) {
	db, _ := seedSessionDB(t)
	var out, errb bytes.Buffer
	code := runSession(context.Background(), []string{"list", "--auth-db", db, "--user", "bob"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], `"user":"bob"`) {
		t.Fatalf("filtered output:\n%s", out.String())
	}
}

func TestSessionList_UsageErrors(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runSession(context.Background(), nil, &out, &errb); code != 2 {
		t.Fatalf("no subcommand: exit %d, want 2", code)
	}
	if code := runSession(context.Background(), []string{"list"}, &out, &errb); code != 2 {
		t.Fatalf("missing --auth-db: exit %d, want 2", code)
	}
}

func TestSessionRevoke_ByHash(t *testing.T) {
	db, aliceRaws := seedSessionDB(t)
	hash := auth.HashSessionID(aliceRaws[0])
	var out, errb bytes.Buffer
	code := runSession(context.Background(), []string{"revoke", "--auth-db", db, "--id-hash", hash}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "revoked=1") {
		t.Fatalf("stdout %q, want revoked=1", out.String())
	}
	// Idempotent: second revoke reports 0 and still exits 0.
	out.Reset()
	if code := runSession(context.Background(), []string{"revoke", "--auth-db", db, "--id-hash", hash}, &out, &errb); code != 0 {
		t.Fatalf("re-revoke exit %d", code)
	}
	if !strings.Contains(out.String(), "revoked=0") {
		t.Fatalf("stdout %q, want revoked=0", out.String())
	}
}

func TestSessionRevoke_ByUser(t *testing.T) {
	db, _ := seedSessionDB(t)
	var out, errb bytes.Buffer
	code := runSession(context.Background(), []string{"revoke", "--auth-db", db, "--user", "alice"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "revoked=2") {
		t.Fatalf("stdout %q, want revoked=2 (both alice sessions)", out.String())
	}
	// bob's session survives.
	out.Reset()
	if code := runSession(context.Background(), []string{"list", "--auth-db", db}, &out, &errb); code != 0 {
		t.Fatalf("list exit %d", code)
	}
	if got := strings.Count(out.String(), `"user":"bob"`); got != 1 {
		t.Fatalf("bob sessions after alice revoke = %d, want 1", got)
	}
	if strings.Contains(out.String(), `"user":"alice"`) {
		t.Fatal("alice sessions remain after revoke --user")
	}
}

func TestSessionRevoke_UsageErrors(t *testing.T) {
	db, aliceRaws := seedSessionDB(t)
	hash := auth.HashSessionID(aliceRaws[0])
	var out, errb bytes.Buffer
	// both flags
	if code := runSession(context.Background(), []string{"revoke", "--auth-db", db, "--id-hash", hash, "--user", "alice"}, &out, &errb); code != 2 {
		t.Fatalf("both flags: exit %d, want 2", code)
	}
	// neither flag
	if code := runSession(context.Background(), []string{"revoke", "--auth-db", db}, &out, &errb); code != 2 {
		t.Fatalf("neither flag: exit %d, want 2", code)
	}
	// unknown user
	if code := runSession(context.Background(), []string{"revoke", "--auth-db", db, "--user", "nobody"}, &out, &errb); code != 1 {
		t.Fatalf("unknown user: exit %d, want 1", code)
	}
}
