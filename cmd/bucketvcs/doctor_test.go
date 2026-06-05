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
