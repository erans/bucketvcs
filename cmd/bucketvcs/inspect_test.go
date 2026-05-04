package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRunInspect_HumanFormat(t *testing.T) {
	dir := t.TempDir()
	var sink bytes.Buffer
	if code := runInit(context.Background(),
		[]string{"--store=localfs:" + dir, "acme", "my-repo"},
		&sink, &sink); code != 0 {
		t.Fatalf("init failed: exit %d, %s", code, sink.String())
	}

	var stdout, stderr bytes.Buffer
	code := runInspect(context.Background(),
		[]string{"--store=localfs:" + dir, "acme", "my-repo"},
		&stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"schema_version", "1",
		"repo_id", "my-repo",
		"manifest_version", "1",
		"latest_tx", "tx_",
		"refs", "0",
		"packs", "0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

func TestRunInspect_JSONFormat(t *testing.T) {
	dir := t.TempDir()
	var sink bytes.Buffer
	runInit(context.Background(),
		[]string{"--store=localfs:" + dir, "acme", "x"}, &sink, &sink)

	var stdout, stderr bytes.Buffer
	code := runInspect(context.Background(),
		[]string{"--store=localfs:" + dir, "--json", "acme", "x"},
		&stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, stderr.String())
	}
	if !strings.HasPrefix(strings.TrimSpace(stdout.String()), "{") {
		t.Errorf("--json output should be a JSON object, got: %s", stdout.String())
	}
}

func TestRunInspect_NotFound(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runInspect(context.Background(),
		[]string{"--store=localfs:" + dir, "acme", "missing"},
		&stdout, &stderr)
	if code != 2 {
		t.Errorf("want exit 2 (not found), got %d", code)
	}
}
