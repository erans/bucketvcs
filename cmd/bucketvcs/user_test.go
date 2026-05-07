package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// userCmdEnv stands up a tmp HOME so resolveAuthDB lands somewhere clean.
func userCmdEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("BUCKETVCS_AUTH_DB", "")
	return filepath.Join(home, ".local", "state", "bucketvcs", "bucketvcs.db")
}

func TestUserAdd_AndList(t *testing.T) {
	_ = userCmdEnv(t)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if rc := runUser(context.Background(), []string{"add", "alice"}, stdout, stderr); rc != 0 {
		t.Fatalf("add rc=%d stderr=%s", rc, stderr)
	}
	stdout.Reset()
	if rc := runUser(context.Background(), []string{"list"}, stdout, stderr); rc != 0 {
		t.Fatalf("list rc=%d", rc)
	}
	if !strings.Contains(stdout.String(), "alice") {
		t.Fatalf("list output missing alice: %q", stdout)
	}
}

func TestUserAdd_DuplicateExit2(t *testing.T) {
	_ = userCmdEnv(t)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	_ = runUser(context.Background(), []string{"add", "alice"}, stdout, stderr)
	rc := runUser(context.Background(), []string{"add", "alice"}, stdout, stderr)
	if rc != 2 {
		t.Fatalf("rc = %d, want 2", rc)
	}
}

func TestUserAdmin_Add(t *testing.T) {
	_ = userCmdEnv(t)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if rc := runUser(context.Background(), []string{"add", "root", "--admin"}, stdout, stderr); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	stdout.Reset()
	_ = runUser(context.Background(), []string{"list"}, stdout, stderr)
	if !strings.Contains(stdout.String(), "admin") {
		t.Fatalf("expected admin marker: %q", stdout)
	}
}

func TestUserDisable_AndEnable(t *testing.T) {
	_ = userCmdEnv(t)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	_ = runUser(context.Background(), []string{"add", "alice"}, stdout, stderr)
	if rc := runUser(context.Background(), []string{"disable", "alice"}, stdout, stderr); rc != 0 {
		t.Fatalf("disable rc=%d", rc)
	}
	if rc := runUser(context.Background(), []string{"enable", "alice"}, stdout, stderr); rc != 0 {
		t.Fatalf("enable rc=%d", rc)
	}
}

func TestUserDelete_LastAdminRefused(t *testing.T) {
	_ = userCmdEnv(t)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	_ = runUser(context.Background(), []string{"add", "root", "--admin"}, stdout, stderr)
	if rc := runUser(context.Background(), []string{"delete", "root"}, stdout, stderr); rc == 0 {
		t.Fatalf("expected non-zero rc on last-admin delete")
	}
}

func TestReorderFlagsFirst_PreservesTerminator(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		bool map[string]bool
		want []string
	}{
		{
			name: "explicit terminator preserved before dash positional",
			in:   []string{"--admin", "--", "-foo"},
			bool: map[string]bool{"admin": true},
			want: []string{"--admin", "--", "-foo"},
		},
		{
			name: "no terminator when no dash positional",
			in:   []string{"alice", "--admin"},
			bool: map[string]bool{"admin": true},
			want: []string{"--admin", "alice"},
		},
		{
			name: "single dash stays positional",
			in:   []string{"--admin", "-"},
			bool: map[string]bool{"admin": true},
			want: []string{"--admin", "-"},
		},
		{
			name: "terminator preserved with multiple positionals after",
			in:   []string{"--admin", "--", "alice", "-x"},
			bool: map[string]bool{"admin": true},
			want: []string{"--admin", "--", "alice", "-x"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := reorderFlagsFirst(c.in, c.bool)
			if strings.Join(got, " ") != strings.Join(c.want, " ") {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

// TestUserAdd_DashPrefixedNameAfterTerminator verifies that a name
// beginning with `-` (allowed by validName) survives reorderFlagsFirst
// when passed after `--`, rather than being reinterpreted as a flag by
// flag.Parse and rejected.
func TestUserAdd_DashPrefixedNameAfterTerminator(t *testing.T) {
	_ = userCmdEnv(t)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if rc := runUser(context.Background(), []string{"add", "--admin", "--", "-dashy"}, stdout, stderr); rc != 0 {
		t.Fatalf("add rc=%d stderr=%s", rc, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if rc := runUser(context.Background(), []string{"list"}, stdout, stderr); rc != 0 {
		t.Fatalf("list rc=%d", rc)
	}
	if !strings.Contains(stdout.String(), "-dashy") {
		t.Fatalf("expected -dashy in list, got: %s", stdout.String())
	}
}
