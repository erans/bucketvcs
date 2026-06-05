package authreplica

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// openSQL opens the test sqlite DB the same way sqlitestore does (WAL + busy timeout).
func openSQL(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	return db
}

// TestRunner_ReplicateDeleteRestore is the load-bearing integration test:
// real litestream, real LTX files, full replicate → wipe → restore cycle.
func TestRunner_ReplicateDeleteRestore(t *testing.T) {
	ctx := context.Background()
	store := newLocalFS(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "auth.db")

	cfg := Config{DBPath: dbPath, Store: store, Prefix: DefaultPrefix, LeaseTTL: time.Minute}

	// Phase 1: prepare (no backup yet → restore is a no-op), open DB, replicate.
	r, err := Prepare(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	db := openSQL(t, dbPath)
	if _, err := db.ExecContext(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO t (v) VALUES ('alpha'), ('beta')`); err != nil {
		t.Fatal(err)
	}
	if err := r.StartReplication(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.SyncNow(ctx); err != nil { // deterministic flush to the store
		t.Fatal(err)
	}
	db.Close()
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// Simulate total disk loss: remove the DB and every litestream artifact.
	matches, _ := filepath.Glob(dbPath + "*")
	for _, m := range matches {
		if err := os.RemoveAll(m); err != nil {
			t.Fatal(err)
		}
	}

	// Phase 2: a fresh boot restores from the bucket.
	r2, err := Prepare(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close(ctx)
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("restore did not recreate db: %v", err)
	}
	db2 := openSQL(t, dbPath)
	defer db2.Close()
	var n int
	if err := db2.QueryRowContext(ctx, `SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("want 2 rows after restore, got %d", n)
	}
}

func TestRunner_PrepareFailsWhenLeaseHeld(t *testing.T) {
	ctx := context.Background()
	store := newLocalFS(t)
	dir := t.TempDir()
	cfg := Config{DBPath: filepath.Join(dir, "a.db"), Store: store, Prefix: DefaultPrefix, LeaseTTL: time.Minute}

	r1, err := Prepare(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r1.Close(ctx)

	cfg2 := cfg
	cfg2.DBPath = filepath.Join(dir, "b.db")
	if _, err := Prepare(ctx, cfg2); err == nil {
		t.Fatal("second Prepare should fail while lease held")
	}
}

func TestRunner_SkipRestore(t *testing.T) {
	ctx := context.Background()
	store := newLocalFS(t)
	dbPath := filepath.Join(t.TempDir(), "a.db")
	cfg := Config{DBPath: dbPath, Store: store, Prefix: DefaultPrefix, LeaseTTL: time.Minute, SkipRestore: true}
	r, err := Prepare(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(ctx)
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatal("SkipRestore must not create/restore the db")
	}
}
