# M12 Phase 3 — ShardedRefStore.Stage

> **Parent plan:** [docs/m12-ref-sharding-plan.md](../m12-ref-sharding-plan.md). Phases 0–2 must be complete.

**Goal:** implement `ShardedRefStore.Stage` — the push-time delta builder. Given a set of ref updates, group them by shard, load only the affected shards, apply the updates, canonicalize each new shard's bytes, compute its content hash, and return a `Stage` containing the new `[]ShardWrite` (to PutIfAbsent) and the new `[]manifest.RefShard` (to publish in the new body).

**Files touched:**
- Modify: `internal/repo/refstore/sharded.go` (replace Stage stub)
- Modify: `internal/repo/refstore/sharded_test.go` (Stage tests)

---

### Task 3.1: ShardedRefStore.Stage

**Files:**
- Modify: `internal/repo/refstore/sharded.go`
- Modify: `internal/repo/refstore/sharded_test.go`

- [ ] **Step 1: Write the failing Stage tests first.**

Append to `internal/repo/refstore/sharded_test.go`:

```go
func TestSharded_Stage_AddOneRef(t *testing.T) {
	mainShard := refstore.ShardKey("refs/heads/main")
	body, store, k := shardFixture(t, map[string]map[string]string{
		mainShard: {"refs/heads/main": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	})
	rs, _ := refstore.New(context.Background(), store, k, body)
	stage, err := rs.Stage(context.Background(), map[string]string{
		"refs/heads/feature": "ffffffffffffffffffffffffffffffffffffffff",
	})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if stage.Mode != refstore.ModeSharded {
		t.Errorf("Mode=%v want sharded", stage.Mode)
	}
	if stage.NewInlineRefs != nil {
		t.Errorf("NewInlineRefs=%v want nil", stage.NewInlineRefs)
	}
	// Exactly one shard changed (either the same as main's shard, or a new one).
	featShard := refstore.ShardKey("refs/heads/feature")
	if featShard == mainShard {
		// Single shard touched — its content changed.
		if len(stage.NewShardObjects) != 1 {
			t.Fatalf("NewShardObjects len=%d want 1", len(stage.NewShardObjects))
		}
		if len(stage.NewRefShards) != 1 {
			t.Fatalf("NewRefShards len=%d want 1", len(stage.NewRefShards))
		}
	} else {
		// Two shards in the result: the unchanged main shard + the new feature shard.
		if len(stage.NewShardObjects) != 1 {
			t.Fatalf("NewShardObjects len=%d want 1 (only the new feature shard rewrites)", len(stage.NewShardObjects))
		}
		if len(stage.NewRefShards) != 2 {
			t.Fatalf("NewRefShards len=%d want 2", len(stage.NewRefShards))
		}
	}
}

func TestSharded_Stage_DeletionRemovesEmptyShard(t *testing.T) {
	// Start with a shard containing a single ref. Delete it. The
	// shard should be removed from NewRefShards (not emitted as
	// "{}").
	mainShard := refstore.ShardKey("refs/heads/only")
	body, store, k := shardFixture(t, map[string]map[string]string{
		mainShard: {"refs/heads/only": "aa"},
	})
	rs, _ := refstore.New(context.Background(), store, k, body)
	stage, err := rs.Stage(context.Background(), map[string]string{"refs/heads/only": ""})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if len(stage.NewRefShards) != 0 {
		t.Errorf("NewRefShards=%v want empty (shard emptied)", stage.NewRefShards)
	}
	if len(stage.NewShardObjects) != 0 {
		t.Errorf("NewShardObjects=%v want empty (no shard to write)", stage.NewShardObjects)
	}
}

func TestSharded_Stage_ThreeShardsTouched(t *testing.T) {
	// Construct refs that land in 3 distinct shards, then update one
	// from each shard. The Stage should produce 3 ShardWrites.
	tries := []string{
		"refs/heads/a", "refs/heads/b", "refs/heads/c", "refs/heads/d",
		"refs/heads/e", "refs/heads/f", "refs/heads/g", "refs/heads/h",
	}
	byShard := map[string][]string{}
	for _, n := range tries {
		sid := refstore.ShardKey(n)
		byShard[sid] = append(byShard[sid], n)
		if len(byShard) >= 3 && len(byShard[sid]) >= 1 {
			// have at least 3 distinct shards
		}
	}
	if len(byShard) < 3 {
		t.Skip("could not find 3 distinct shards from candidate refnames; rare sha256 distribution")
	}
	// Pick one refname from each of three distinct shards.
	var picks []string
	for _, names := range byShard {
		picks = append(picks, names[0])
		if len(picks) == 3 {
			break
		}
	}
	perShard := map[string]map[string]string{}
	for _, n := range picks {
		sid := refstore.ShardKey(n)
		if perShard[sid] == nil {
			perShard[sid] = map[string]string{}
		}
		perShard[sid][n] = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	}
	body, store, k := shardFixture(t, perShard)
	rs, _ := refstore.New(context.Background(), store, k, body)
	updates := map[string]string{}
	for _, n := range picks {
		updates[n] = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	}
	stage, err := rs.Stage(context.Background(), updates)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if len(stage.NewShardObjects) != 3 {
		t.Errorf("NewShardObjects len=%d want 3", len(stage.NewShardObjects))
	}
	if len(stage.NewRefShards) != 3 {
		t.Errorf("NewRefShards len=%d want 3", len(stage.NewRefShards))
	}
}

func TestSharded_Stage_UntouchedShardPreserved(t *testing.T) {
	// Two shards in the body. Update a ref in one. The other shard
	// must appear in NewRefShards with its original hash + key
	// (unchanged), and must NOT appear in NewShardObjects.
	aShard := refstore.ShardKey("refs/heads/a")
	bShard := refstore.ShardKey("refs/heads/b")
	if aShard == bShard {
		t.Skip("a and b share a shard; pick different fixture refnames")
	}
	body, store, k := shardFixture(t, map[string]map[string]string{
		aShard: {"refs/heads/a": "aa"},
		bShard: {"refs/heads/b": "bb"},
	})
	// Capture the original B-shard reference for comparison.
	var origB manifest.RefShard
	for _, s := range body.RefShards {
		if s.Shard == bShard {
			origB = s
			break
		}
	}
	rs, _ := refstore.New(context.Background(), store, k, body)
	stage, err := rs.Stage(context.Background(), map[string]string{"refs/heads/a": "cc"})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	// B-shard appears unchanged in NewRefShards.
	var newB manifest.RefShard
	for _, s := range stage.NewRefShards {
		if s.Shard == bShard {
			newB = s
			break
		}
	}
	if newB.Hash != origB.Hash || newB.Key != origB.Key {
		t.Errorf("unchanged B-shard mutated: orig=%+v new=%+v", origB, newB)
	}
	// B-shard NOT in NewShardObjects (no re-PUT for unchanged shards).
	for _, w := range stage.NewShardObjects {
		if w.Hash == origB.Hash {
			t.Errorf("untouched shard appears in NewShardObjects: %+v", w)
		}
	}
}

func TestSharded_Stage_NullOIDDeletes(t *testing.T) {
	const nullOID = "0000000000000000000000000000000000000000"
	mainShard := refstore.ShardKey("refs/heads/only")
	body, store, k := shardFixture(t, map[string]map[string]string{
		mainShard: {"refs/heads/only": "aa"},
	})
	rs, _ := refstore.New(context.Background(), store, k, body)
	stage, err := rs.Stage(context.Background(), map[string]string{"refs/heads/only": nullOID})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if len(stage.NewRefShards) != 0 {
		t.Errorf("nullOID should delete and empty shard; got NewRefShards=%v", stage.NewRefShards)
	}
}

func TestSharded_Stage_BackendFetchError(t *testing.T) {
	body, store, k := shardFixture(t, map[string]map[string]string{
		refstore.ShardKey("refs/heads/main"): {"refs/heads/main": "aa"},
	})
	store.(*fakeStore).failOnGet = true
	rs, _ := refstore.New(context.Background(), store, k, body)
	_, err := rs.Stage(context.Background(), map[string]string{"refs/heads/main": "bb"})
	if err == nil {
		t.Fatal("Stage: want error, got nil")
	}
}

func TestSharded_Stage_DeterministicShardObjects(t *testing.T) {
	// The same updates against the same body must produce
	// byte-identical NewShardObjects across runs.
	mainShard := refstore.ShardKey("refs/heads/main")
	updates := map[string]string{
		"refs/heads/new1": "1111111111111111111111111111111111111111",
		"refs/heads/new2": "2222222222222222222222222222222222222222",
	}
	body, store, k := shardFixture(t, map[string]map[string]string{
		mainShard: {"refs/heads/main": "aa"},
	})
	rs, _ := refstore.New(context.Background(), store, k, body)
	first, err := rs.Stage(context.Background(), updates)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	for i := 0; i < 20; i++ {
		rs2, _ := refstore.New(context.Background(), store, k, body)
		again, err := rs2.Stage(context.Background(), updates)
		if err != nil {
			t.Fatalf("Stage retry: %v", err)
		}
		if len(again.NewShardObjects) != len(first.NewShardObjects) {
			t.Fatalf("non-deterministic count: first=%d later=%d", len(first.NewShardObjects), len(again.NewShardObjects))
		}
		for j, w := range first.NewShardObjects {
			if w.Hash != again.NewShardObjects[j].Hash {
				t.Errorf("non-deterministic hash j=%d first=%s later=%s", j, w.Hash, again.NewShardObjects[j].Hash)
			}
		}
	}
}
```

- [ ] **Step 2: Run the tests; expect FAIL (Stage still ErrUnsupported).**

Run: `go test ./internal/repo/refstore/... -run TestSharded_Stage -count=1 2>&1 | head -30`
Expected: builds; tests FAIL with ErrUnsupported.

- [ ] **Step 3: Implement Stage.**

Replace the `Stage` method in `internal/repo/refstore/sharded.go`:

```go
// Stage groups updates by shard, loads only the affected shards from
// the store (parallel; same hash-verification as Lookup/List), applies
// the updates to those shards in memory, computes the new canonical
// content + hash for each, and returns the full Stage.
//
// Untouched shards are forwarded into NewRefShards verbatim (same
// hash/key) so the new body still references them. A shard that
// becomes empty after applied deletions is dropped from NewRefShards
// entirely; no "{}" shard object is written.
//
// Deletion convention matches InlineRefStore.Stage: empty OID or
// 40-zero nullOIDHex deletes; any other 40-hex OID upserts.
func (s *ShardedRefStore) Stage(ctx context.Context, updates map[string]string) (Stage, error) {
	if len(updates) == 0 {
		// No changes: return the existing shard list as-is.
		out := make([]manifest.RefShard, len(s.shards))
		copy(out, s.shards)
		return Stage{Mode: ModeSharded, NewRefShards: out}, nil
	}

	// Group updates by shard ID.
	updatesByShard := make(map[string]map[string]string)
	for refname, oid := range updates {
		sid := shardKey(refname)
		if updatesByShard[sid] == nil {
			updatesByShard[sid] = make(map[string]string)
		}
		updatesByShard[sid][refname] = oid
	}

	// Index existing shards by ID for O(1) lookup.
	existingByShard := make(map[string]manifest.RefShard, len(s.shards))
	for _, sh := range s.shards {
		existingByShard[sh.Shard] = sh
	}

	// Load existing contents for every affected shard (parallel).
	loadedRefs := make(map[string]map[string]string, len(updatesByShard))
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	for sid := range updatesByShard {
		sid := sid
		existing, exists := existingByShard[sid]
		if !exists {
			// No existing shard for this ID; start from empty.
			mu.Lock()
			loadedRefs[sid] = map[string]string{}
			mu.Unlock()
			continue
		}
		g.Go(func() error {
			refs, err := s.fetchShard(gctx, existing)
			if err != nil {
				return err
			}
			mu.Lock()
			loadedRefs[sid] = refs
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return Stage{}, err
	}

	// Apply updates per shard, compute new content + hash for changed shards.
	var writes []ShardWrite
	var newShards []manifest.RefShard

	// Sort shard IDs for deterministic output across runs.
	changedIDs := make([]string, 0, len(updatesByShard))
	for sid := range updatesByShard {
		changedIDs = append(changedIDs, sid)
	}
	sort.Strings(changedIDs)

	for _, sid := range changedIDs {
		refs := loadedRefs[sid]
		for refname, oid := range updatesByShard[sid] {
			if oid == "" || oid == nullOIDHex {
				delete(refs, refname)
				continue
			}
			refs[refname] = oid
		}
		if len(refs) == 0 {
			// Shard emptied — drop it entirely (no "{}" object).
			continue
		}
		contents, err := marshalShardContent(refs)
		if err != nil {
			return Stage{}, fmt.Errorf("refstore: marshal shard %s: %w", sid, err)
		}
		hash := hashShardContent(contents)
		// Reuse existing key if hash matches existing shard (idempotent re-stage).
		var key string
		if existing, ok := existingByShard[sid]; ok && existing.Hash == hash {
			key = existing.Key
		} else {
			key = s.keys.RefShardKey(hash)
		}
		newShards = append(newShards, manifest.RefShard{
			Shard:    sid,
			Key:      key,
			Hash:     hash,
			RefCount: len(refs),
		})
		// Only emit a ShardWrite if the hash differs from any existing shard at this ID.
		if existing, ok := existingByShard[sid]; ok && existing.Hash == hash {
			// Unchanged — no PUT needed.
		} else {
			writes = append(writes, ShardWrite{
				Shard:    sid,
				Key:      key,
				Hash:     hash,
				Contents: contents,
			})
		}
	}

	// Forward untouched shards (those not in updatesByShard).
	for _, sh := range s.shards {
		if _, touched := updatesByShard[sh.Shard]; touched {
			continue
		}
		newShards = append(newShards, sh)
	}

	// Sort newShards by Shard ID for canonical body shape.
	sort.Slice(newShards, func(i, j int) bool { return newShards[i].Shard < newShards[j].Shard })

	return Stage{
		Mode:            ModeSharded,
		NewShardObjects: writes,
		NewRefShards:    newShards,
	}, nil
}
```

Add the missing `sort` import at the top of `sharded.go`. Also remove the now-unused `errors` import (the package no longer references `errors.ErrUnsupported`).

- [ ] **Step 4: Run all sharded tests; expect PASS.**

Run: `go test ./internal/repo/refstore/... -count=1 -v 2>&1 | tail -40`
Expected: every sharded test PASSES.

- [ ] **Step 5: Run the full sweep.**

Run: `go test ./... -count=1 2>&1 | grep -E "^(FAIL|---)" | head -10`
Expected: empty.

- [ ] **Step 6: Commit.**

```bash
git add internal/repo/refstore/sharded.go internal/repo/refstore/sharded_test.go
git commit -m "refstore: ShardedRefStore.Stage with delta builder (M12 Phase 3)"
```

---

### Task 3.2: Phase 3 boundary checkpoint

- [ ] **Step 1: Full test sweep + vet.**

```bash
go test ./... -count=1 2>&1 | grep -E "^(FAIL|---)" | head -10
go vet ./...
```

Expected: empty + clean.

- [ ] **Step 2: Two-stage review.**

Focus areas for reviewers:
- Determinism of `Stage` output (sort by shard ID before building writes).
- The "hash matches existing → no write" optimization (idempotent re-stage on CAS retry).
- Empty-shard handling (deletion that empties → drop from NewRefShards, no PUT).
- Defensive copy of `s.shards` for "no updates" pass-through.

- [ ] **Step 3: roborev-refine.**

- [ ] **Step 4: Proceed to Phase 4 (conformance suite).**
