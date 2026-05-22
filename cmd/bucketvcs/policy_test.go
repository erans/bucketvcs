package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

// tempAuthDBWithRepo returns a fresh authdb path with a (tenant, repo)
// row pre-seeded so the protected_refs FK is satisfiable.
func tempAuthDBWithRepo(t *testing.T, tenant, repo string) string {
	t.Helper()
	authDB := filepath.Join(t.TempDir(), "auth.db")
	store, err := sqlitestore.Open(authDB)
	if err != nil {
		t.Fatalf("sqlitestore.Open: %v", err)
	}
	defer store.Close()
	if _, err := store.DB().Exec(
		`INSERT INTO repos (tenant, name, public_read, created_at)
		 VALUES (?, ?, 0, strftime('%s','now'))`,
		tenant, repo,
	); err != nil {
		t.Fatalf("seed repos: %v", err)
	}
	return authDB
}

func TestPolicy_CLI_HelpExitsZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runPolicy(context.Background(), []string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("--help exit=%d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "refs") {
		t.Errorf("--help missing 'refs' subcommand; got: %s", stdout.String())
	}
}

func TestPolicy_CLI_UnknownSubcommandIsUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runPolicy(context.Background(), []string{"bogus"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("unknown subcommand exit=%d, want 2", code)
	}
}

func TestPolicy_CLI_RefsAddListRemove_Roundtrip(t *testing.T) {
	authDB := tempAuthDBWithRepo(t, "acme", "site")

	// add: default flags → both toggles ON
	var s1, e1 bytes.Buffer
	if code := runPolicy(context.Background(),
		[]string{"refs", "add",
			"--auth-db", authDB,
			"--tenant=acme", "--repo=site",
			"--pattern=refs/heads/main"},
		&s1, &e1); code != 0 {
		t.Fatalf("add: code=%d stderr=%s", code, e1.String())
	}

	// list: text format includes pattern and both toggles
	var s2, e2 bytes.Buffer
	if code := runPolicy(context.Background(),
		[]string{"refs", "list",
			"--auth-db", authDB,
			"--tenant=acme", "--repo=site"},
		&s2, &e2); code != 0 {
		t.Fatalf("list: code=%d stderr=%s", code, e2.String())
	}
	out := s2.String()
	for _, want := range []string{
		"pattern=refs/heads/main",
		"block_deletion=true",
		"block_force_push=true",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("list missing %q in %s", want, out)
		}
	}

	// remove
	var s3, e3 bytes.Buffer
	if code := runPolicy(context.Background(),
		[]string{"refs", "remove",
			"--auth-db", authDB,
			"--tenant=acme", "--repo=site",
			"--pattern=refs/heads/main"},
		&s3, &e3); code != 0 {
		t.Fatalf("remove: code=%d stderr=%s", code, e3.String())
	}

	// list now empty
	var s4, e4 bytes.Buffer
	_ = runPolicy(context.Background(),
		[]string{"refs", "list",
			"--auth-db", authDB,
			"--tenant=acme", "--repo=site"},
		&s4, &e4)
	if !strings.Contains(s4.String(), "no protected refs") {
		t.Errorf("list after remove missing 'no protected refs' hint; got: %s", s4.String())
	}
}

func TestPolicy_CLI_AddAllowFlagsLooseProtection(t *testing.T) {
	authDB := tempAuthDBWithRepo(t, "acme", "site")
	var stdout, stderr bytes.Buffer
	if code := runPolicy(context.Background(),
		[]string{"refs", "add",
			"--auth-db", authDB,
			"--tenant=acme", "--repo=site",
			"--pattern=refs/heads/lab/*",
			"--allow-deletion", "--allow-force-push"},
		&stdout, &stderr); code != 0 {
		t.Fatalf("add: code=%d stderr=%s", code, stderr.String())
	}
	var lstdout, lstderr bytes.Buffer
	_ = runPolicy(context.Background(),
		[]string{"refs", "list",
			"--auth-db", authDB,
			"--tenant=acme", "--repo=site"},
		&lstdout, &lstderr)
	out := lstdout.String()
	for _, want := range []string{
		"block_deletion=false",
		"block_force_push=false",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("list missing %q; got: %s", want, out)
		}
	}
}

func TestPolicy_CLI_AddRejectsMalformedPattern(t *testing.T) {
	authDB := tempAuthDBWithRepo(t, "acme", "site")
	var stdout, stderr bytes.Buffer
	code := runPolicy(context.Background(),
		[]string{"refs", "add",
			"--auth-db", authDB,
			"--tenant=acme", "--repo=site",
			"--pattern=refs/heads/[broken"},
		&stdout, &stderr)
	if code != 2 {
		t.Fatalf("add malformed: code=%d, want 2; stderr=%s", code, stderr.String())
	}
}

func TestPolicy_CLI_ListJSONFormat(t *testing.T) {
	authDB := tempAuthDBWithRepo(t, "acme", "site")
	_ = runPolicy(context.Background(),
		[]string{"refs", "add",
			"--auth-db", authDB,
			"--tenant=acme", "--repo=site",
			"--pattern=refs/heads/main"},
		&bytes.Buffer{}, &bytes.Buffer{})

	var stdout, stderr bytes.Buffer
	if code := runPolicy(context.Background(),
		[]string{"refs", "list",
			"--auth-db", authDB,
			"--tenant=acme", "--repo=site",
			"--format=json"},
		&stdout, &stderr); code != 0 {
		t.Fatalf("list json: code=%d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		`"tenant":"acme"`,
		`"repo":"site"`,
		`"pattern":"refs/heads/main"`,
		`"block_deletion":true`,
		`"block_force_push":true`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("json missing %q in: %s", want, out)
		}
	}
}
