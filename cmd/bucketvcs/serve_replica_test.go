package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

// replicaArgs returns a serve arg set with --replica-of pointing at a second
// localfs store and a migrated sqlite authdb (which replica mode refuses —
// most tests here exercise validation exits).
func replicaArgs(t *testing.T, extra ...string) []string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "auth.db")
	s, err := sqlitestore.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	args := []string{"serve",
		"--addr", "127.0.0.1:0",
		"--store", "localfs:" + t.TempDir(),
		"--replica-of", "localfs:" + t.TempDir(),
		"--auth-db", dbPath,
		"--lfs=false",
	}
	return append(args, extra...)
}

func TestServeReplicaRefusesSqliteAuthDB(t *testing.T) {
	var out, errb bytes.Buffer
	code := run(context.Background(), replicaArgs(t), &out, &errb)
	if code != 2 {
		t.Fatalf("exit=%d, want 2 (sqlite authdb on replica)\nstderr:\n%s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "postgres") {
		t.Errorf("error should name the postgres requirement:\n%s", errb.String())
	}
}

func TestServeReplicaRefusesExplicitUI(t *testing.T) {
	var out, errb bytes.Buffer
	code := run(context.Background(), replicaArgs(t, "--ui=true"), &out, &errb)
	if code != 2 {
		t.Fatalf("exit=%d, want 2 (explicit --ui on replica)\nstderr:\n%s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "web UI") {
		t.Errorf("error should mention web UI:\n%s", errb.String())
	}
}

func TestServeReplicaRefusesOIDC(t *testing.T) {
	var out, errb bytes.Buffer
	code := run(context.Background(), replicaArgs(t, "--oidc=true"), &out, &errb)
	if code != 2 {
		t.Fatalf("exit=%d, want 2 (--oidc on replica)\nstderr:\n%s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "--oidc is not available on replicas") {
		t.Errorf("error should explain the oidc refusal:\n%s", errb.String())
	}
}

func TestServeReplicaBadMode(t *testing.T) {
	var out, errb bytes.Buffer
	code := run(context.Background(), replicaArgs(t, "--replica-mode=eventual"), &out, &errb)
	if code != 2 {
		t.Fatalf("exit=%d, want 2 (bad mode)\nstderr:\n%s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "strong-current|bounded-stale") {
		t.Errorf("error should list the valid modes:\n%s", errb.String())
	}
}

func TestServeReplicaBudgetFloor(t *testing.T) {
	var out, errb bytes.Buffer
	code := run(context.Background(), replicaArgs(t, "--replica-lag-budget=5s"), &out, &errb)
	if code != 2 {
		t.Fatalf("exit=%d, want 2 (budget < 30s)\nstderr:\n%s", code, errb.String())
	}
}

func TestServeNonReplicaUnaffected(t *testing.T) {
	// Sanity: a plain serve invocation with a bad flag value for a replica
	// flag is still a flag-parse error, and the replica flags' presence
	// does not change non-replica startup validation. --lfs=false so we
	// reach the "--store is required" gate (the LFS gate, which defaults on,
	// fires earlier and is unrelated to replica wiring).
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"serve", "--addr", "127.0.0.1:0", "--lfs=false"}, &out, &errb)
	if code != 2 || !strings.Contains(errb.String(), "--store is required") {
		t.Fatalf("non-replica validation changed: exit=%d stderr=%s", code, errb.String())
	}
}
