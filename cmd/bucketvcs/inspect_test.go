package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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

func TestRunInspect_UnsupportedSchemaExitCode(t *testing.T) {
	dir := t.TempDir()
	// Init the repo, then directly clobber the on-disk root with a
	// future-schema manifest to exercise the unsupported-schema path.
	var sink bytes.Buffer
	if code := runInit(context.Background(),
		[]string{"--store=localfs:" + dir, "acme", "x"}, &sink, &sink); code != 0 {
		t.Fatalf("init failed: %s", sink.String())
	}

	// The root.json is stored in objects/ directory by localfs.
	rootPath := filepath.Join(dir, "objects/tenants/acme/repos/x/manifest/root.json")
	bad := `{"schema_version":999,"min_reader_version":"0.1.0","repo_id":"x","repo_format":{"object_format":"sha1"},"manifest_version":1,"latest_tx":"tx_a","created_at":"2026-05-03T20:00:00Z","updated_at":"2026-05-03T20:00:00Z","refs":{}}`
	if err := os.WriteFile(rootPath, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	// Clear the localfs sidecar so it doesn't prevent the rewritten manifest
	// from being re-read. The sidecar stores the SHA256 and size of the
	// old content; when we overwrite root.json directly, the sidecar becomes
	// stale and needs to be removed so localfs can recompute it.
	metaPath := rootPath + ".meta"
	if err := os.Remove(metaPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runInspect(context.Background(),
		[]string{"--store=localfs:" + dir, "acme", "x"}, &stdout, &stderr)
	if code != 3 {
		t.Errorf("want exit 3 (unsupported schema), got %d; stderr=%s", code, stderr.String())
	}
}
