package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
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

func TestSessionList_UnknownUserErrors(t *testing.T) {
	// --user with a nonexistent name must not read as "no sessions": list
	// validates the name up front, like revoke.
	db, _ := seedSessionDB(t)
	var out, errb bytes.Buffer
	code := runSession(context.Background(), []string{"list", "--auth-db", db, "--user", "nobody"}, &out, &errb)
	if code != 1 {
		t.Fatalf("unknown user: exit %d, want 1; stderr: %s", code, errb.String())
	}
	if out.Len() != 0 {
		t.Fatalf("unknown user must print nothing to stdout, got:\n%s", out.String())
	}
}

func TestSessionList_UsageErrors(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runSession(context.Background(), nil, &out, &errb); code != 2 {
		t.Fatalf("no subcommand: exit %d, want 2", code)
	}
	if code := runSession(context.Background(), []string{"bogus"}, &out, &errb); code != 2 {
		t.Fatalf("unknown subcommand: exit %d, want 2", code)
	}
}

func TestSessionList_AuthDBResolvedFromEnv(t *testing.T) {
	// No --auth-db is no longer a usage error: the path resolves like the
	// other auth-DB commands (flag → BUCKETVCS_AUTH_DB → XDG default). An
	// env-resolved missing path still fails the no-create stat check.
	t.Setenv("BUCKETVCS_AUTH_DB", filepath.Join(t.TempDir(), "nope", "auth.db"))
	var out, errb bytes.Buffer
	if code := runSession(context.Background(), []string{"list"}, &out, &errb); code != 1 {
		t.Fatalf("env-resolved missing db: exit %d, want 1; stderr: %s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "no such file") {
		t.Fatalf("want stat failure on env-resolved path, stderr: %s", errb.String())
	}
}

func TestSessionList_DSNAuthDBSkipsStat(t *testing.T) {
	// postgres:// DSNs are not filesystem paths; they must reach the
	// backend (and fail at connect) rather than dying on os.Stat.
	var out, errb bytes.Buffer
	code := runSession(context.Background(), []string{"list", "--auth-db", "postgres://user@127.0.0.1:1/db"}, &out, &errb)
	if code != 1 {
		t.Fatalf("dsn auth-db: exit %d, want 1 (connect failure); stderr: %s", code, errb.String())
	}
	if strings.Contains(errb.String(), "no such file or directory") {
		t.Fatalf("dsn auth-db must not be stat-checked, stderr: %s", errb.String())
	}
}

func TestSessionList_SQLiteDSNAuthDB(t *testing.T) {
	// sqlite:-scheme DSNs name a real on-disk file; the no-create stat
	// check must strip the scheme (via sqlitestore.SQLitePath) instead of
	// stat-ing the literal "sqlite:/..." string.
	db, _ := seedSessionDB(t)
	var out, errb bytes.Buffer
	code := runSession(context.Background(), []string{"list", "--auth-db", "sqlite:" + db}, &out, &errb)
	if code != 0 {
		t.Fatalf("sqlite: DSN auth-db: exit %d, want 0; stderr: %s", code, errb.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3:\n%s", len(lines), out.String())
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
	for _, want := range []string{"auth.session.admin_revoked", "target_user=alice", "count=1"} {
		if !strings.Contains(errb.String(), want) {
			t.Fatalf("stderr missing %q:\n%s", want, errb.String())
		}
	}
	// Idempotent: second revoke reports 0 and still exits 0, with no audit line.
	out.Reset()
	errb.Reset()
	if code := runSession(context.Background(), []string{"revoke", "--auth-db", db, "--id-hash", hash}, &out, &errb); code != 0 {
		t.Fatalf("re-revoke exit %d", code)
	}
	if !strings.Contains(out.String(), "revoked=0") {
		t.Fatalf("stdout %q, want revoked=0", out.String())
	}
	if strings.Contains(errb.String(), "auth.session.admin_revoked") {
		t.Fatalf("no-op revoke must not emit audit line, stderr:\n%s", errb.String())
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
	for _, want := range []string{"target_user=alice", "count=2"} {
		if !strings.Contains(errb.String(), want) {
			t.Fatalf("stderr missing %q:\n%s", want, errb.String())
		}
	}
	// id_hash is not applicable on the by-user path and must be omitted.
	if strings.Contains(errb.String(), "id_hash=") {
		t.Fatalf("stderr contains id_hash= on --user path:\n%s", errb.String())
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

func TestSessionRevoke_OwnerLookupFailureIsUnresolved(t *testing.T) {
	db, aliceRaws := seedSessionDB(t)
	hash := auth.HashSessionID(aliceRaws[0])

	// Make SessionOwnerByHash fail (its query names the users table) while
	// the delete still works: rename users out of the way. SQLite >= 3.25
	// rewrites the sessions FK to point at the renamed table, so the
	// sessions-only DELETE keeps working — a plain DROP would not (the FK
	// parent must exist for any sessions DML under foreign_keys=ON).
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(`ALTER TABLE users RENAME TO users_gone`); err != nil {
		raw.Close()
		t.Fatalf("rename users: %v", err)
	}
	raw.Close()

	var out, errb bytes.Buffer
	code := runSession(context.Background(), []string{"revoke", "--auth-db", db, "--id-hash", hash}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d (revoke must proceed despite lookup failure); stderr: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "revoked=1") {
		t.Fatalf("stdout %q, want revoked=1", out.String())
	}
	if !strings.Contains(errb.String(), "could not resolve session owner") {
		t.Fatalf("missing lookup-failure warning; stderr: %s", errb.String())
	}
	if !strings.Contains(errb.String(), "target_user=(unresolved)") {
		t.Fatalf("missing (unresolved) audit attr; stderr: %s", errb.String())
	}
}

func TestSessionList_MissingDBErrors(t *testing.T) {
	var out, errb bytes.Buffer
	missing := filepath.Join(t.TempDir(), "nope", "auth.db")
	if code := runSession(context.Background(), []string{"list", "--auth-db", missing}, &out, &errb); code != 1 {
		t.Fatalf("missing db: exit %d, want 1 (must not create an empty db)", code)
	}
	if _, err := os.Stat(missing); err == nil {
		t.Fatal("missing db path was created by a read command")
	}
	if code := runSession(context.Background(), []string{"revoke", "--auth-db", missing, "--user", "alice"}, &out, &errb); code != 1 {
		t.Fatalf("missing db revoke: exit %d, want 1", code)
	}
}
