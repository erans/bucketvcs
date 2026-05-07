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
