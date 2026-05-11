package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/tx"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
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

// TestRunInspect_JSON_ReachabilityBlock verifies that inspect-manifest --json
// emits:
//   - a "reachability_summary" key with the derived fields (delta_chain_length,
//     delta_chain_bytes, delta_files, base_manifest)
//   - the raw "reachability" body field intact (base_manifest + deltas array)
func TestRunInspect_JSON_ReachabilityBlock(t *testing.T) {
	storeDir := t.TempDir()
	ctx := context.Background()

	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}

	// Create the repo and commit a manifest body with a reachability delta.
	r, err := repo.Create(ctx, store, "t", "r", repo.CreateOptions{Actor: "u_test"})
	if err != nil {
		t.Fatal(err)
	}

	// Build a minimal manifest.Body with a reachability ref.
	deltaKey := "tenants/t/repos/r/indexes/reachability-delta/abcd1234.bvrd"
	body := manifest.Body{
		Refs:  map[string]string{},
		Packs: []manifest.PackEntry{},
		Indexes: manifest.Indexes{
			ObjectMap:   &manifest.IndexRef{Key: "om-key", Hash: "om-hash", SizeBytes: 1024},
			CommitGraph: &manifest.IndexRef{Key: "cg-key", Hash: "cg-hash", SizeBytes: 512},
			Reachability: &manifest.ReachabilityRef{
				BaseManifest: "v00000001",
				Deltas: []manifest.IndexRef{
					{Key: deltaKey, Hash: "abcd1234", SizeBytes: 8192},
				},
			},
		},
	}
	bodyBytes, err := manifest.MarshalBody(body)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Commit(ctx, tx.Body{Type: "test", Actor: "u_test"}, func(*repo.RootView) ([]byte, error) {
		return bodyBytes, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	store.Close()

	// Now run inspect-manifest --json.
	var stdout, stderr bytes.Buffer
	code := runInspect(ctx,
		[]string{"--store=localfs://" + storeDir, "--json", "t", "r"},
		&stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, stderr.String())
	}

	var out map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json: %v\nstdout=%s", err, stdout.String())
	}

	// Verify the derived summary is under "reachability_summary".
	summaryRaw, ok := out["reachability_summary"]
	if !ok {
		t.Fatalf("JSON output missing 'reachability_summary' key; keys present: %v\nstdout=%s", keys2(out), stdout.String())
	}
	var rb map[string]json.RawMessage
	if err := json.Unmarshal(summaryRaw, &rb); err != nil {
		t.Fatalf("reachability_summary: %v", err)
	}
	for _, field := range []string{"base_manifest", "delta_chain_length", "delta_chain_bytes", "delta_files"} {
		if _, ok := rb[field]; !ok {
			t.Errorf("reachability_summary block missing field %q; keys present: %v", field, keys2(rb))
		}
	}
	var chainLen float64
	if err := json.Unmarshal(rb["delta_chain_length"], &chainLen); err != nil || chainLen != 1 {
		t.Errorf("delta_chain_length = %v, want 1", rb["delta_chain_length"])
	}
	var chainBytes float64
	if err := json.Unmarshal(rb["delta_chain_bytes"], &chainBytes); err != nil || chainBytes != 8192 {
		t.Errorf("delta_chain_bytes = %v, want 8192", rb["delta_chain_bytes"])
	}

	// Verify the raw "reachability" field is intact inside the "indexes" body field.
	indexesRaw, ok := out["indexes"]
	if !ok {
		t.Fatalf("JSON output missing 'indexes' key; keys present: %v", keys2(out))
	}
	var indexesMap map[string]json.RawMessage
	if err := json.Unmarshal(indexesRaw, &indexesMap); err != nil {
		t.Fatalf("indexes: %v", err)
	}
	rawReach, ok := indexesMap["reachability"]
	if !ok {
		t.Fatalf("indexes missing 'reachability' key; keys present: %v", keys2(indexesMap))
	}
	var rawRB map[string]json.RawMessage
	if err := json.Unmarshal(rawReach, &rawRB); err != nil {
		t.Fatalf("reachability raw: %v", err)
	}
	if _, ok := rawRB["base_manifest"]; !ok {
		t.Errorf("raw reachability missing 'base_manifest'")
	}
	if _, ok := rawRB["deltas"]; !ok {
		t.Errorf("raw reachability missing 'deltas' array")
	}
}

// TestRunInspect_JSON_NoReachabilityBlock verifies that inspect-manifest --json
// does NOT include a "reachability_summary" key for a pre-M10 (no deltas) repo.
func TestRunInspect_JSON_NoReachabilityBlock(t *testing.T) {
	dir := t.TempDir()
	var sink bytes.Buffer
	if code := runInit(context.Background(),
		[]string{"--store=localfs:" + dir, "acme", "x"}, &sink, &sink); code != 0 {
		t.Fatalf("init failed: %s", sink.String())
	}
	var stdout, stderr bytes.Buffer
	code := runInspect(context.Background(),
		[]string{"--store=localfs:" + dir, "--json", "acme", "x"},
		&stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, stderr.String())
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json: %v", err)
	}
	if _, ok := out["reachability_summary"]; ok {
		t.Errorf("unexpected 'reachability_summary' key in pre-M10 repo JSON output")
	}
}

// keys2 returns the keys of a map as a slice (for error messages).
func keys2[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

