package sqlitestore

import (
	"database/sql"
	"net/url"
	"path/filepath"
	"testing"
)

// newTestDB opens a fresh file-backed SQLite database with the same pragmas
// as Open (WAL, foreign_keys, busy_timeout) but WITHOUT running migrations.
// Tests that call RunMigrations directly should use this instead of mustOpen.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "schema_test.db")
	u := &url.URL{
		Scheme: "file",
		Opaque: (&url.URL{Path: path}).EscapedPath(),
	}
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatalf("newTestDB open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func TestRunMigrations_AppliesV2_SSHKeys(t *testing.T) {
	db := newTestDB(t)
	if err := RunMigrations(db); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	var n int
	err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='ssh_keys'`).Scan(&n)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 1 {
		t.Fatalf("ssh_keys table missing")
	}

	var ver int
	if err := db.QueryRow(`SELECT max(version) FROM schema_version`).Scan(&ver); err != nil {
		t.Fatalf("schema_version: %v", err)
	}
	if ver < 2 {
		t.Fatalf("schema_version max = %d, want >= 2", ver)
	}

	// Confirm the unique index exists.
	var ixCount int
	err = db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND name='ssh_keys_fingerprint_idx'`).Scan(&ixCount)
	if err != nil {
		t.Fatalf("index query: %v", err)
	}
	if ixCount != 1 {
		t.Fatalf("ssh_keys_fingerprint_idx missing")
	}
}

func TestRunMigrations_AppliesV3_LFSLocks(t *testing.T) {
	db := newTestDB(t)
	if err := RunMigrations(db); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	// Assert the lfs_locks table exists with the expected columns.
	rows, err := db.Query(`SELECT name FROM pragma_table_info('lfs_locks') ORDER BY cid`)
	if err != nil {
		t.Fatalf("pragma_table_info: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, n)
	}
	want := []string{"id", "tenant", "repo", "path", "ref_name", "owner_user_id", "locked_at"}
	if len(got) != len(want) {
		t.Fatalf("columns=%v want %v", got, want)
	}
	for i, name := range want {
		if got[i] != name {
			t.Errorf("col %d: got %q want %q", i, got[i], name)
		}
	}
}

func TestRunMigrations_SSHKeys_CheckXOR(t *testing.T) {
	// Verify the CHECK constraint actually rejects bad rows.
	db := newTestDB(t)
	if err := RunMigrations(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Insert a user and a repo so foreign keys are satisfiable.
	_, err := db.Exec(`INSERT INTO users (id, name, created_at) VALUES ('u1','alice', strftime('%s','now'))`)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	_, err = db.Exec(`INSERT INTO repos (tenant, name, created_at) VALUES ('acme','web', strftime('%s','now'))`)
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	// Both user_id and scope_* set → must fail.
	_, err = db.Exec(`INSERT INTO ssh_keys (id, fingerprint, public_key, key_type, created_at,
                       user_id, scope_tenant, scope_repo, scope_perm)
                       VALUES ('k1','SHA256:1', X'00', 'ssh-ed25519', strftime('%s','now'),
                       'u1','acme','web','read')`)
	if err == nil {
		t.Fatal("expected CHECK violation: both user_id and scope set")
	}

	// Neither user_id nor scope_* set → must fail.
	_, err = db.Exec(`INSERT INTO ssh_keys (id, fingerprint, public_key, key_type, created_at)
                       VALUES ('k2','SHA256:2', X'00', 'ssh-ed25519', strftime('%s','now'))`)
	if err == nil {
		t.Fatal("expected CHECK violation: neither user_id nor scope set")
	}

	// Only user_id → ok.
	_, err = db.Exec(`INSERT INTO ssh_keys (id, fingerprint, public_key, key_type, created_at, user_id)
                       VALUES ('k3','SHA256:3', X'00', 'ssh-ed25519', strftime('%s','now'),'u1')`)
	if err != nil {
		t.Fatalf("user-key insert failed: %v", err)
	}

	// Only scope_* → ok.
	_, err = db.Exec(`INSERT INTO ssh_keys (id, fingerprint, public_key, key_type, created_at,
                       scope_tenant, scope_repo, scope_perm)
                       VALUES ('k4','SHA256:4', X'00', 'ssh-ed25519', strftime('%s','now'),
                       'acme','web','read')`)
	if err != nil {
		t.Fatalf("deploy-key insert failed: %v", err)
	}
}
