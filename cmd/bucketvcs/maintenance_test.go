package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/maintenance/mtest"
	"github.com/bucketvcs/bucketvcs/internal/reachability/deltaindex"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/tx"
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

func TestMaintenance_CLI_ReachabilityFlags_Plumbed(t *testing.T) {
	mtest.GitAvailable(t)
	ctx := context.Background()

	// Seed a localfs repo, then close the store before running CLI commands
	// (localfs uses file locking; two instances cannot hold the lock).
	storeDir := t.TempDir()
	s, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	mtest.SeedRepoFromImport(t, s, "acme", "site")
	s.Close()

	storeURL := "localfs://" + storeDir

	// Run maintenance --force to build the base reachability index.
	var forceOut, forceErr bytes.Buffer
	if code := runMaintenance(ctx,
		[]string{"--store=" + storeURL, "--repo=acme/site", "--force", "--output=json"},
		&forceOut, &forceErr); code != 0 {
		t.Fatalf("force run exit %d (stderr=%s)", code, forceErr.String())
	}

	// Re-open the store to inject 5 synthetic .bvrd delta entries (> threshold of 3).
	s2, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Open(ctx, s2, "acme", "site")
	if err != nil {
		t.Fatal(err)
	}
	k, err := keys.NewRepo("acme", "site")
	if err != nil {
		t.Fatal(err)
	}
	deltas := make([]manifest.IndexRef, 5)
	for i := range deltas {
		d := deltaindex.Delta{
			Commits: []deltaindex.CommitRecord{{Generation: uint32(i + 2)}},
		}
		b, encErr := deltaindex.Encode(d)
		if encErr != nil {
			t.Fatalf("encode delta %d: %v", i, encErr)
		}
		sum := sha256.Sum256(b)
		hash := hex.EncodeToString(sum[:])
		dkey := k.ReachabilityDeltaKey(hash)
		if _, putErr := s2.PutIfAbsent(ctx, dkey, bytes.NewReader(b), nil); putErr != nil {
			t.Fatalf("PutIfAbsent delta %d: %v", i, putErr)
		}
		deltas[i] = manifest.IndexRef{Key: dkey, Hash: hash, SizeBytes: int64(len(b))}
	}
	_, injErr := r.Commit(ctx, tx.Body{Type: "test_inject_deltas", Actor: "u_test"},
		func(prev *repo.RootView) ([]byte, error) {
			var body manifest.Body
			if err := json.Unmarshal(prev.Body, &body); err != nil {
				return nil, err
			}
			if body.Indexes.Reachability == nil {
				body.Indexes.Reachability = &manifest.ReachabilityRef{}
			}
			body.Indexes.Reachability.Deltas = append(body.Indexes.Reachability.Deltas, deltas...)
			return manifest.MarshalBody(body)
		})
	if injErr != nil {
		t.Fatalf("inject deltas: %v", injErr)
	}
	s2.Close()

	// Run maintenance with --reachability-delta-pushes=3. With 5 deltas > 3,
	// reachability_compaction.triggered must be true.
	var stdout, stderr bytes.Buffer
	code := runMaintenance(ctx,
		[]string{
			"--store=" + storeURL,
			"--repo=acme/site",
			"--reachability-delta-pushes=3",
			"--output=json",
		},
		&stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d (stderr=%s)\nstdout=%s", code, stderr.String(), stdout.String())
	}
	var out []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json: %v\nstdout=%s", err, stdout.String())
	}
	if len(out) != 1 {
		t.Fatalf("got %d reports, want 1", len(out))
	}
	rc, ok := out[0]["reachability_compaction"].(map[string]any)
	if !ok {
		t.Fatalf("reachability_compaction missing or wrong type; report=%v", out[0])
	}
	if triggered, _ := rc["triggered"].(bool); !triggered {
		t.Errorf("reachability_compaction.triggered = false, want true; rc=%v", rc)
	}
}

func TestMaintenance_CLI_RejectsNegativeReachabilityThreshold(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runMaintenance(context.Background(),
		[]string{"--store=localfs:///tmp/x", "--repo=acme/site",
			"--reachability-delta-commits=-1"}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("exit = %d, want 2 for negative reachability-delta-commits", code)
	}
	if !strings.Contains(stderr.String(), "reachability-delta-commits") {
		t.Errorf("stderr does not mention flag name: %q", stderr.String())
	}
}

func TestParseByteSize(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"0", 0, false},
		{"1024", 1024, false},
		{"1K", 1024, false},
		{"1k", 1024, false},
		{"2M", 2 * 1024 * 1024, false},
		{"64M", 64 * 1024 * 1024, false},
		{"1G", 1024 * 1024 * 1024, false},
		{"", 0, true},
		{"abc", 0, true},
		{"-1", 0, true},
		{"-9223372036854775000G", 0, true}, // negative with multiplier
	}
	for _, tc := range cases {
		got, err := parseByteSize(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseByteSize(%q): want error, got %d", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseByteSize(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseByteSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParseByteSize_OverflowG(t *testing.T) {
	_, err := parseByteSize("9223372036854775807G")
	if err == nil {
		t.Fatal("expected overflow error, got nil")
	}
}

func TestRunMaintenance_BundleFlags_Parsed(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := runMaintenance(context.Background(), []string{"--help"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	for _, want := range []string{"--bundle-only", "--no-bundle", "--bundle-commits", "--bundle-age", "--bundle-default-branch"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("usage missing %q", want)
		}
	}
}

func TestRunMaintenance_BundleOnlyAndNoBundle_Reject(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := runMaintenance(context.Background(), []string{
		"--store=localfs:" + t.TempDir(),
		"--repo=t/r",
		"--bundle-only", "--no-bundle",
	}, &stdout, &stderr)
	if rc != 2 {
		t.Fatalf("rc = %d, want 2", rc)
	}
	if !strings.Contains(stderr.String(), "mutually exclusive") {
		t.Errorf("expected mutually-exclusive error: %s", stderr.String())
	}
}

