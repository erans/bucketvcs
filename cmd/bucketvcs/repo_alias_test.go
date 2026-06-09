package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestRepoAlias_ListAndRemove exercises the full alias CLI round-trip:
// seed an alias via register+rename, list it, remove it, confirm it's gone.
func TestRepoAlias_ListAndRemove(t *testing.T) {
	tmp := t.TempDir()
	authDB := filepath.Join(tmp, "auth.db")
	storeDir := filepath.Join(tmp, "store")
	storeURL := "localfs:" + storeDir
	ctx := context.Background()

	var stdout, stderr bytes.Buffer

	// Step 1: register acme/a (--no-init skips storage init).
	rc := runRepo(ctx, []string{
		"register",
		"--auth-db=" + authDB,
		"--no-init",
		"acme/a",
	}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("register acme/a: rc=%d stderr=%s", rc, stderr.String())
	}

	// Step 2: init the store so localfs layout exists before rename's
	// collision probe (List on a fresh dir should return empty, but localfs
	// requires the bucket dir to exist).
	stdout.Reset()
	stderr.Reset()
	rc = run(ctx, []string{"init", "--store=" + storeURL, "acme", "a"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("init acme/a: rc=%d stderr=%s", rc, stderr.String())
	}

	// Step 3: rename acme/a → b, which creates alias a→b in the authdb.
	stdout.Reset()
	stderr.Reset()
	rc = runRepo(ctx, []string{
		"rename",
		"--auth-db=" + authDB,
		"--store=" + storeURL,
		"acme/a",
		"b",
	}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rename acme/a->b: rc=%d stderr=%s", rc, stderr.String())
	}

	// Step 4: alias list for acme/b → should contain alias "a".
	stdout.Reset()
	stderr.Reset()
	rc = runRepo(ctx, []string{
		"alias", "list",
		"--auth-db=" + authDB,
		"--format=json",
		"acme/b",
	}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("alias list: rc=%d stderr=%s stdout=%s", rc, stderr.String(), stdout.String())
	}
	listOut := stdout.String()
	if !strings.Contains(listOut, `"alias":"a"`) {
		t.Errorf("alias list missing alias=a: %s", listOut)
	}
	if !strings.Contains(listOut, `"target":"b"`) {
		t.Errorf("alias list missing target=b: %s", listOut)
	}

	// Step 5: alias remove acme/a.
	stdout.Reset()
	stderr.Reset()
	rc = runRepo(ctx, []string{
		"alias", "remove",
		"--auth-db=" + authDB,
		"acme/a",
	}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("alias remove: rc=%d stderr=%s", rc, stderr.String())
	}

	// Step 6: alias list acme/b → should be empty (JSON emits nothing).
	stdout.Reset()
	stderr.Reset()
	rc = runRepo(ctx, []string{
		"alias", "list",
		"--auth-db=" + authDB,
		"--format=json",
		"acme/b",
	}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("alias list after remove: rc=%d stderr=%s", rc, stderr.String())
	}
	if strings.Contains(stdout.String(), `"alias":"a"`) {
		t.Errorf("alias list after remove still contains alias=a: %s", stdout.String())
	}
}
