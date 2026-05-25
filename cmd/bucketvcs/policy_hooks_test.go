package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

// tempAuthDBWithRepoHooks creates a fresh authdb file and seeds a (tenant, repo)
// row so the hooks-table FK is satisfied. Separate helper to avoid coupling
// with tempAuthDBWithRepo defined in policy_test.go (same package).
func tempAuthDBWithRepoHooks(t *testing.T, tenant, repo string) string {
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

func TestPolicyHooks_Help_ExitsZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runPolicyHooks([]string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("--help: exit %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Usage:") {
		t.Errorf("--help stdout missing 'Usage:': %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "pre-receive") {
		t.Errorf("--help missing trigger names: %s", stdout.String())
	}
}

func TestPolicyHooks_UnknownAction_UsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runPolicyHooks([]string{"bogus"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("unknown action exit=%d, want 2", code)
	}
}

func TestPolicyHooks_NoArgs_UsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runPolicyHooks(nil, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("no args exit=%d, want 2", code)
	}
}

func TestPolicyHooksAdd_MissingRequiredFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runPolicyHooks([]string{"add"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("missing flags exit=%d, want 2; stderr=%s", code, stderr.String())
	}
}

func TestPolicyHooksAdd_RejectsBadTrigger(t *testing.T) {
	authDB := tempAuthDBWithRepoHooks(t, "acme", "site")
	var stdout, stderr bytes.Buffer
	code := runPolicyHooks([]string{
		"add",
		"--auth-db", authDB,
		"--tenant=acme", "--repo=site",
		"--trigger=garbage", "--script=ok.sh",
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("bad trigger exit=%d, want 2; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--trigger") {
		t.Errorf("stderr missing --trigger explanation: %s", stderr.String())
	}
}

func TestPolicyHooksAdd_RejectsBadScriptName(t *testing.T) {
	authDB := tempAuthDBWithRepoHooks(t, "acme", "site")
	var stdout, stderr bytes.Buffer
	code := runPolicyHooks([]string{
		"add",
		"--auth-db", authDB,
		"--tenant=acme", "--repo=site",
		"--trigger=pre-receive", "--script=../etc/passwd",
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("bad script exit=%d, want 2; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--script") {
		t.Errorf("stderr missing --script explanation: %s", stderr.String())
	}
}

// TestPolicyHooks_AddListRemove_Roundtrip exercises the full CRUD cycle
// through the CLI surface. Mirrors TestPolicy_CLI_RefsAddListRemove_Roundtrip.
func TestPolicyHooks_AddListRemove_Roundtrip(t *testing.T) {
	authDB := tempAuthDBWithRepoHooks(t, "acme", "site")

	// add: trigger=pre-receive, script=lint.sh, order=10
	var s1, e1 bytes.Buffer
	if code := runPolicyHooks([]string{
		"add",
		"--auth-db", authDB,
		"--tenant=acme", "--repo=site",
		"--trigger=pre-receive", "--script=lint.sh",
		"--order=10",
	}, &s1, &e1); code != 0 {
		t.Fatalf("add: code=%d stderr=%s", code, e1.String())
	}
	if !strings.Contains(s1.String(), "added: acme/site pre-receive lint.sh") {
		t.Errorf("add stdout unexpected: %s", s1.String())
	}

	// list (NDJSON): one row, with the expected fields
	var s2, e2 bytes.Buffer
	if code := runPolicyHooks([]string{
		"list",
		"--auth-db", authDB,
		"--tenant=acme", "--repo=site",
	}, &s2, &e2); code != 0 {
		t.Fatalf("list: code=%d stderr=%s", code, e2.String())
	}
	lines := strings.Split(strings.TrimRight(s2.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("list: expected 1 NDJSON line, got %d: %s", len(lines), s2.String())
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &row); err != nil {
		t.Fatalf("list: bad JSON %q: %v", lines[0], err)
	}
	if row["script_name"] != "lint.sh" || row["trigger"] != "pre-receive" ||
		row["enabled"] != true {
		t.Errorf("list row fields wrong: %+v", row)
	}
	if so, _ := row["sort_order"].(float64); int(so) != 10 {
		t.Errorf("list row sort_order=%v, want 10", row["sort_order"])
	}

	// list with --trigger=post-receive returns nothing
	var s3, e3 bytes.Buffer
	if code := runPolicyHooks([]string{
		"list",
		"--auth-db", authDB,
		"--tenant=acme", "--repo=site",
		"--trigger=post-receive",
	}, &s3, &e3); code != 0 {
		t.Fatalf("list filtered: code=%d stderr=%s", code, e3.String())
	}
	if s3.String() != "" {
		t.Errorf("list with no matches should emit nothing, got: %s", s3.String())
	}

	// disable
	var s4, e4 bytes.Buffer
	if code := runPolicyHooks([]string{
		"disable",
		"--auth-db", authDB,
		"--tenant=acme", "--repo=site",
		"--trigger=pre-receive", "--script=lint.sh",
	}, &s4, &e4); code != 0 {
		t.Fatalf("disable: code=%d stderr=%s", code, e4.String())
	}

	// list shows enabled=false
	var s5, e5 bytes.Buffer
	_ = runPolicyHooks([]string{
		"list",
		"--auth-db", authDB,
		"--tenant=acme", "--repo=site",
	}, &s5, &e5)
	if !strings.Contains(s5.String(), `"enabled":false`) {
		t.Errorf("list after disable should show enabled=false; got %s", s5.String())
	}

	// enable
	var s6, e6 bytes.Buffer
	if code := runPolicyHooks([]string{
		"enable",
		"--auth-db", authDB,
		"--tenant=acme", "--repo=site",
		"--trigger=pre-receive", "--script=lint.sh",
	}, &s6, &e6); code != 0 {
		t.Fatalf("enable: code=%d stderr=%s", code, e6.String())
	}

	// remove
	var s7, e7 bytes.Buffer
	if code := runPolicyHooks([]string{
		"remove",
		"--auth-db", authDB,
		"--tenant=acme", "--repo=site",
		"--trigger=pre-receive", "--script=lint.sh",
	}, &s7, &e7); code != 0 {
		t.Fatalf("remove: code=%d stderr=%s", code, e7.String())
	}

	// list now empty
	var s8, e8 bytes.Buffer
	_ = runPolicyHooks([]string{
		"list",
		"--auth-db", authDB,
		"--tenant=acme", "--repo=site",
	}, &s8, &e8)
	if s8.String() != "" {
		t.Errorf("list after remove should be empty, got: %s", s8.String())
	}
}

func TestPolicyHooks_Remove_NotFound_ReturnsOne(t *testing.T) {
	authDB := tempAuthDBWithRepoHooks(t, "acme", "site")
	var stdout, stderr bytes.Buffer
	code := runPolicyHooks([]string{
		"remove",
		"--auth-db", authDB,
		"--tenant=acme", "--repo=site",
		"--trigger=pre-receive", "--script=missing.sh",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("remove missing exit=%d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "not found") {
		t.Errorf("stderr should mention 'not found': %s", stderr.String())
	}
}

func TestPolicyHooks_Enable_NotFound_ReturnsOne(t *testing.T) {
	authDB := tempAuthDBWithRepoHooks(t, "acme", "site")
	var stdout, stderr bytes.Buffer
	code := runPolicyHooks([]string{
		"enable",
		"--auth-db", authDB,
		"--tenant=acme", "--repo=site",
		"--trigger=pre-receive", "--script=missing.sh",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("enable missing exit=%d, want 1; stderr=%s", code, stderr.String())
	}
}

// TestPolicyHooks_Add_Idempotent verifies that Add is upsert (the underlying
// store's INSERT...ON CONFLICT DO UPDATE). Re-adding same (tenant, repo,
// trigger, script) with a new --order should succeed and update sort_order.
func TestPolicyHooks_Add_Idempotent(t *testing.T) {
	authDB := tempAuthDBWithRepoHooks(t, "acme", "site")

	for _, ord := range []string{"--order=1", "--order=99"} {
		var stdout, stderr bytes.Buffer
		if code := runPolicyHooks([]string{
			"add",
			"--auth-db", authDB,
			"--tenant=acme", "--repo=site",
			"--trigger=post-receive", "--script=notify.sh",
			ord,
		}, &stdout, &stderr); code != 0 {
			t.Fatalf("add %s: code=%d stderr=%s", ord, code, stderr.String())
		}
	}

	// list shows the final sort_order=99
	var stdout, stderr bytes.Buffer
	_ = runPolicyHooks([]string{
		"list",
		"--auth-db", authDB,
		"--tenant=acme", "--repo=site",
	}, &stdout, &stderr)
	if !strings.Contains(stdout.String(), `"sort_order":99`) {
		t.Errorf("after re-add, sort_order should be 99: %s", stdout.String())
	}
}

// TestPolicy_CLI_HooksReachable verifies the hooks dispatch is reachable
// through the top-level runPolicy entrypoint and policyUsage advertises it.
func TestPolicy_CLI_HooksReachable(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// runPolicy("hooks", "--help") should return 0 with usage text.
	code := runPolicy(nil, []string{"hooks", "--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runPolicy hooks --help exit=%d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Usage:") {
		t.Errorf("hooks --help missing usage: %s", stdout.String())
	}

	// And policy --help mentions 'hooks'.
	var s2, e2 bytes.Buffer
	if code := runPolicy(nil, []string{"--help"}, &s2, &e2); code != 0 {
		t.Fatalf("policy --help exit=%d", code)
	}
	if !strings.Contains(s2.String(), "hooks") {
		t.Errorf("policy --help should list 'hooks' object: %s", s2.String())
	}
}
