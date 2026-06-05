package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

// doctorEnv creates a healthy localfs store dir + migrated auth.db.
func doctorEnv(t *testing.T) (storeDir, dbPath string) {
	t.Helper()
	storeDir = t.TempDir()
	dbPath = filepath.Join(t.TempDir(), "auth.db")
	s, err := sqlitestore.Open(dbPath)
	if err != nil {
		t.Fatalf("seed authdb: %v", err)
	}
	s.Close()
	return storeDir, dbPath
}

func TestDoctorHealthy(t *testing.T) {
	storeDir, dbPath := doctorEnv(t)
	var out, errb bytes.Buffer
	code := run(context.Background(),
		[]string{"doctor", "--store", "localfs:" + storeDir, "--auth-db", dbPath, "--lfs=false"},
		&out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d, want 0\nstdout:\n%s\nstderr:\n%s", code, out.String(), errb.String())
	}
	for _, want := range []string{"storage.reachable", "storage.writable", "authdb.open", "authdb.migrations", "deps.git"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing check %q:\n%s", want, out.String())
		}
	}
}

func TestDoctorFailsOnLFSWithoutKey(t *testing.T) {
	storeDir, dbPath := doctorEnv(t)
	var out, errb bytes.Buffer
	code := run(context.Background(),
		[]string{"doctor", "--store", "localfs:" + storeDir, "--auth-db", dbPath, "--lfs=true"},
		&out, &errb)
	if code != 1 {
		t.Fatalf("exit=%d, want 1\nstdout:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "config.lfs") || !strings.Contains(out.String(), "FAIL") {
		t.Errorf("expected FAIL on config.lfs:\n%s", out.String())
	}
}

func TestDoctorFailsOnStaleMigrations(t *testing.T) {
	storeDir, dbPath := doctorEnv(t)
	s, err := sqlitestore.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(context.Background(),
		`DELETE FROM schema_version WHERE version=?`, sqlitestore.LatestMigrationVersion()); err != nil {
		t.Fatalf("simulate stale: %v", err)
	}
	s.Close()

	var out, errb bytes.Buffer
	code := run(context.Background(),
		[]string{"doctor", "--store", "localfs:" + storeDir, "--auth-db", dbPath, "--lfs=false"},
		&out, &errb)
	if code != 1 {
		t.Fatalf("exit=%d, want 1\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "authdb.migrations") {
		t.Errorf("expected authdb.migrations failure:\n%s", out.String())
	}
}

func TestDoctorMissingAuthDBDoesNotCreateIt(t *testing.T) {
	storeDir, _ := doctorEnv(t)
	missing := filepath.Join(t.TempDir(), "nope", "auth.db")
	var out, errb bytes.Buffer
	code := run(context.Background(),
		[]string{"doctor", "--store", "localfs:" + storeDir, "--auth-db", missing, "--lfs=false"},
		&out, &errb)
	if code != 1 {
		t.Fatalf("exit=%d, want 1\n%s", code, out.String())
	}
	if _, err := os.Stat(missing); err == nil {
		t.Fatal("doctor created the missing auth.db — it must be read-only")
	}
}

func TestDoctorJSON(t *testing.T) {
	storeDir, dbPath := doctorEnv(t)
	var out, errb bytes.Buffer
	code := run(context.Background(),
		[]string{"doctor", "--json", "--store", "localfs:" + storeDir, "--auth-db", dbPath, "--lfs=false"},
		&out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d\n%s\n%s", code, out.String(), errb.String())
	}
	for i, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("line %d not JSON: %v\n%s", i+1, err, line)
		}
	}
}

func TestDoctorReplicaChecks(t *testing.T) {
	storeDir, dbPath := doctorEnv(t)
	canonicalDir := t.TempDir()
	var out, errb bytes.Buffer
	code := run(context.Background(),
		[]string{"doctor", "--store", "localfs:" + storeDir, "--auth-db", dbPath,
			"--lfs=false", "--replica-of", "localfs:" + canonicalDir},
		&out, &errb)
	// sqlite authdb → config.replica FAILs (replicas need postgres) → exit 1.
	if code != 1 {
		t.Fatalf("exit=%d, want 1\n%s", code, out.String())
	}
	for _, want := range []string{"replica.canonical", "config.replica"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
	// storage.writable must SKIP (no probe write) on replicas.
	if !strings.Contains(out.String(), "read-only replica") {
		t.Errorf("storage.writable should skip on replicas:\n%s", out.String())
	}
}

func TestDoctorReplicaBadMode(t *testing.T) {
	storeDir, dbPath := doctorEnv(t)
	var out, errb bytes.Buffer
	code := run(context.Background(),
		[]string{"doctor", "--store", "localfs:" + storeDir, "--auth-db", dbPath,
			"--lfs=false", "--replica-of", "localfs:" + t.TempDir(), "--replica-mode", "eventual"},
		&out, &errb)
	if code != 1 || !strings.Contains(out.String(), "config.replica") {
		t.Fatalf("bad mode must FAIL config.replica: exit=%d\n%s", code, out.String())
	}
}

func TestDoctorNonReplicaHasNoReplicaChecks(t *testing.T) {
	storeDir, dbPath := doctorEnv(t)
	var out, errb bytes.Buffer
	code := run(context.Background(),
		[]string{"doctor", "--store", "localfs:" + storeDir, "--auth-db", dbPath, "--lfs=false"},
		&out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d, want 0\n%s", code, out.String())
	}
	if strings.Contains(out.String(), "replica.") || strings.Contains(out.String(), "config.replica") {
		t.Errorf("non-replica doctor must not run replica checks:\n%s", out.String())
	}
}
