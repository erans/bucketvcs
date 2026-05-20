# M12 Phase 6 — reshard maintenance op + CLI

> **Parent plan:** [docs/m12-ref-sharding-plan.md](../m12-ref-sharding-plan.md). Phases 0–5 must be complete.

**Goal:** add the one-shot manual inline→sharded migration. Two pieces:

1. `internal/maintenance/reshard.go` — the migration pipeline (read v1 body → shard refs → PutIfAbsent shards → CAS root with v2 body).
2. `cmd/bucketvcs/reshard_refs.go` — a new `bucketvcs reshard-refs` subcommand that wires CLI flags to the maintenance op.

After this phase an operator can run `bucketvcs reshard-refs --store=<URL> --repo=<tenant>/<repo>` and the repo flips to sharded mode.

**Files created/modified:**
- Create: `internal/maintenance/reshard.go`
- Create: `internal/maintenance/reshard_test.go`
- Create: `cmd/bucketvcs/reshard_refs.go`
- Create: `cmd/bucketvcs/reshard_refs_test.go`
- Modify: `cmd/bucketvcs/main.go` (add subcommand dispatch)

---

### Task 6.1: maintenance.Reshard function

**Files:**
- Create: `internal/maintenance/reshard.go`
- Create: `internal/maintenance/reshard_test.go`

- [ ] **Step 1: Write the failing reshard test against localfs.**

Create `internal/maintenance/reshard_test.go`:

```go
package maintenance_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/maintenance"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/refstore"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// seedRepoWithInlineRefs creates a fresh repo with the given inline
// refs. Returns the open Repo and a closing function.
func seedRepoWithInlineRefs(t *testing.T, refs map[string]string, defaultBranch string) (*repo.Repo, *keys.Repo, *localfs.Store, func()) {
	t.Helper()
	tmp := t.TempDir()
	store, err := localfs.Open(localfs.Config{Root: tmp})
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	k, err := keys.NewRepo("acme", "demo")
	if err != nil {
		t.Fatalf("keys.NewRepo: %v", err)
	}
	r, err := repo.Create(context.Background(), store, "acme", "demo", repo.CreateOptions{
		DefaultBranch: defaultBranch,
	})
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	// Inject refs via a Commit. Borrow the importer-style buildBody path.
	if len(refs) > 0 {
		_, err := r.Commit(context.Background(), tx.Body{Type: "push", Actor: "u_test"}, func(prev *repo.RootView) ([]byte, error) {
			body := manifest.Body{
				DefaultBranch: defaultBranch,
				Refs:          refs,
				Packs:         []manifest.PackEntry{},
				Bundles:       []manifest.BundleEntry{},
			}
			return manifest.MarshalBody(body)
		})
		if err != nil {
			t.Fatalf("seed Commit: %v", err)
		}
	}
	return r, k, store, func() { _ = store.Close() }
}
```

Add `"github.com/bucketvcs/bucketvcs/internal/repo/tx"` to the imports.

- [ ] **Step 2: Add the reshard happy-path test.**

Continue `internal/maintenance/reshard_test.go`:

```go
func TestReshard_HappyPath(t *testing.T) {
	refs := map[string]string{
		"refs/heads/main": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"refs/heads/dev":  "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"refs/tags/v1.0":  "cccccccccccccccccccccccccccccccccccccccc",
	}
	r, k, store, cleanup := seedRepoWithInlineRefs(t, refs, "refs/heads/main")
	defer cleanup()

	report, err := maintenance.Reshard(context.Background(), store, r, k, maintenance.ReshardOptions{
		Actor: "u_test",
	})
	if err != nil {
		t.Fatalf("Reshard: %v", err)
	}
	if report.Outcome != "success" {
		t.Errorf("Outcome=%q want success", report.Outcome)
	}
	if report.RefCount != 3 {
		t.Errorf("RefCount=%d want 3", report.RefCount)
	}

	// Read the new manifest and assert v2 shape.
	view, err := r.ReadRoot(context.Background())
	if err != nil {
		t.Fatalf("ReadRoot: %v", err)
	}
	body, err := manifest.UnmarshalBody(view.Body)
	if err != nil {
		t.Fatalf("UnmarshalBody: %v", err)
	}
	if len(body.Refs) != 0 {
		t.Errorf("Refs=%v want empty", body.Refs)
	}
	if body.RefSharding != "hash_v1" {
		t.Errorf("RefSharding=%q want hash_v1", body.RefSharding)
	}
	if len(body.RefShards) == 0 {
		t.Fatal("RefShards empty after reshard")
	}

	// Read every ref through ShardedRefStore to confirm round-trip.
	rs, err := refstore.New(context.Background(), store, k, &body)
	if err != nil {
		t.Fatalf("refstore.New: %v", err)
	}
	got, err := rs.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != len(refs) {
		t.Fatalf("List len=%d want %d", len(got), len(refs))
	}
	for k, v := range refs {
		if got[k] != v {
			t.Errorf("ref %q: got=%q want=%q", k, got[k], v)
		}
	}
}

func TestReshard_AlreadyV2IsNoop(t *testing.T) {
	// Build a v2 repo by hand (via reshard once), then run reshard
	// again and assert it exits "noop" without mutating.
	refs := map[string]string{"refs/heads/main": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	r, k, store, cleanup := seedRepoWithInlineRefs(t, refs, "refs/heads/main")
	defer cleanup()

	if _, err := maintenance.Reshard(context.Background(), store, r, k, maintenance.ReshardOptions{Actor: "u_test"}); err != nil {
		t.Fatalf("first Reshard: %v", err)
	}
	// Capture the version after first reshard.
	view, err := r.ReadRoot(context.Background())
	if err != nil {
		t.Fatalf("ReadRoot: %v", err)
	}
	versionBefore := view.Header.ManifestVersion

	report, err := maintenance.Reshard(context.Background(), store, r, k, maintenance.ReshardOptions{Actor: "u_test"})
	if err != nil {
		t.Fatalf("second Reshard: %v", err)
	}
	if report.Outcome != "noop" {
		t.Errorf("Outcome=%q want noop", report.Outcome)
	}

	view2, _ := r.ReadRoot(context.Background())
	if view2.Header.ManifestVersion != versionBefore {
		t.Errorf("ManifestVersion bumped on noop: before=%d after=%d", versionBefore, view2.Header.ManifestVersion)
	}
}

func TestReshard_EmptyRepoSucceeds(t *testing.T) {
	r, k, store, cleanup := seedRepoWithInlineRefs(t, map[string]string{}, "refs/heads/main")
	defer cleanup()
	report, err := maintenance.Reshard(context.Background(), store, r, k, maintenance.ReshardOptions{Actor: "u_test"})
	if err != nil {
		t.Fatalf("Reshard: %v", err)
	}
	if report.Outcome != "success" {
		t.Errorf("Outcome=%q want success", report.Outcome)
	}
	if report.RefCount != 0 {
		t.Errorf("RefCount=%d want 0", report.RefCount)
	}
	// Body should still v2 even with zero refs.
	view, _ := r.ReadRoot(context.Background())
	body, _ := manifest.UnmarshalBody(view.Body)
	if body.RefSharding != "hash_v1" {
		t.Errorf("RefSharding=%q want hash_v1", body.RefSharding)
	}
}

func TestReshard_ConcurrentMutationAborts(t *testing.T) {
	refs := map[string]string{"refs/heads/main": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	r, k, store, cleanup := seedRepoWithInlineRefs(t, refs, "refs/heads/main")
	defer cleanup()

	// Hook in a between-snapshot-and-CAS mutation by injecting an
	// observer via ReshardOptions. The test option lets us race a
	// concurrent commit between Read and CAS.
	mutated := false
	opts := maintenance.ReshardOptions{
		Actor: "u_test",
		BetweenSnapshotAndCAS: func() {
			if mutated {
				return
			}
			mutated = true
			// Push a small inline change to bump the manifest version.
			_, err := r.Commit(context.Background(), tx.Body{Type: "push", Actor: "u_race"}, func(prev *repo.RootView) ([]byte, error) {
				body, _ := manifest.UnmarshalBody(prev.Body)
				body.Refs["refs/heads/sneaky"] = "ffffffffffffffffffffffffffffffffffffffff"
				return manifest.MarshalBody(body)
			})
			if err != nil {
				t.Fatalf("racing commit: %v", err)
			}
		},
	}

	_, err := maintenance.Reshard(context.Background(), store, r, k, opts)
	if !errors.Is(err, maintenance.ErrConcurrentMutation) {
		t.Fatalf("err=%v want ErrConcurrentMutation", err)
	}
}
```

- [ ] **Step 3: Write the maintenance.Reshard implementation.**

Create `internal/maintenance/reshard.go`:

```go
package maintenance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/refstore"
	"github.com/bucketvcs/bucketvcs/internal/repo/tx"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// ErrConcurrentMutation indicates that a concurrent push won the root
// CAS during a Reshard call. The shard objects this Reshard wrote are
// orphaned (content-addressed; GC sweeps them after retention). The
// operator can retry — the next call will see the new manifest version
// and either re-target it or no-op if it's already v2.
var ErrConcurrentMutation = errors.New("maintenance: concurrent mutation during reshard")

// ReshardOptions configures a Reshard run.
type ReshardOptions struct {
	// Actor is the principal recorded in the tx record. Defaults to
	// "u_op" if empty (matches the maintenance convention).
	Actor string

	// BetweenSnapshotAndCAS is a test hook fired between the body
	// snapshot and the root CAS. Production callers leave it nil.
	BetweenSnapshotAndCAS func()
}

// ReshardReport summarises one Reshard call.
type ReshardReport struct {
	// Outcome is one of: "success" (resharded), "noop" (already v2),
	// "failed_concurrent_mutation", "failed_other".
	Outcome string

	// RefCount is the number of refs in the source body. Zero for
	// empty repos (still emits a v2 body with empty RefShards).
	RefCount int

	// ShardCount is the number of non-empty shards produced. Zero for
	// empty repos.
	ShardCount int

	// ManifestVersionFrom / ManifestVersionTo bracket the CAS. Both
	// equal on noop.
	ManifestVersionFrom uint64
	ManifestVersionTo   uint64

	DurationMS int64
}

// Reshard converts a v1 (inline) repo to v2 (sharded) by reading the
// current refs, sharding them via the hash_v1 strategy, writing each
// shard object via PutIfAbsent, then CAS-publishing a new root
// manifest with RefShards populated and Refs cleared.
//
// On an already-v2 repo this is a no-op (Outcome="noop"; no CAS
// attempted).
//
// On a concurrent push winning the CAS race, Reshard returns
// ErrConcurrentMutation. The already-written shard objects are
// orphans; GC sweeps them after retention. Operators retry — the
// retry sees the new manifest version and either no-ops (if the
// concurrent push happened to bump to v2, unlikely without M12
// involvement) or re-targets the new version.
func Reshard(ctx context.Context, store storage.ObjectStore, r *repo.Repo, k *keys.Repo, opts ReshardOptions) (ReshardReport, error) {
	start := time.Now()
	report := ReshardReport{Outcome: "failed_other"}

	view, err := r.ReadRoot(ctx)
	if err != nil {
		return report, fmt.Errorf("maintenance.Reshard: read root: %w", err)
	}
	report.ManifestVersionFrom = view.Header.ManifestVersion

	body, err := manifest.UnmarshalBody(view.Body)
	if err != nil {
		return report, fmt.Errorf("maintenance.Reshard: unmarshal body: %w", err)
	}

	// Already-v2: noop.
	if len(body.RefShards) > 0 {
		report.Outcome = "noop"
		report.ManifestVersionTo = view.Header.ManifestVersion
		report.RefCount = 0 // count of v1 refs migrated; zero on noop
		report.ShardCount = len(body.RefShards)
		report.DurationMS = time.Since(start).Milliseconds()
		return report, nil
	}

	// Build the sharded layout from body.Refs.
	report.RefCount = len(body.Refs)
	rs, err := refstore.New(ctx, store, k, &body)
	if err != nil {
		return report, fmt.Errorf("maintenance.Reshard: refstore: %w", err)
	}
	if rs.Mode() != refstore.ModeInline {
		// Defense in depth — shouldn't happen if the noop check above is correct.
		return report, fmt.Errorf("maintenance.Reshard: refstore unexpectedly sharded (mode=%v)", rs.Mode())
	}

	// Stage the refs as a sharded delta against an empty sharded body.
	// To do this cleanly, build a synthetic empty-shard body, construct
	// a ShardedRefStore over it, and Stage all of body.Refs.
	emptyShardedBody := &manifest.Body{
		DefaultBranch: body.DefaultBranch,
		RefShards:     []manifest.RefShard{},
		RefSharding:   "hash_v1",
	}
	target, err := refstore.New(ctx, store, k, emptyShardedBody)
	if err != nil {
		return report, fmt.Errorf("maintenance.Reshard: empty-sharded refstore: %w", err)
	}
	stage, err := target.Stage(ctx, body.Refs)
	if err != nil {
		return report, fmt.Errorf("maintenance.Reshard: stage: %w", err)
	}
	report.ShardCount = len(stage.NewRefShards)

	// Phase A: PutIfAbsent every shard object before the root CAS.
	for _, w := range stage.NewShardObjects {
		_, err := store.PutIfAbsent(ctx, w.Key, bytes.NewReader(w.Contents), nil)
		if err != nil && !errors.Is(err, storage.ErrAlreadyExists) {
			return report, fmt.Errorf("maintenance.Reshard: PutIfAbsent shard %s: %w", w.Key, err)
		}
	}

	if opts.BetweenSnapshotAndCAS != nil {
		opts.BetweenSnapshotAndCAS()
	}

	// Phase C: build new body and CAS.
	actor := opts.Actor
	if actor == "" {
		actor = "u_op"
	}
	_, err = r.Commit(ctx, tx.Body{Type: "reshard-refs", Actor: actor}, func(prev *repo.RootView) ([]byte, error) {
		// Inside the callback: we MUST re-check that the body is still v1
		// and matches what we staged. If concurrent mutation has occurred,
		// abort by returning ErrConcurrentMutation; Repo.Commit's retry loop
		// would retry forever if we don't fail-fast here.
		if prev.Header.ManifestVersion != view.Header.ManifestVersion {
			return nil, ErrConcurrentMutation
		}
		newBody := manifest.Body{
			DefaultBranch: body.DefaultBranch,
			Refs:          nil,
			RefShards:     stage.NewRefShards,
			RefSharding:   "hash_v1",
			Packs:         body.Packs,
			Indexes:       body.Indexes,
			Bundles:       body.Bundles,
		}
		return manifest.MarshalBody(newBody)
	})
	if err != nil {
		if errors.Is(err, ErrConcurrentMutation) {
			report.Outcome = "failed_concurrent_mutation"
			report.DurationMS = time.Since(start).Milliseconds()
			return report, ErrConcurrentMutation
		}
		return report, fmt.Errorf("maintenance.Reshard: commit: %w", err)
	}

	view2, err := r.ReadRoot(ctx)
	if err != nil {
		// Reshard committed; the version readback is informational. Don't fail.
		report.Outcome = "success"
		report.DurationMS = time.Since(start).Milliseconds()
		return report, nil
	}
	report.ManifestVersionTo = view2.Header.ManifestVersion
	report.Outcome = "success"
	report.DurationMS = time.Since(start).Milliseconds()
	return report, nil
}
```

- [ ] **Step 4: Sanity-check the tx.Body field names against the current code.**

Run: `grep -nA 15 "^type Body struct" internal/repo/tx/record.go`

The plan was written against `tx.Body{Type, Actor, RefUpdates, NewPacks, Validation}`. If any field has been renamed since (very unlikely in M12-era), update the literal at the call sites.

- [ ] **Step 5: Run the tests.**

```bash
go test ./internal/maintenance/ -run TestReshard -count=1 -v
```

Expected: HappyPath and AlreadyV2 PASS. The ConcurrentMutation test may need adjustment depending on whether `Repo.Commit` surfaces `ErrConcurrentMutation` unwrapped — verify the error chain.

- [ ] **Step 6: Commit.**

```bash
git add internal/maintenance/reshard.go internal/maintenance/reshard_test.go
git commit -m "maintenance: Reshard (inline → sharded one-shot migration) (M12 Phase 6.1)"
```

---

### Task 6.2: CLI subcommand `bucketvcs reshard-refs`

**Files:**
- Create: `cmd/bucketvcs/reshard_refs.go`
- Create: `cmd/bucketvcs/reshard_refs_test.go`
- Modify: `cmd/bucketvcs/main.go`

- [ ] **Step 1: Wire the subcommand into the dispatcher.**

In `cmd/bucketvcs/main.go`, find the `switch sub { ... }` block. Add:

```go
case "reshard-refs":
	return runReshardRefs(ctx, rest, stdout, stderr)
```

Also add a line to the `usage(w io.Writer)` function's "Subcommands:" list:

```
  reshard-refs       Convert a repo from inline refs to sharded refs (M12)
```

- [ ] **Step 2: Implement `runReshardRefs`.**

Create `cmd/bucketvcs/reshard_refs.go`:

```go
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/bucketvcs/bucketvcs/internal/maintenance"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
)

const reshardRefsUsage = `usage: bucketvcs reshard-refs --store=<URL> --repo=<tenant>/<repo> [--actor=<u_*>]

Convert one repo from inline refs (v1) to sharded refs (v2).

This is a one-shot manual migration. After it succeeds, every push goes
through the sharded code path, and the root manifest no longer carries
the full ref list. Below ~10k refs, inline mode is faster — operators
should opt in only when scale warrants it.

The migration tolerates concurrent pushes but may fail with
"concurrent mutation" if a push wins the root CAS race; in that case
operators retry. Already-sharded repos are a no-op.

Flags:
  --store      Storage URL (e.g. localfs:/path, s3://bucket, gcs://bucket).
  --repo       Repo identifier in <tenant>/<repo> form.
  --actor      Principal recorded in the tx record. Defaults to u_op.
  --json       Emit the run report as JSON on stdout instead of a text summary.
`

func runReshardRefs(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reshard-refs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, reshardRefsUsage) }

	storeURL := fs.String("store", "", "")
	repoPath := fs.String("repo", "", "")
	actor := fs.String("actor", "u_op", "")
	asJSON := fs.Bool("json", false, "")
	help := fs.Bool("h", false, "")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *help {
		fmt.Fprint(stdout, reshardRefsUsage)
		return 0
	}
	if *storeURL == "" {
		fmt.Fprintln(stderr, "reshard-refs: --store is required")
		return 2
	}
	if *repoPath == "" {
		fmt.Fprintln(stderr, "reshard-refs: --repo is required")
		return 2
	}
	tenant, repoID, ok := splitTenantRepo(*repoPath)
	if !ok {
		fmt.Fprintf(stderr, "reshard-refs: --repo=%q must be <tenant>/<repo>\n", *repoPath)
		return 2
	}

	store, err := openStore(ctx, *storeURL)
	if err != nil {
		fmt.Fprintf(stderr, "reshard-refs: open store: %v\n", err)
		return 1
	}
	defer store.Close()

	k, err := keys.NewRepo(tenant, repoID)
	if err != nil {
		fmt.Fprintf(stderr, "reshard-refs: keys: %v\n", err)
		return 2
	}
	r, err := repo.Open(ctx, store, tenant, repoID)
	if err != nil {
		fmt.Fprintf(stderr, "reshard-refs: open repo: %v\n", err)
		return 1
	}

	report, err := maintenance.Reshard(ctx, store, r, k, maintenance.ReshardOptions{Actor: *actor})
	if err != nil {
		if errors.Is(err, maintenance.ErrConcurrentMutation) {
			fmt.Fprintf(stderr, "reshard-refs: aborted due to concurrent mutation; retry the command\n")
			return 1
		}
		fmt.Fprintf(stderr, "reshard-refs: %v\n", err)
		return 1
	}

	if *asJSON {
		fmt.Fprintf(stdout, `{"outcome":%q,"ref_count":%d,"shard_count":%d,"manifest_version_from":%d,"manifest_version_to":%d,"duration_ms":%d}`,
			report.Outcome, report.RefCount, report.ShardCount, report.ManifestVersionFrom, report.ManifestVersionTo, report.DurationMS)
		fmt.Fprintln(stdout)
		return 0
	}
	fmt.Fprintf(stdout, "reshard-refs %s/%s: %s (refs=%d shards=%d v%d→v%d %dms)\n",
		tenant, repoID, report.Outcome, report.RefCount, report.ShardCount,
		report.ManifestVersionFrom, report.ManifestVersionTo, report.DurationMS)
	return 0
}

// splitTenantRepo splits "tenant/repo" into the two parts. Returns
// ok=false if the input does not contain exactly one slash.
func splitTenantRepo(s string) (tenant, repoID string, ok bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			if i == 0 || i == len(s)-1 {
				return "", "", false
			}
			// Reject multiple slashes.
			for j := i + 1; j < len(s); j++ {
				if s[j] == '/' {
					return "", "", false
				}
			}
			return s[:i], s[i+1:], true
		}
	}
	return "", "", false
}
```

The `openStore(ctx, storeURL)` helper already exists in `cmd/bucketvcs/store.go` — verify with:

Run: `grep -n "func openStore" cmd/bucketvcs/*.go`

If the function name differs, adjust the call accordingly.

- [ ] **Step 3: Write a CLI smoke test.**

Create `cmd/bucketvcs/reshard_refs_test.go`:

```go
package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestReshardRefs_UsageOnHelp(t *testing.T) {
	var stdout bytes.Buffer
	code := runReshardRefs(context.Background(), []string{"-h"}, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Errorf("code=%d want 0", code)
	}
	if !strings.Contains(stdout.String(), "reshard-refs") {
		t.Errorf("usage missing 'reshard-refs': %q", stdout.String())
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

func TestSplitTenantRepo(t *testing.T) {
	cases := []struct {
		in       string
		tenant   string
		repoID   string
		ok       bool
	}{
		{"acme/demo", "acme", "demo", true},
		{"acme/demo/extra", "", "", false},
		{"acme", "", "", false},
		{"/demo", "", "", false},
		{"acme/", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			tenant, repoID, ok := splitTenantRepo(c.in)
			if tenant != c.tenant || repoID != c.repoID || ok != c.ok {
				t.Errorf("splitTenantRepo(%q) = (%q, %q, %v) want (%q, %q, %v)",
					c.in, tenant, repoID, ok, c.tenant, c.repoID, c.ok)
			}
		})
	}
}
```

- [ ] **Step 4: Run the CLI tests.**

```bash
go test ./cmd/bucketvcs/... -run Reshard -count=1 -v
```

Expected: PASS.

- [ ] **Step 5: Run the full sweep.**

```bash
go test ./... -count=1 2>&1 | grep -E "^(FAIL|---)" | head -10
```

Expected: empty.

- [ ] **Step 6: Manual smoke against localfs.**

```bash
go build -o /tmp/bvcs ./cmd/bucketvcs
TMP=$(mktemp -d)
/tmp/bvcs init --store=localfs:"$TMP" --repo=acme/demo --default-branch=refs/heads/main
# (use the existing import or test-only helper to inject refs; or just run reshard against an empty repo)
/tmp/bvcs reshard-refs --store=localfs:"$TMP" --repo=acme/demo
# Expected output: "reshard-refs acme/demo: success (refs=0 shards=0 v1→v2 ?ms)"
/tmp/bvcs reshard-refs --store=localfs:"$TMP" --repo=acme/demo
# Expected output: "reshard-refs acme/demo: noop (refs=0 shards=0 v2→v2 ?ms)"
```

If the second invocation reports `success` instead of `noop`, the no-op detection is broken; revisit Task 6.1.

- [ ] **Step 7: Commit.**

```bash
git add cmd/bucketvcs/reshard_refs.go cmd/bucketvcs/reshard_refs_test.go cmd/bucketvcs/main.go
git commit -m "cmd: bucketvcs reshard-refs subcommand (M12 Phase 6.2)"
```

---

### Task 6.3: Phase 6 boundary checkpoint

- [ ] **Step 1: Sweep + vet.**

```bash
go test ./... -count=1 2>&1 | grep -E "^(FAIL|---)" | head -10
go vet ./...
```

Expected: empty + clean.

- [ ] **Step 2: Two-stage review.**

Focus areas:
- Concurrent-mutation safety: the `BetweenSnapshotAndCAS` hook simulation; whether the CAS race truly aborts.
- Idempotency: re-running the CLI on a v2 repo emits "noop" not "success".
- Orphan accounting: shard objects from a failed CAS retry attempt are NOT silently leaked into the manifest.
- `splitTenantRepo` rejects malformed inputs.

- [ ] **Step 3: roborev-refine.**

- [ ] **Step 4: Proceed to Phase 7 (GC integration).**
