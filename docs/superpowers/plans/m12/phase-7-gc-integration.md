# M12 Phase 7 — GC integration

> **Parent plan:** [docs/m12-ref-sharding-plan.md](../m12-ref-sharding-plan.md). Phases 0–6 must be complete.

**Goal:** make `gc.BuildLiveSet` include `body.RefShards[*].Key` in the live set so a GC sweep against a v2 manifest does NOT delete the live shard objects. This is the smallest phase but the most safety-critical: without it, the first GC after a reshard would corrupt the repo.

**Why this phase is in M12 and not deferred:** the live-set builder is the gating contract for what survives sweep. Adding RefShards is a one-line additive change; deferring it means anyone who reshards before the follow-on milestone loses their refs at the next GC.

**Files modified:**
- `internal/gc/liveset.go`
- `internal/gc/liveset_test.go` (or add to existing test file)

---

### Task 7.1: Extend `BuildLiveSet`

**Files:**
- Modify: `internal/gc/liveset.go`
- Modify: `internal/gc/liveset_test.go`

- [ ] **Step 1: Locate the existing live-set builder.**

Already explored — `internal/gc/liveset.go:BuildLiveSet` iterates `body.Packs`, `body.Indexes.*`, `body.Indexes.Reachability.Deltas`, and `body.Bundles`. The block ends at line 75 (return live, nil).

The file already mentions M12 in a comment:
> "Future-fields recognized but currently emitted as empty (ref-shards for M12) are tolerated."

Now we wire the actual extraction.

- [ ] **Step 2: Write the failing test first.**

Modify `internal/gc/liveset_test.go` (or add a new test if the file's pattern allows). The test asserts that a v2 body's `RefShards[*].Key` appear in the live set:

```go
func TestBuildLiveSet_IncludesRefShards(t *testing.T) {
	k, err := keys.NewRepo("acme", "demo")
	if err != nil { t.Fatalf("keys.NewRepo: %v", err) }
	header := manifest.RootHeader{
		SchemaVersion:   2,
		RepoID:          "demo",
		ManifestVersion: 1,
		LatestTx:        "tx_abc",
	}
	body := manifest.Body{
		DefaultBranch: "refs/heads/main",
		RefShards: []manifest.RefShard{
			{Shard: "00", Key: k.RefShardKey("sha256-aa00000000000000000000000000000000000000000000000000000000000000"), Hash: "sha256-aa00000000000000000000000000000000000000000000000000000000000000", RefCount: 1},
			{Shard: "f3", Key: k.RefShardKey("sha256-bb00000000000000000000000000000000000000000000000000000000000000"), Hash: "sha256-bb00000000000000000000000000000000000000000000000000000000000000", RefCount: 2},
		},
		RefSharding: "hash_v1",
		Packs:       []manifest.PackEntry{},
		Bundles:     []manifest.BundleEntry{},
	}
	bodyJSON, err := manifest.MarshalBody(body)
	if err != nil { t.Fatalf("MarshalBody: %v", err) }
	live, err := gc.BuildLiveSet(k, header, bodyJSON)
	if err != nil { t.Fatalf("BuildLiveSet: %v", err) }
	for _, s := range body.RefShards {
		if _, ok := live[s.Key]; !ok {
			t.Errorf("RefShard.Key %q missing from live set", s.Key)
		}
	}
}

func TestBuildLiveSet_V1BodyHasNoShardKeys(t *testing.T) {
	// Regression guard: a v1 body must NOT add any phantom ref-shard
	// keys to the live set.
	k, _ := keys.NewRepo("acme", "demo")
	header := manifest.RootHeader{SchemaVersion: 1, RepoID: "demo", ManifestVersion: 1}
	body := manifest.Body{
		DefaultBranch: "refs/heads/main",
		Refs:          map[string]string{"refs/heads/main": "aa"},
		Packs:         []manifest.PackEntry{},
		Bundles:       []manifest.BundleEntry{},
	}
	bodyJSON, _ := manifest.MarshalBody(body)
	live, err := gc.BuildLiveSet(k, header, bodyJSON)
	if err != nil { t.Fatalf("BuildLiveSet: %v", err) }
	// Look for any key under ref-shards/ — should be zero.
	for key := range live {
		if strings.Contains(key, "ref-shards/") {
			t.Errorf("v1 body produced ref-shards live key: %q", key)
		}
	}
}
```

Add the missing imports to the test file: `manifest`, `gc`, `keys`, `strings`, `testing`.

- [ ] **Step 3: Run the test; expect FAIL.**

```bash
go test ./internal/gc/... -run TestBuildLiveSet_IncludesRefShards -count=1 -v
```

Expected: FAIL ("RefShard.Key ... missing from live set").

- [ ] **Step 4: Add the extraction to `BuildLiveSet`.**

In `internal/gc/liveset.go`, immediately after the existing `for _, b := range body.Bundles { ... }` loop, insert:

```go
	for _, s := range body.RefShards {
		if s.Key != "" {
			live[s.Key] = struct{}{}
		}
	}
```

Also update the doc-comment that mentions "ref-shards for M12 ... tolerated" — change "tolerated" to "live-set members" or similar:

```go
// The body is parsed as manifest.Body. Unknown fields in the body are
// ignored. M12 fields (RefShards) are extracted and added to the live
// set so a GC sweep against a v2 manifest does not delete live
// ref-shard objects.
```

- [ ] **Step 5: Run the tests; expect PASS.**

```bash
go test ./internal/gc/... -count=1 -v 2>&1 | tail -30
```

Expected: every gc test passes; the two new TestBuildLiveSet tests PASS.

- [ ] **Step 6: Commit.**

```bash
git add internal/gc/liveset.go internal/gc/liveset_test.go
git commit -m "gc: BuildLiveSet enumerates RefShards keys (M12 Phase 7)"
```

---

### Task 7.2: Integration test — sweep does not delete shards

**Files:**
- Modify: `internal/gc/` (add or extend an integration test)

- [ ] **Step 1: Find an existing GC integration test to mirror.**

Run: `ls internal/gc/`

Look for a test that runs `RunMark` + `RunSweep` against localfs. If `mark_test.go` or `sweep_test.go` has an end-to-end harness, mirror it for v2.

- [ ] **Step 2: Write the integration test.**

The structure is: build a v2 repo (via the reshard CLI helper from Phase 6, or directly via `manifesttest.MakeShardedBody`), run `gc.RunMark` + `gc.RunSweep`, list the storage, assert the shard keys are still present.

If the existing GC tests don't have a clean fixture, this can be deferred to Phase 8's smoke (`scripts/m12-reshard-smoke.sh`) which does a full reshard → GC → fetch roundtrip. In that case skip Task 7.2 and document the deferral inline.

Decision: if writing this integration test takes more than 30 minutes (i.e., requires building a non-trivial fixture), defer to the Phase 8 smoke. Otherwise add it here.

- [ ] **Step 3: Either commit the integration test, or document the deferral.**

If skipped:

```bash
# No commit; deferral is documented in Phase 8's smoke script comment.
```

If implemented:

```bash
git add internal/gc/gc_v2_integration_test.go
git commit -m "gc: integration test — v2 sweep preserves ref shards (M12 Phase 7.2)"
```

---

### Task 7.3: Phase 7 boundary checkpoint

- [ ] **Step 1: Sweep + vet.**

```bash
go test ./... -count=1 2>&1 | grep -E "^(FAIL|---)" | head -10
go vet ./...
```

Expected: empty + clean.

- [ ] **Step 2: Two-stage review.**

Focus areas:
- `BuildLiveSet` change is additive (no removed keys, no semantic change for v1 bodies).
- The doc comment is updated to reflect the new behavior.
- Tests cover both directions (v2 adds keys; v1 does not phantom-add).

- [ ] **Step 3: roborev-refine.**

- [ ] **Step 4: Proceed to Phase 8 (smoke, docs, tag).**
