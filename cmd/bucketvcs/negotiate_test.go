package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/maintenance/mtest"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// TestNegotiate_CLI_TextOutput exercises the negotiate subcommand against a
// real localfs store. It seeds a single-commit repo via SeedRepoFromImport,
// runs maintenance --force to build the base index, then asserts that
// runNegotiate with --wants=<tip-oid> emits "Shipping plan:" on stdout.
func TestNegotiate_CLI_TextOutput(t *testing.T) {
	mtest.GitAvailable(t)
	ctx := context.Background()

	// Seed a localfs repo, then close the store before running CLI commands
	// (localfs uses file locking; two instances cannot hold the lock).
	storeDir := t.TempDir()
	s, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	mtest.SeedRepoFromImport(t, s, "t", "r")
	s.Close()

	storeURL := "localfs://" + storeDir

	// Build the base reachability index via maintenance --force.
	var mOut, mErr bytes.Buffer
	if code := runMaintenance(ctx,
		[]string{"--store=" + storeURL, "--repo=t/r", "--force", "--output=json"},
		&mOut, &mErr); code != 0 {
		t.Fatalf("maintenance exit %d (stderr=%s)", code, mErr.String())
	}

	// Re-open the store to read the manifest and extract the main tip OID.
	s2, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}

	r, err := repo.Open(ctx, s2, "t", "r")
	if err != nil {
		s2.Close()
		t.Fatalf("repo.Open: %v", err)
	}
	view, err := r.ReadRoot(ctx)
	if err != nil {
		s2.Close()
		t.Fatalf("ReadRoot: %v", err)
	}
	var body manifest.Body
	if err := json.Unmarshal(view.Body, &body); err != nil {
		s2.Close()
		t.Fatalf("Unmarshal body: %v", err)
	}
	tipOID, ok := body.Refs["refs/heads/main"]
	s2.Close()
	if !ok || tipOID == "" {
		t.Fatalf("refs/heads/main missing from manifest refs: %v", body.Refs)
	}

	// Run negotiate with --wants=<tip> and no haves (fresh clone scenario).
	// The output must contain "Shipping plan:" and the tip OID.
	var stdout, stderr bytes.Buffer
	code := runNegotiate(ctx,
		[]string{
			"--store=" + storeURL,
			"--repo=t/r",
			"--wants=" + tipOID,
		},
		&stdout, &stderr)
	if code != 0 {
		t.Fatalf("negotiate exit %d (stderr=%s)\nstdout=%s", code, stderr.String(), stdout.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Shipping plan:") {
		t.Errorf("stdout missing 'Shipping plan:'; got:\n%s", out)
	}
	if !strings.Contains(out, tipOID) {
		t.Errorf("stdout missing tip OID %s; got:\n%s", tipOID, out)
	}
}

// TestNegotiate_CLI_UnknownWant verifies exit code 3 is returned when the
// client requests a commit not present in the reachability index.
func TestNegotiate_CLI_UnknownWant(t *testing.T) {
	t.Skip("CLI integration test — implement once cmd/bucketvcs fixture harness is in place")
}
