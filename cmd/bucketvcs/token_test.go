package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

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
	full := strings.TrimSpace(stdout.String())
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
