package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestOIDCCLI_IssuerAndRuleRoundTrip(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "auth.db")
	ctx := context.Background()

	// Seed a repo via the existing repo CLI so the rule FK is satisfiable.
	var rout, rerr bytes.Buffer
	if rc := runRepo(ctx, []string{"register", "org/app", "--no-init", "--auth-db", db}, &rout, &rerr); rc != 0 {
		t.Fatalf("repo register: rc=%d err=%s", rc, rerr.String())
	}

	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := runOIDC(ctx, args, &out, &errb)
		return code, out.String(), errb.String()
	}

	if code, _, e := run("issuer", "add", "--auth-db", db, "--alias", "gh", "--url", "https://i.example"); code != 0 {
		t.Fatalf("issuer add: code=%d err=%s", code, e)
	}
	code, out, _ := run("issuer", "list", "--auth-db", db, "--format", "json")
	if code != 0 || !strings.Contains(out, `"alias":"gh"`) {
		t.Fatalf("issuer list: code=%d out=%s", code, out)
	}
	if code, _, e := run("rule", "add", "--auth-db", db, "--issuer", "gh",
		"--audience", "aud", "--tenant", "org", "--repo", "app",
		"--scopes", "repo:write", "--ttl", "15m", "--claim", "repository=org/app"); code != 0 {
		t.Fatalf("rule add: code=%d err=%s", code, e)
	}
	code, out, _ = run("rule", "list", "--auth-db", db, "--issuer", "gh")
	if code != 0 || !strings.Contains(out, "repo=app") {
		t.Fatalf("rule list: code=%d out=%s", code, out)
	}
	// bad ttl rejected
	if code, _, _ := run("rule", "add", "--auth-db", db, "--issuer", "gh",
		"--audience", "aud", "--tenant", "org", "--repo", "app",
		"--scopes", "repo:write", "--ttl", "5h"); code != 2 {
		t.Fatalf("over-ceiling ttl: want exit 2, got %d", code)
	}
}
