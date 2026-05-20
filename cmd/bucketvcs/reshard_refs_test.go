package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestReshardRefs_UsageOnHelp(t *testing.T) {
	for _, flag := range []string{"-h", "--help"} {
		t.Run(flag, func(t *testing.T) {
			var stdout bytes.Buffer
			code := runReshardRefs(context.Background(), []string{flag}, &stdout, &bytes.Buffer{})
			if code != 0 {
				t.Errorf("code=%d want 0", code)
			}
			if !strings.Contains(stdout.String(), "reshard-refs") {
				t.Errorf("usage missing 'reshard-refs': %q", stdout.String())
			}
		})
	}
}

func TestReshardRefs_RequiresStore(t *testing.T) {
	var stderr bytes.Buffer
	code := runReshardRefs(context.Background(), []string{"--repo=acme/demo"}, &bytes.Buffer{}, &stderr)
	if code != 2 {
		t.Errorf("code=%d want 2", code)
	}
	if !strings.Contains(stderr.String(), "--store is required") {
		t.Errorf("missing required-flag message: %q", stderr.String())
	}
}

func TestReshardRefs_RequiresRepoFormat(t *testing.T) {
	var stderr bytes.Buffer
	code := runReshardRefs(context.Background(), []string{"--store=localfs:/tmp", "--repo=invalidformat"}, &bytes.Buffer{}, &stderr)
	if code != 2 {
		t.Errorf("code=%d want 2", code)
	}
	if !strings.Contains(stderr.String(), "<tenant>/<repo>") {
		t.Errorf("missing format-error message: %q", stderr.String())
	}
}

func TestReshardRefs_JSONOutput(t *testing.T) {
	tmp := t.TempDir()
	storeURL := "localfs:" + filepath.Join(tmp, "store")
	initCode := runInit(context.Background(), []string{
		"--store=" + storeURL,
		"--default-branch=refs/heads/main",
		"acme", "demo",
	}, &bytes.Buffer{}, &bytes.Buffer{})
	if initCode != 0 {
		t.Fatalf("init code=%d", initCode)
	}
	var stdout bytes.Buffer
	code := runReshardRefs(context.Background(), []string{
		"--store=" + storeURL,
		"--repo=acme/demo",
		"--json",
	}, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("code=%d, stdout=%q", code, stdout.String())
	}
	var payload struct {
		Outcome             string `json:"outcome"`
		RefCount            int    `json:"ref_count"`
		ShardCount          int    `json:"shard_count"`
		ManifestVersionFrom uint64 `json:"manifest_version_from"`
		ManifestVersionTo   uint64 `json:"manifest_version_to"`
		DurationMS          int64  `json:"duration_ms"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &payload); err != nil {
		t.Fatalf("unmarshal: %v; output=%q", err, stdout.String())
	}
	if payload.Outcome != "empty_v1_kept" {
		t.Errorf("outcome=%q want empty_v1_kept (fresh repo has no refs)", payload.Outcome)
	}
}
