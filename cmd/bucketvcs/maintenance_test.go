package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/maintenance/mtest"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestRunMaintenance_RejectsMissingStoreFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runMaintenance(context.Background(), []string{"--repo=acme/site"}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "store") {
		t.Errorf("stderr does not mention --store: %q", stderr.String())
	}
}

func TestRunMaintenance_RejectsBothRepoAndAllRepos(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runMaintenance(context.Background(),
		[]string{"--store=localfs:///tmp/x", "--repo=acme/site", "--all-repos"}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
}

func TestRunMaintenance_RejectsSubHourRecentWindow(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runMaintenance(context.Background(),
		[]string{"--store=localfs:///tmp/x", "--repo=acme/site", "--recent-window=30m"}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "1h") {
		t.Errorf("stderr does not mention 1h minimum: %q", stderr.String())
	}
}

func TestRunMaintenance_HelpFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runMaintenance(context.Background(), []string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "maintenance") {
		t.Errorf("usage missing 'maintenance': %q", stdout.String())
	}
}

func TestRunMaintenance_AllReposEmptyStore(t *testing.T) {
	storeDir := t.TempDir()
	storeURL := "localfs://" + storeDir
	var stdout, stderr bytes.Buffer
	code := runMaintenance(context.Background(),
		[]string{"--store=" + storeURL, "--all-repos", "--output=json"}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit = %d, want 0 (empty store, zero repos)", code)
	}
	// Output should be a JSON array (possibly empty).
	if stdout.Len() == 0 {
		t.Errorf("stdout empty; want JSON array")
	}
}

func TestRunMaintenance_E2E_SingleRepoConvergesToOnePack(t *testing.T) {
	mtest.GitAvailable(t)

	storeDir := t.TempDir()
	s, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	mtest.SeedRepoFromImport(t, s, "acme", "site")
	s.Close()

	storeURL := "localfs://" + storeDir
	var stdout, stderr bytes.Buffer
	code := runMaintenance(context.Background(),
		[]string{"--store=" + storeURL, "--repo=acme/site", "--force", "--output=json"},
		&stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d (stderr=%s)", code, stderr.String())
	}
	var out []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json: %v\nstdout=%s", err, stdout.String())
	}
	if len(out) != 1 {
		t.Fatalf("got %d reports, want 1", len(out))
	}
	if out[0]["outcome"] != "success" {
		t.Errorf("outcome = %v, want success", out[0]["outcome"])
	}
	apc, ok := out[0]["after_pack_count"].(float64)
	if !ok || int(apc) != 1 {
		t.Errorf("after_pack_count = %v, want 1", out[0]["after_pack_count"])
	}
}
