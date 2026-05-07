package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
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
