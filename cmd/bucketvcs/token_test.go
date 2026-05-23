package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// extractTokenLine returns the value after `token=` in tokenCreate/tokenRotate
// output (one line of `key=value`-shaped fields). Returns "" if missing.
func extractTokenLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "token=") {
			return strings.TrimPrefix(line, "token=")
		}
	}
	return ""
}

// extractIDLine returns the value after `id=` (taking just the id token, in
// case the line carries a trailing word like " rotated").
func extractIDLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "id=") {
			tail := strings.TrimPrefix(line, "id=")
			if i := strings.IndexAny(tail, " \t"); i >= 0 {
				return tail[:i]
			}
			return tail
		}
	}
	return ""
}

func TestTokenCreate_PrintsTokenOnce(t *testing.T) {
	_ = userCmdEnv(t)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if rc := runUser(context.Background(), []string{"add", "alice"}, stdout, stderr); rc != 0 {
		t.Fatalf("add: rc=%d stderr=%s", rc, stderr)
	}
	stdout.Reset()
	if rc := runToken(context.Background(), []string{"create", "alice", "--label", "laptop"}, stdout, stderr); rc != 0 {
		t.Fatalf("create: rc=%d stderr=%s", rc, stderr)
	}
	if !strings.Contains(stdout.String(), "bvts_") {
		t.Fatalf("expected bvts_ prefix in stdout, got: %q", stdout)
	}
}

func TestTokenList_AfterCreate(t *testing.T) {
	_ = userCmdEnv(t)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	_ = runUser(context.Background(), []string{"add", "alice"}, stdout, stderr)
	stdout.Reset()
	_ = runToken(context.Background(), []string{"create", "alice", "--label", "laptop"}, stdout, stderr)
	stdout.Reset()
	if rc := runToken(context.Background(), []string{"list", "alice"}, stdout, stderr); rc != 0 {
		t.Fatalf("list rc=%d", rc)
	}
	if !strings.Contains(stdout.String(), "laptop") {
		t.Fatalf("list missing label: %q", stdout)
	}
}

func TestTokenRevoke_ByPrefix(t *testing.T) {
	_ = userCmdEnv(t)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	_ = runUser(context.Background(), []string{"add", "alice"}, stdout, stderr)
	stdout.Reset()
	_ = runToken(context.Background(), []string{"create", "alice"}, stdout, stderr)
	full := extractTokenLine(stdout.String())
	if full == "" {
		t.Fatalf("create output missing token= line: %q", stdout)
	}
	parts := strings.Split(full, "_")
	if len(parts) != 3 {
		t.Fatalf("unexpected token shape: %q", full)
	}
	id := parts[1]
	stdout.Reset()
	if rc := runToken(context.Background(), []string{"revoke", id[:8]}, stdout, stderr); rc != 0 {
		t.Fatalf("revoke rc=%d stderr=%s", rc, stderr)
	}
}

func TestTokenCreate_WithScopes(t *testing.T) {
	_ = userCmdEnv(t)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if rc := runUser(context.Background(), []string{"add", "alice"}, stdout, stderr); rc != 0 {
		t.Fatalf("add: rc=%d stderr=%s", rc, stderr)
	}
	stdout.Reset()
	stderr.Reset()
	if rc := runToken(context.Background(), []string{
		"create", "alice", "--scopes=repo:read,lfs:read",
	}, stdout, stderr); rc != 0 {
		t.Fatalf("create with scopes: rc=%d stderr=%s", rc, stderr)
	}
	if !strings.Contains(stdout.String(), "scopes=repo:read,lfs:read") {
		t.Errorf("output missing scopes=repo:read,lfs:read: %s", stdout.String())
	}
}

func TestTokenCreate_NoScopesWarns(t *testing.T) {
	_ = userCmdEnv(t)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if rc := runUser(context.Background(), []string{"add", "alice"}, stdout, stderr); rc != 0 {
		t.Fatalf("add: rc=%d stderr=%s", rc, stderr)
	}
	stdout.Reset()
	stderr.Reset()
	if rc := runToken(context.Background(), []string{"create", "alice"}, stdout, stderr); rc != 0 {
		t.Fatalf("create no scopes: rc=%d stderr=%s", rc, stderr)
	}
	if !strings.Contains(stderr.String(), "warning:") || !strings.Contains(stderr.String(), "scopes") {
		t.Errorf("expected scopes warning on stderr, got %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "scopes=legacy") {
		t.Errorf("output should show scopes=legacy: %s", stdout.String())
	}
}

func TestTokenCreate_BogusScopes(t *testing.T) {
	_ = userCmdEnv(t)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if rc := runUser(context.Background(), []string{"add", "alice"}, stdout, stderr); rc != 0 {
		t.Fatalf("add: rc=%d stderr=%s", rc, stderr)
	}
	stdout.Reset()
	stderr.Reset()
	rc := runToken(context.Background(), []string{
		"create", "alice", "--scopes=bogus",
	}, stdout, stderr)
	if rc != 2 {
		t.Errorf("bogus scopes: rc=%d, want 2 (stderr=%s)", rc, stderr.String())
	}
}

// TestTokenCreate_ExplicitEmptyScopesRejected covers the round-1 roborev L3
// fix. Per spec §9, --scopes= with an explicitly empty value is a usage
// error (the operator asked for "no scope at all"). It is distinct from
// omitting --scopes entirely, which falls through to legacy mode with a
// stderr warning. fs.Func is what lets the flag library tell the two apart.
func TestTokenCreate_ExplicitEmptyScopesRejected(t *testing.T) {
	_ = userCmdEnv(t)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if rc := runUser(context.Background(), []string{"add", "alice"}, stdout, stderr); rc != 0 {
		t.Fatalf("add: rc=%d stderr=%s", rc, stderr)
	}
	stdout.Reset()
	stderr.Reset()
	rc := runToken(context.Background(), []string{
		"create", "alice", "--scopes=",
	}, stdout, stderr)
	if rc != 2 {
		t.Errorf("--scopes= empty: rc=%d, want 2 (usage error); stderr=%s",
			rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "invalid --scopes") {
		t.Errorf("stderr should mention 'invalid --scopes': %s", stderr.String())
	}
}

func TestTokenList_ShowsScopes(t *testing.T) {
	_ = userCmdEnv(t)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	_ = runUser(context.Background(), []string{"add", "alice"}, stdout, stderr)
	stdout.Reset()
	stderr.Reset()
	if rc := runToken(context.Background(), []string{
		"create", "alice", "--scopes=repo:write",
	}, stdout, stderr); rc != 0 {
		t.Fatalf("create: rc=%d stderr=%s", rc, stderr)
	}
	stdout.Reset()
	stderr.Reset()
	if rc := runToken(context.Background(), []string{"list", "alice"}, stdout, stderr); rc != 0 {
		t.Fatalf("list: rc=%d stderr=%s", rc, stderr)
	}
	if !strings.Contains(stdout.String(), "scopes") {
		t.Errorf("list header should mention 'scopes': %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "repo:write") {
		t.Errorf("list should show repo:write: %s", stdout.String())
	}
}

func TestTokenRotate(t *testing.T) {
	_ = userCmdEnv(t)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	_ = runUser(context.Background(), []string{"add", "alice"}, stdout, stderr)
	stdout.Reset()
	stderr.Reset()
	if rc := runToken(context.Background(), []string{
		"create", "alice", "--scopes=repo:read",
	}, stdout, stderr); rc != 0 {
		t.Fatalf("create: rc=%d stderr=%s", rc, stderr)
	}
	tokenID := extractIDLine(stdout.String())
	if tokenID == "" {
		t.Fatalf("create output missing id= line: %s", stdout.String())
	}
	origToken := extractTokenLine(stdout.String())

	stdout.Reset()
	stderr.Reset()
	if rc := runToken(context.Background(), []string{
		"rotate", "--id", tokenID,
	}, stdout, stderr); rc != 0 {
		t.Fatalf("rotate: rc=%d stderr=%s", rc, stderr)
	}
	out := stdout.String()
	if !strings.Contains(out, "rotated") {
		t.Errorf("rotate output missing 'rotated': %s", out)
	}
	newToken := extractTokenLine(out)
	if newToken == "" {
		t.Errorf("rotate output missing token= line: %s", out)
	}
	if newToken == origToken {
		t.Errorf("rotate did not change token plaintext")
	}
	// The id segment of the new token must equal the old id (only the
	// secret rotates).
	parts := strings.Split(newToken, "_")
	if len(parts) != 3 || parts[1] != tokenID {
		t.Errorf("rotate new token has unexpected id segment: full=%q want id=%q", newToken, tokenID)
	}
}

func TestTokenRotate_NotFound(t *testing.T) {
	_ = userCmdEnv(t)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if rc := runUser(context.Background(), []string{"add", "alice"}, stdout, stderr); rc != 0 {
		t.Fatalf("add: rc=%d stderr=%s", rc, stderr)
	}
	stdout.Reset()
	stderr.Reset()
	// Token ids are 24 Crockford chars; supply a syntactically-plausible
	// id that won't exist in the empty DB.
	rc := runToken(context.Background(), []string{
		"rotate", "--id", "ZZZZZZZZZZZZZZZZZZZZZZZZ",
	}, stdout, stderr)
	if rc != 2 {
		t.Errorf("rotate nonexistent: rc=%d, want 2 (stderr=%s)", rc, stderr.String())
	}
}
