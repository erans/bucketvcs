package authreplica

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
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

// recordingHandler captures emitted log messages for assertion.
type recordingHandler struct {
	mu     sync.Mutex
	events []string
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, r.Message)
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

// TestRunner_RestoredEventOnlyOnActualRestore asserts the authdb.replica.restored
// audit event fires only when a missing local DB is actually materialized from
// the replica — never on an empty-bucket first boot and never when the file
// already exists (clean restart).
func TestRunner_RestoredEventOnlyOnActualRestore(t *testing.T) {
	ctx := context.Background()
	store := newLocalFS(t)
	dbPath := filepath.Join(t.TempDir(), "a.db")
	h := &recordingHandler{}
	cfg := Config{DBPath: dbPath, Store: store, Prefix: DefaultPrefix, LeaseTTL: time.Minute, Logger: slog.New(h)}

	// First boot: file missing, bucket empty → EnsureExists no-ops → NO event.
	r, err := Prepare(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	for _, e := range h.events {
		if e == "authdb.replica.restored" {
			t.Fatal("restored event emitted on empty-bucket first boot")
		}
	}

	// Seed: create db, replicate, close, delete local file.
	db := openSQL(t, dbPath)
	if _, err := db.ExecContext(ctx, `CREATE TABLE t (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	r2, err := Prepare(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := r2.StartReplication(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r2.SyncNow(ctx); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := r2.Close(ctx); err != nil {
		t.Fatal(err)
	}
	// Existing-file boot must NOT have emitted restored either.
	for _, e := range h.events {
		if e == "authdb.replica.restored" {
			t.Fatal("restored event emitted when file already existed")
		}
	}
	matches, _ := filepath.Glob(dbPath + "*")
	for _, m := range matches {
		os.RemoveAll(m)
	}

	// Real restore boot → event MUST fire exactly once.
	r3, err := Prepare(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r3.Close(ctx)
	var n int
	for _, e := range h.events {
		if e == "authdb.replica.restored" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want exactly 1 restored event, got %d", n)
	}
}
