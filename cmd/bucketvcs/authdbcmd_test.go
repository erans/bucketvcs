package main

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/bucketvcs/bucketvcs/internal/authreplica"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// seedReplica replicates a tiny DB into a localfs store and returns its root.
func seedReplica(t *testing.T) (storeRoot, dbPath string) {
	t.Helper()
	ctx := context.Background()
	storeRoot = t.TempDir()
	st, err := localfs.Open(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	dbPath = filepath.Join(t.TempDir(), "auth.db")
	r, err := authreplica.Prepare(ctx, authreplica.Config{
		DBPath: dbPath, Store: st, Prefix: authreplica.DefaultPrefix, LeaseTTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `CREATE TABLE marker (v TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO marker VALUES ('present')`); err != nil {
		t.Fatal(err)
	}
	if err := r.StartReplication(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.SyncNow(ctx); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	// localfs takes an exclusive .lock on its root for the lifetime of the
	// store; release it so the CLI under test can reopen the same root.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return storeRoot, dbPath
}

func TestAuthDBRestore_ToOutput(t *testing.T) {
	storeRoot, _ := seedReplica(t)
	out := filepath.Join(t.TempDir(), "restored.db")

	var stdout, stderr bytes.Buffer
	code := run(context.Background(),
		[]string{"authdb", "restore", "--replica=localfs:" + storeRoot, "--output=" + out},
		&stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	db, err := sql.Open("sqlite", "file:"+out)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v string
	if err := db.QueryRow(`SELECT v FROM marker`).Scan(&v); err != nil || v != "present" {
		t.Fatalf("restored db wrong: v=%q err=%v", v, err)
	}
}

func TestAuthDBRestore_RefusesOverwriteWithoutForce(t *testing.T) {
	storeRoot, _ := seedReplica(t)
	out := filepath.Join(t.TempDir(), "x.db")
	if err := os.WriteFile(out, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(),
		[]string{"authdb", "restore", "--replica=localfs:" + storeRoot, "--output=" + out},
		&stdout, &stderr)
	if code == 0 {
		t.Fatal("expected refusal without --force")
	}
	if !strings.Contains(stderr.String(), "--force") {
		t.Fatalf("error should mention --force: %s", stderr.String())
	}
	// --if-not-exists turns the same situation into a no-op success.
	code = run(context.Background(),
		[]string{"authdb", "restore", "--replica=localfs:" + storeRoot, "--output=" + out, "--if-not-exists"},
		&stdout, &stderr)
	if code != 0 {
		t.Fatalf("--if-not-exists should no-op, got %d: %s", code, stderr.String())
	}
}

func TestAuthDBRestore_ForceRemovesStaleWALSidecars(t *testing.T) {
	storeRoot, _ := seedReplica(t)
	out := filepath.Join(t.TempDir(), "x.db")
	if err := os.WriteFile(out, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out+"-wal", []byte("stale-wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(),
		[]string{"authdb", "restore", "--replica=localfs:" + storeRoot, "--output=" + out, "--force"},
		&stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if _, err := os.Stat(out + "-wal"); !os.IsNotExist(err) {
		t.Fatal("stale -wal sidecar survived --force restore")
	}
	db, err := sql.Open("sqlite", "file:"+out+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v string
	if err := db.QueryRow(`SELECT v FROM marker`).Scan(&v); err != nil || v != "present" {
		t.Fatalf("restored db wrong after force: v=%q err=%v", v, err)
	}
}

func TestAuthDBReplicaStatus(t *testing.T) {
	storeRoot, _ := seedReplica(t)
	var stdout, stderr bytes.Buffer
	code := run(context.Background(),
		[]string{"authdb", "replica-status", "--replica=localfs:" + storeRoot},
		&stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"level":0`) {
		t.Fatalf("status missing level 0 line: %s", stdout.String())
	}
}
