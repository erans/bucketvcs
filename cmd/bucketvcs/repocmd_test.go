package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

func TestRepoRegister_GrantPublicList(t *testing.T) {
	_ = userCmdEnv(t)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	_ = runUser(context.Background(), []string{"add", "alice"}, stdout, stderr)
	stdout.Reset()
	if rc := runRepo(context.Background(), []string{"register", "acme/foo", "--no-init"}, stdout, stderr); rc != 0 {
		t.Fatalf("register rc=%d stderr=%s", rc, stderr)
	}
	if rc := runRepo(context.Background(), []string{"grant", "alice", "acme/foo", "write"}, stdout, stderr); rc != 0 {
		t.Fatalf("grant rc=%d stderr=%s", rc, stderr)
	}
	if rc := runRepo(context.Background(), []string{"public", "acme/foo", "on"}, stdout, stderr); rc != 0 {
		t.Fatalf("public rc=%d", rc)
	}
	stdout.Reset()
	if rc := runRepo(context.Background(), []string{"list"}, stdout, stderr); rc != 0 {
		t.Fatalf("list rc=%d", rc)
	}
	if !strings.Contains(stdout.String(), "acme") || !strings.Contains(stdout.String(), "foo") {
		t.Fatalf("list missing repo: %q", stdout)
	}
}

func TestRepoGrant_RefusesUnregistered(t *testing.T) {
	_ = userCmdEnv(t)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	_ = runUser(context.Background(), []string{"add", "alice"}, stdout, stderr)
	if rc := runRepo(context.Background(), []string{"grant", "alice", "ghost/x", "read"}, stdout, stderr); rc == 0 {
		t.Fatalf("expected non-zero rc; stderr=%s", stderr)
	}
}

// TestSplitTenantRepo_RejectsMultiSlash exercises the input-validation
// guard that the M4 ship-gate roborev iteration 3 finding 2 added: the
// gateway route parser only accepts /{tenant}/{repo}.git, so CLI inputs
// that smuggle extra path segments or invalid characters past the parser
// would register an unservable repo. splitTenantRepo now rejects them at
// the CLI boundary.
func TestSplitTenantRepo_RejectsMultiSlash(t *testing.T) {
	cases := []struct {
		in       string
		wantOK   bool
		wantTen  string
		wantRepo string
	}{
		{in: "acme/foo", wantOK: true, wantTen: "acme", wantRepo: "foo"},
		{in: "Acme.Co_1/repo-2", wantOK: true, wantTen: "Acme.Co_1", wantRepo: "repo-2"},
		{in: "acme/foo/bar", wantOK: false},
		{in: "acme", wantOK: false},
		{in: "acme/", wantOK: false},
		{in: "/foo", wantOK: false},
		{in: "", wantOK: false},
		{in: "/", wantOK: false},
		{in: "acme/foo!", wantOK: false},
		{in: "acme!/foo", wantOK: false},
		{in: "acme/foo bar", wantOK: false},
	}
	for _, c := range cases {
		ten, repo, err := splitTenantRepo(c.in)
		if c.wantOK {
			if err != nil {
				t.Errorf("splitTenantRepo(%q) err = %v, want nil", c.in, err)
				continue
			}
			if ten != c.wantTen || repo != c.wantRepo {
				t.Errorf("splitTenantRepo(%q) = (%q, %q), want (%q, %q)",
					c.in, ten, repo, c.wantTen, c.wantRepo)
			}
		} else {
			if err == nil {
				t.Errorf("splitTenantRepo(%q) = (%q, %q), want error", c.in, ten, repo)
			}
		}
	}
}

func TestRepoRegister_ActorFlagOverridesOSUsername(t *testing.T) {
	tmp := t.TempDir()
	authDB := filepath.Join(tmp, "auth.db")

	// Initialize authdb.
	store, err := sqlitestore.Open(authDB)
	if err != nil {
		t.Fatalf("open authdb: %v", err)
	}
	store.Close()

	// Register an endpoint subscribing to repo.created for acme/site.
	store, _ = sqlitestore.Open(authDB)
	svc := webhooks.New(store.DB())
	// Seed the repos row so the endpoint FK is satisfiable.
	if _, err := store.DB().ExecContext(context.Background(),
		`INSERT INTO repos (tenant, name, public_read, created_at)
		 VALUES (?, ?, 0, strftime('%s','now'))`,
		"acme", "site",
	); err != nil {
		t.Fatalf("seed repo row: %v", err)
	}
	if _, err := svc.Create(context.Background(), webhooks.EndpointInput{
		Tenant: "acme", Repo: "site",
		URL: "https://hook.example.com", EventMask: webhooks.EventRepoCreated,
	}); err != nil {
		t.Fatalf("Create endpoint: %v", err)
	}
	// Drop the seeded repo row so re-register-via-CLI actually inserts.
	// We must disable FK enforcement first; otherwise the ON DELETE CASCADE
	// on webhook_endpoints(tenant, repo) -> repos(tenant, name) would
	// cascade-delete the endpoint we just created.
	if _, err := store.DB().ExecContext(context.Background(), `PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatalf("disable fk: %v", err)
	}
	if _, err := store.DB().ExecContext(context.Background(), `DELETE FROM repos WHERE tenant=? AND name=?`, "acme", "site"); err != nil {
		t.Fatalf("delete seed repo: %v", err)
	}
	if _, err := store.DB().ExecContext(context.Background(), `PRAGMA foreign_keys=ON`); err != nil {
		t.Fatalf("re-enable fk: %v", err)
	}
	store.Close()

	// Register via CLI with --actor=ops-cron.
	var stdout, stderr bytes.Buffer
	rc := runRepo(context.Background(), []string{
		"register",
		"--auth-db=" + authDB,
		"--no-init",
		"--actor=ops-cron",
		"acme/site",
	}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("repo register --actor=ops-cron: rc=%d, stderr=%s", rc, stderr.String())
	}

	// Verify the webhook_deliveries row has actor=ops-cron.
	store, _ = sqlitestore.Open(authDB)
	defer store.Close()
	svc = webhooks.New(store.DB())
	rows, err := svc.PendingForTest(context.Background())
	if err != nil {
		t.Fatalf("PendingForTest: %v", err)
	}
	var found bool
	for _, r := range rows {
		if r.EventType == "repo.created" && strings.Contains(string(r.PayloadJSON), `"actor":"ops-cron"`) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no webhook delivery with actor=ops-cron; rows=%+v", rows)
	}
}

func TestRepoRegister_EmptyActorFlagRejected(t *testing.T) {
	tmp := t.TempDir()
	authDB := filepath.Join(tmp, "auth.db")
	store, err := sqlitestore.Open(authDB)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	store.Close()

	var stdout, stderr bytes.Buffer
	rc := runRepo(context.Background(), []string{
		"register",
		"--auth-db=" + authDB,
		"--no-init",
		"--actor=",
		"acme/site",
	}, &stdout, &stderr)
	if rc != 2 {
		t.Errorf("--actor= empty: rc=%d, want 2 (usage); stderr=%s", rc, stderr.String())
	}
}

func TestRepoDelete_AuthDBOnly(t *testing.T) {
	tmp := t.TempDir()
	authDB := filepath.Join(tmp, "auth.db")
	store, err := sqlitestore.Open(authDB)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	store.Close()

	var stdout, stderr bytes.Buffer
	rc := runRepo(context.Background(), []string{
		"register", "--auth-db=" + authDB, "--no-init", "acme/site",
	}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("register: rc=%d, stderr=%s", rc, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	rc = runRepo(context.Background(), []string{
		"delete", "--auth-db=" + authDB, "acme/site",
	}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("delete: rc=%d, stderr=%s", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "deleted") {
		t.Errorf("delete output missing 'deleted': %s", stdout.String())
	}

	store, _ = sqlitestore.Open(authDB)
	defer store.Close()
	repos, _ := store.ListRepos(context.Background(), "acme")
	for _, r := range repos {
		if r.Tenant == "acme" && r.Name == "site" {
			t.Errorf("repo row still exists: %+v", r)
		}
	}
}

func TestRepoDelete_NotFound(t *testing.T) {
	tmp := t.TempDir()
	authDB := filepath.Join(tmp, "auth.db")
	store, err := sqlitestore.Open(authDB)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	store.Close()

	var stdout, stderr bytes.Buffer
	rc := runRepo(context.Background(), []string{
		"delete", "--auth-db=" + authDB, "acme/nonexistent",
	}, &stdout, &stderr)
	if rc != 1 {
		t.Errorf("delete non-existent: rc=%d, want 1; stderr=%s", rc, stderr.String())
	}
}

func TestRepoDelete_EmitsRepoDeletedWebhook(t *testing.T) {
	tmp := t.TempDir()
	authDB := filepath.Join(tmp, "auth.db")
	store, err := sqlitestore.Open(authDB)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	store.Close()

	var stdout, stderr bytes.Buffer
	_ = runRepo(context.Background(), []string{
		"register", "--auth-db=" + authDB, "--no-init", "acme/site",
	}, &stdout, &stderr)

	store, _ = sqlitestore.Open(authDB)
	svc := webhooks.New(store.DB())
	if _, err := svc.Create(context.Background(), webhooks.EndpointInput{
		Tenant: "acme", Repo: "site",
		URL:       "https://hook.example.com",
		EventMask: webhooks.EventRepoDeleted,
	}); err != nil {
		t.Fatalf("Create endpoint: %v", err)
	}
	// Drain any pending rows from register so we measure only delete.
	if _, err := store.DB().ExecContext(context.Background(), `DELETE FROM webhook_deliveries`); err != nil {
		t.Fatalf("drain deliveries: %v", err)
	}
	store.Close()

	stdout.Reset()
	stderr.Reset()
	rc := runRepo(context.Background(), []string{
		"delete", "--auth-db=" + authDB,
		"--actor=ops-cron", "acme/site",
	}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("delete: rc=%d, stderr=%s", rc, stderr.String())
	}

	store, _ = sqlitestore.Open(authDB)
	defer store.Close()
	svc = webhooks.New(store.DB())
	rows, _ := svc.PendingForTest(context.Background())
	var found bool
	for _, r := range rows {
		if r.EventType == "repo.deleted" && strings.Contains(string(r.PayloadJSON), `"actor":"ops-cron"`) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no repo.deleted webhook with actor=ops-cron; rows=%+v", rows)
	}
}
