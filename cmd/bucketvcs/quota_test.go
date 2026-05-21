package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// tempAuthDB returns a fresh authdb file path inside t.TempDir().
// The file does NOT exist yet — runQuota* opens it and migrates on
// first use via sqlitestore.Open.
func tempAuthDB(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "auth.db")
}

func TestQuota_CLI_HelpExitsZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runQuota(context.Background(), []string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("--help exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "set") || !strings.Contains(stdout.String(), "reconcile") {
		t.Errorf("--help missing subcommand list; got: %s", stdout.String())
	}
}

func TestQuota_CLI_UnknownSubcommandIsUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runQuota(context.Background(), []string{"bogus"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("unknown subcommand exit = %d, want 2; stderr=%s", code, stderr.String())
	}
}

func TestQuota_CLI_SetShowClear_Roundtrip(t *testing.T) {
	authDB := tempAuthDB(t)

	// Set
	var s1, e1 bytes.Buffer
	if code := runQuota(context.Background(),
		[]string{"set", "--auth-db", authDB, "--tenant=acme", "--limit=100GiB"},
		&s1, &e1); code != 0 {
		t.Fatalf("set: code=%d stderr=%s", code, e1.String())
	}

	// Show
	var s2, e2 bytes.Buffer
	if code := runQuota(context.Background(),
		[]string{"show", "--auth-db", authDB, "--tenant=acme"},
		&s2, &e2); code != 0 {
		t.Fatalf("show: code=%d stderr=%s", code, e2.String())
	}
	if !strings.Contains(s2.String(), "tenant=acme") || !strings.Contains(s2.String(), "limit=") {
		t.Errorf("show output missing fields: %s", s2.String())
	}

	// Clear
	var s3, e3 bytes.Buffer
	if code := runQuota(context.Background(),
		[]string{"clear", "--auth-db", authDB, "--tenant=acme"},
		&s3, &e3); code != 0 {
		t.Fatalf("clear: code=%d stderr=%s", code, e3.String())
	}

	// Show after clear → tenant absent
	var s4, e4 bytes.Buffer
	_ = runQuota(context.Background(),
		[]string{"show", "--auth-db", authDB, "--tenant=acme"},
		&s4, &e4)
	if !strings.Contains(s4.String(), "no quota") {
		t.Errorf("show after clear missing 'no quota' hint; got: %s", s4.String())
	}
}

func TestQuota_CLI_SetRejectsMalformedSize(t *testing.T) {
	authDB := tempAuthDB(t)
	var stdout, stderr bytes.Buffer
	code := runQuota(context.Background(),
		[]string{"set", "--auth-db", authDB, "--tenant=acme", "--limit=banana"},
		&stdout, &stderr)
	if code != 2 {
		t.Fatalf("set with bogus size: code=%d, want 2; stderr=%s", code, stderr.String())
	}
}

func TestQuota_CLI_SetRejectsOverflowingSize(t *testing.T) {
	authDB := tempAuthDB(t)
	var stdout, stderr bytes.Buffer
	// "999999999TiB" — n=999999999, mult=2^40, product overflows int64.
	code := runQuota(context.Background(),
		[]string{"set", "--auth-db", authDB, "--tenant=acme", "--limit=999999999TiB"},
		&stdout, &stderr)
	if code != 2 {
		t.Fatalf("set with overflow size: code=%d, want 2; stderr=%s", code, stderr.String())
	}
}
