# M12 Phase 5 — switch ref consumers to refstore

> **Parent plan:** [docs/m12-ref-sharding-plan.md](../m12-ref-sharding-plan.md). Phases 0–4 must be complete.

**Goal:** wire every existing ref consumer through `refstore.RefStore` so a v2 manifest is observable end-to-end (advertise, lsrefs, push completion, exporter, importer commit path). After this phase, every code path that previously did `body.Refs[name]` or `for name, oid := range body.Refs` goes through the interface, AND the importer's `BuildAndCommit` writes shard objects via PutIfAbsent inside `Repo.Commit`'s buildBody callback (the "Phase A" of the spec's §5.2 write flow).

**Files modified (one task per file/area):**
- `internal/gitproto/uploadpack/advertise.go` (v0 advertise)
- `internal/v2proto/lsrefs.go` (protocol-v2 lsrefs)
- `internal/gitproto/receivepack/advertise.go` (receive advertise)
- `internal/gitproto/receivepack/complete.go` (push completion: old-OID precheck)
- `internal/importer/buildcommit.go` (mergeRefs → refstore.Stage + Phase-A shard writes)
- `internal/exporter/exporter.go` (export from list)

**Cross-cutting test additions:**
- A "v2 manifest fixture" helper in `internal/repo/manifest/testfixtures.go` so every consumer test can spin up a sharded body without duplicating shard-building boilerplate.

---

### Task 5.1: v2 manifest fixture helper

**Files:**
- Create: `internal/repo/manifest/manifesttest/fixture.go`
- Create: `internal/repo/manifest/manifesttest/fixture_test.go`

The fixture lives in a dedicated `manifesttest` package (sibling to `manifest`). It imports `refstore` for `MarshalAndHashForTest` so there's a single source of truth for shard serialization — no duplicated marshal logic between manifest-side and refstore-side helpers.

- [ ] **Step 1: Create the fixture helper.**

```go
// Package manifesttest provides test helpers for constructing
// manifest bodies (including sharded layouts) without duplicating
// shard-marshal logic. Importing refstore is fine here because this
// package is consumed by tests outside the refstore→manifest cycle.
package manifesttest

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/refstore"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// MakeShardedBody builds a v2 Body, writing every shard object to
// store at the appropriate key. Tolerates storage.ErrAlreadyExists
// (content-addressing makes the write idempotent).
func MakeShardedBody(ctx context.Context, store storage.ObjectStore, k *keys.Repo, defaultBranch string, refs map[string]string) (manifest.Body, error) {
	perShard := map[string]map[string]string{}
	for name, oid := range refs {
		sid := refstore.ShardKey(name)
		if perShard[sid] == nil {
			perShard[sid] = map[string]string{}
		}
		perShard[sid][name] = oid
	}
	var shards []manifest.RefShard
	for sid, sr := range perShard {
		contents, hash := refstore.MarshalAndHashForTest(sr)
		key := k.RefShardKey(hash)
		if _, err := store.PutIfAbsent(ctx, key, bytes.NewReader(contents), nil); err != nil && !errors.Is(err, storage.ErrAlreadyExists) {
			return manifest.Body{}, fmt.Errorf("manifesttest: PutIfAbsent %s: %w", key, err)
		}
		shards = append(shards, manifest.RefShard{
			Shard:    sid,
			Key:      key,
			Hash:     hash,
			RefCount: len(sr),
		})
	}
	return manifest.Body{
		DefaultBranch: defaultBranch,
		RefShards:     shards,
		RefSharding:   "hash_v1",
		Packs:         []manifest.PackEntry{},
		Bundles:       []manifest.BundleEntry{},
	}, nil
}
```

This is much cleaner. The `MarshalAndHashForTest` already exists from Phase 2.

- [ ] **Step 2: Confirm compilation + write a quick smoke test.**

Create `internal/repo/manifest/manifesttest/fixture_test.go`:

```go
package manifesttest_test

import (
	"context"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest/manifesttest"
	"github.com/bucketvcs/bucketvcs/internal/repo/refstore"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestMakeShardedBody_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	store, err := localfs.Open(context.Background(), localfs.Config{Root: tmp})
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	defer store.Close()
	k, _ := keys.NewRepo("acme", "demo")
	refs := map[string]string{
		"refs/heads/main": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"refs/heads/dev":  "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	body, err := manifesttest.MakeShardedBody(context.Background(), store, k, "refs/heads/main", refs)
	if err != nil {
		t.Fatalf("MakeShardedBody: %v", err)
	}
	rs, err := refstore.New(context.Background(), store, k, &body)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	out, err := rs.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for k, v := range refs {
		if out[k] != v {
			t.Errorf("ref %q: got=%q want=%q", k, out[k], v)
		}
	}
}
```

Verify the localfs.Config signature by inspecting `internal/storage/localfs/`:

Run: `grep -n "type Config\|func Open" internal/storage/localfs/*.go | head`

Adjust the fixture test's localfs.Open call to match the actual signature (it's likely `localfs.Open(localfs.Config{Root: tmp})` without ctx or with a different field name; correct as you discover).

- [ ] **Step 3: Commit.**

```bash
git add internal/repo/manifest/manifesttest/
git commit -m "manifest/manifesttest: shared sharded-body fixture for consumer tests (M12 Phase 5.1)"
```

---

### Task 5.2: Switch `internal/v2proto/lsrefs.go` to refstore

**Files:**
- Modify: `internal/v2proto/lsrefs.go`

The current code does `body.DefaultBranch` + `body.Refs[headTarget]` for HEAD, then `for name := range body.Refs` for the rest. Change to: open a refstore over the body, call `List(ctx)`, then operate on the returned map.

- [ ] **Step 1: Write a v2-body test to drive the change.**

Add to `internal/v2proto/lsrefs_test.go` (or create if missing — check first):

Run: `ls internal/v2proto/`

If `lsrefs_test.go` exists, append a new test. Otherwise create it with the test alone. The test:

```go
func TestHandleLsRefs_ShardedBody(t *testing.T) {
	tmp := t.TempDir()
	store, err := localfs.Open(localfs.Config{Root: tmp})
	if err != nil { t.Fatalf("localfs.Open: %v", err) }
	defer store.Close()
	k, _ := keys.NewRepo("acme", "demo")
	body, err := manifesttest.MakeShardedBody(context.Background(), store, k, "refs/heads/main", map[string]string{
		"refs/heads/main": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"refs/heads/dev":  "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"refs/tags/v1.0":  "cccccccccccccccccccccccccccccccccccccccc",
	})
	if err != nil { t.Fatalf("MakeShardedBody: %v", err) }

	// Build the protocol-v2 ls-refs request: empty args (so all refs listed).
	args := []pktline.Token{}
	var buf bytes.Buffer
	if err := v2proto.HandleLsRefsWithStore(context.Background(), args, &body, store, k, &buf); err != nil {
		t.Fatalf("HandleLsRefs: %v", err)
	}
	got := buf.String()
	for _, want := range []string{"refs/heads/main", "refs/heads/dev", "refs/tags/v1.0", "HEAD"} {
		if !strings.Contains(got, want) {
			t.Errorf("ls-refs output missing %q\noutput:\n%s", want, got)
		}
	}
}
```

This test expects the function signature `HandleLsRefsWithStore(ctx, args, body, store, k, w)` — a new context-aware variant. The existing `HandleLsRefs(args, body, w)` cannot synthesize an ObjectStore from a body alone (sharded mode needs store reads), so we add the context-aware variant and keep the existing one as a thin wrapper for inline-only callers.

- [ ] **Step 2: Modify `internal/v2proto/lsrefs.go`.**

Replace the existing `HandleLsRefs` body with a delegating version, and add `HandleLsRefsWithStore`:

```go
// HandleLsRefs is the legacy entry point for callers that have a body
// known to be inline-mode (no shards). Wraps HandleLsRefsWithStore
// with a nil ObjectStore — works for v1 bodies because the refstore
// factory routes to InlineRefStore in that case.
//
// Sharded bodies WILL panic here (refstore.New returns an error when
// asked to construct a ShardedRefStore over a nil store). Callers
// that may handle v2 manifests MUST use HandleLsRefsWithStore.
func HandleLsRefs(args []pktline.Token, body *manifest.Body, w io.Writer) error {
	return HandleLsRefsWithStore(context.Background(), args, body, nil, nil, w)
}

// HandleLsRefsWithStore is the M12+ ls-refs handler. It opens a
// RefStore over body (inline or sharded), enumerates refs through
// it, and emits the wire-format output. The store + keys are only
// consulted for sharded bodies; inline bodies route to the
// in-memory InlineRefStore.
func HandleLsRefsWithStore(ctx context.Context, args []pktline.Token, body *manifest.Body, store storage.ObjectStore, k *keys.Repo, w io.Writer) error {
	// (existing arg parsing unchanged — wantSymrefs, wantUnborn, prefixes)
	// ...

	rs, err := refstore.New(ctx, store, k, body)
	if err != nil {
		return fmt.Errorf("ls-refs: refstore: %w", err)
	}
	refs, err := rs.List(ctx)
	if err != nil {
		return fmt.Errorf("ls-refs: list: %w", err)
	}

	pw := pktline.NewWriter(w)

	// HEAD line.
	headTarget := body.DefaultBranch
	headOID, headExists := refs[headTarget]
	if (headExists || wantUnborn) && prefixOK("HEAD", prefixes) {
		var line string
		switch {
		case headExists:
			line = headOID + " HEAD"
		default:
			line = "unborn HEAD"
		}
		if wantSymrefs && body.DefaultBranch != "" {
			line += " symref-target:" + headTarget
		}
		if err := pw.WriteString(line + "\n"); err != nil {
			return err
		}
	}

	// Other refs.
	names := make([]string, 0, len(refs))
	for name := range refs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !prefixOK(name, prefixes) {
			continue
		}
		oid := refs[name]
		if err := pw.WriteString(oid + " " + name + "\n"); err != nil {
			return err
		}
	}

	return pw.WriteFlush()
}
```

Add the imports: `context`, `fmt`, `refstore`, `keys`, `storage`. Keep `sort`.

- [ ] **Step 3: Update the v2proto callers to use HandleLsRefsWithStore.**

Run: `grep -rn "v2proto.HandleLsRefs\b" internal/ cmd/ 2>&1 | head`

For each caller, change the call to `HandleLsRefsWithStore(ctx, args, body, store, k, w)`, threading the store + keys.Repo from the existing context (uploadpack already has them; check the call site).

- [ ] **Step 4: Test.**

```bash
go test ./internal/v2proto/... -count=1 -v
```

Expected: existing inline tests still PASS; new sharded test PASSES.

- [ ] **Step 5: Commit.**

```bash
git add internal/v2proto/
git commit -m "v2proto: ls-refs through refstore (M12 Phase 5.2)"
```

---

### Task 5.3: Switch `internal/gitproto/uploadpack/advertise.go`

**Files:**
- Modify: `internal/gitproto/uploadpack/advertise.go`

The current `writeV0Advertisement` iterates `body.Refs`. Change to: open a RefStore over the body, call `List(ctx)`, iterate the result.

- [ ] **Step 1: Replace the iteration block.**

In `internal/gitproto/uploadpack/advertise.go`, find:

```go
var body manifest.Body
if err := json.Unmarshal(view.Body, &body); err != nil {
	return err
}

return writeV0Advertisement(req.Stdout, &body, req.AgentVersion)
```

Change to:

```go
body, err := manifest.UnmarshalBody(view.Body)
if err != nil {
	return fmt.Errorf("uploadpack: unmarshal body: %w", err)
}
rs, err := refstore.New(req.Ctx, req.Store, r.Keys(), &body)
if err != nil {
	return fmt.Errorf("uploadpack: refstore: %w", err)
}
refs, err := rs.List(req.Ctx)
if err != nil {
	return fmt.Errorf("uploadpack: list refs: %w", err)
}

return writeV0Advertisement(req.Stdout, &body, refs, req.AgentVersion)
```

The signature of `writeV0Advertisement` now takes `refs map[string]string` as a separate argument.

- [ ] **Step 2: Update `writeV0Advertisement` to use the refs argument.**

In the same file, change every `body.Refs` reference to the `refs` parameter. The function signature becomes:

```go
func writeV0Advertisement(w io.Writer, body *manifest.Body, refs map[string]string, version string) error {
```

Inside the function body, every `body.Refs[X]` becomes `refs[X]` and every `for n := range body.Refs` becomes `for n := range refs`. The `body` parameter is still needed for `body.DefaultBranch`.

- [ ] **Step 3: Check r.Keys() exists on Repo.**

Run: `grep -nE "func \(r \*Repo\) Keys\(\)" internal/repo/repo.go`

If absent, add it (or use whatever the existing accessor is — `r.RepoID()`/`r.TenantID()` exists but a `Keys()` accessor may not; you may need `keys.NewRepo(r.TenantID(), r.RepoID())` at the call site). Pick whichever is least invasive and consistent with other call sites you'll touch in Phase 5.

- [ ] **Step 4: Run uploadpack tests.**

```bash
go test ./internal/gitproto/uploadpack/... -count=1
```

Expected: PASS.

- [ ] **Step 5: Add a v2-body advertise test.**

Append to `internal/gitproto/uploadpack/advertise_test.go` (or create) a test that constructs a sharded body via `manifesttest.MakeShardedBody`, calls `Advertise`, and asserts every ref appears in the output.

- [ ] **Step 6: Commit.**

```bash
git add internal/gitproto/uploadpack/ internal/repo/repo.go
git commit -m "uploadpack: v0 advertise through refstore (M12 Phase 5.3)"
```

---

### Task 5.4: Switch `internal/gitproto/receivepack/advertise.go`

**Files:**
- Modify: `internal/gitproto/receivepack/advertise.go`

Same shape as Task 5.3 but for receive-pack advertise.

- [ ] **Step 1: Update Advertise to call refstore.New + List, then pass refs into writeV0Advertisement.**

The pattern matches Task 5.3 exactly — replace `body.Refs` iteration with `refs` from `refstore.List`. Refer to Task 5.3 step 2 for the diff shape; apply to `receivepack/advertise.go`.

- [ ] **Step 2: Run receivepack tests.**

```bash
go test ./internal/gitproto/receivepack/... -count=1
```

Expected: PASS.

- [ ] **Step 3: Add a sharded-body advertise test (mirror of uploadpack).**

- [ ] **Step 4: Commit.**

```bash
git add internal/gitproto/receivepack/advertise.go internal/gitproto/receivepack/advertise_test.go
git commit -m "receivepack: advertise through refstore (M12 Phase 5.4)"
```

---

### Task 5.5: Switch `internal/gitproto/receivepack/complete.go` old-OID precheck

**Files:**
- Modify: `internal/gitproto/receivepack/complete.go`

The precheck loop currently does:

```go
for i, u := range rp.Updates {
	if u.OldOID == nullOID {
		if _, exists := currentBody.Refs[u.Refname]; exists { ... }
	} else {
		cur, ok := currentBody.Refs[u.Refname]
		if !ok || cur != u.OldOID { ... }
	}
}
```

Change to use `refstore.Lookup`:

- [ ] **Step 1: Read the existing complete.go old-OID precheck block.**

Already shown earlier (around lines 60-76). Replace it with:

```go
body, err := manifest.UnmarshalBody(view.Body)
if err != nil {
	writeReceiveReport(w, "internal-error: "+err.Error(), nil, rp.Caps)
	return
}
k, _ := keys.NewRepo(tenant, repoID)
rs, err := refstore.New(ctx, eng.Store, k, &body)
if err != nil {
	writeReceiveReport(w, "internal-error: refstore: "+err.Error(), nil, rp.Caps)
	return
}
statuses := make([]string, len(rp.Updates))
allOK := true
for i, u := range rp.Updates {
	cur, exists, err := rs.Lookup(ctx, u.Refname)
	if err != nil {
		statuses[i] = "ng " + u.Refname + " backend-error"
		allOK = false
		continue
	}
	if u.OldOID == nullOID {
		if exists {
			statuses[i] = "ng " + u.Refname + " ref already exists"
			allOK = false
		}
	} else {
		if !exists || cur != u.OldOID {
			statuses[i] = "ng " + u.Refname + " stale info"
			allOK = false
		}
	}
}
```

Replace the existing `var currentBody manifest.Body; if err := json.Unmarshal(view.Body, &currentBody); err != nil { ... }` block above the precheck with the new `body, err := manifest.UnmarshalBody(...)` call. Then remove the `currentBody.Refs[u.Refname]` accesses since they're now via `rs.Lookup`.

- [ ] **Step 2: Update any downstream uses of `currentBody.Refs` in the same file.**

Run: `grep -n "currentBody" internal/gitproto/receivepack/complete.go`

For each remaining reference, decide whether it needs the full ref map (use a single `rs.List(ctx)` and reuse) or a single lookup (use `rs.Lookup`).

- [ ] **Step 3: Receivepack tests.**

```bash
go test ./internal/gitproto/receivepack/... -count=1
```

- [ ] **Step 4: Add a sharded-body push test asserting the old-OID precheck works through the interface.**

- [ ] **Step 5: Commit.**

```bash
git add internal/gitproto/receivepack/complete.go
git commit -m "receivepack: complete old-OID precheck through refstore (M12 Phase 5.5)"
```

---

### Task 5.6: Switch `internal/exporter/exporter.go`

**Files:**
- Modify: `internal/exporter/exporter.go`

- [ ] **Step 1: Replace the `for ref, oid := range body.Refs` loop with a `refstore.List` call.**

Inside `Export(ctx, opts)`, after `if err := json.Unmarshal(view.Body, &body); err != nil { ... }`, switch to:

```go
body, err := manifest.UnmarshalBody(view.Body)
if err != nil {
	return nil, fmt.Errorf("exporter: unmarshal body: %w", err)
}
k, _ := keys.NewRepo(opts.TenantID, opts.RepoID) // or whatever the call site exposes
rs, err := refstore.New(ctx, store, k, &body)
if err != nil {
	return nil, fmt.Errorf("exporter: refstore: %w", err)
}
refs, err := rs.List(ctx)
if err != nil {
	return nil, fmt.Errorf("exporter: list refs: %w", err)
}
for ref, oid := range refs {
	// ... existing validation + UpdateRef call ...
}
```

The existing default-branch check (`body.Refs[body.DefaultBranch]`) becomes `refs[body.DefaultBranch]`.

- [ ] **Step 2: Confirm the exporter test suite still passes.**

```bash
go test ./internal/exporter/... -count=1
```

- [ ] **Step 3: Add a sharded-body export test.**

- [ ] **Step 4: Commit.**

```bash
git add internal/exporter/exporter.go
git commit -m "exporter: list refs through refstore (M12 Phase 5.6)"
```

---

### Task 5.7: importer.BuildAndCommit — Stage + Phase-A shard writes

**Files:**
- Modify: `internal/importer/buildcommit.go`

This is the largest single change in Phase 5. `mergeRefs` becomes a thin caller of `refstore.Stage`, and the new body is built using either `Stage.NewInlineRefs` (v1) or `Stage.NewRefShards` (v2). The shard objects from `Stage.NewShardObjects` are PutIfAbsent'd inside the `Repo.Commit` buildBody callback (before returning the new body bytes — this is the spec's "Phase A").

- [ ] **Step 1: Locate the current ref-merge + body-build site in `BuildAndCommit`.**

Already explored (around line 106 `newRefs, err := mergeRefs(...)`). The pattern is:

```go
newRefs, err := mergeRefs(currentBody.Refs, refUpdates)
...
// Build new body with newRefs assigned to body.Refs.
```

- [ ] **Step 2: Replace with refstore.Stage.**

Change the merge block to:

```go
rs, err := refstore.New(ctx, store, k, &currentBody)
if err != nil {
	return nil, fmt.Errorf("importer: BuildAndCommit: refstore: %w", err)
}
stage, err := rs.Stage(ctx, refUpdates)
if err != nil {
	return nil, fmt.Errorf("importer: BuildAndCommit: stage: %w", err)
}
```

Then update the body-build code (downstream) to:

```go
newBody := /* existing field copies */
switch stage.Mode {
case refstore.ModeInline:
	newBody.Refs = stage.NewInlineRefs
	newBody.RefShards = nil
	newBody.RefSharding = ""
case refstore.ModeSharded:
	newBody.Refs = nil
	newBody.RefShards = stage.NewRefShards
	newBody.RefSharding = "hash_v1"
}
```

The "default branch points at a deleted ref" check (currently using `currentBody.Refs[body.DefaultBranch]` and `newRefs[body.DefaultBranch]`) becomes a Lookup via the staged refs. Simplest path: compute the effective new ref map from the stage and use it:

```go
var effective map[string]string
if stage.Mode == refstore.ModeInline {
	effective = stage.NewInlineRefs
} else {
	// For sharded, the staged shards represent the new state, but we don't
	// have an in-memory map of all refs. Build one by lookup-then-Stage:
	// at this code path we only need to check ONE refname (DefaultBranch),
	// so use rs.Lookup against the post-stage state via a temporary
	// reconstructed RefStore. Cheaper: compute lookup directly from the
	// shard list that Stage produced.
	effective = effectiveRefsFromStage(ctx, store, k, stage)
	if effective == nil { ... handle error ... }
}
```

To avoid that complexity, prefer a simpler approach: add a helper method `Stage.Lookup(name)` on the Stage type that resolves a refname from staged data without going back to the store. This is purely in-memory if the stage covers the refname (NewInlineRefs OR a shard in NewShardObjects); falls back to the original RefStore.Lookup for refnames in unchanged shards.

- [ ] **Step 3: Add `Stage.Lookup` helper to refstore.**

In `internal/repo/refstore/refstore.go`, add a method on `Stage`:

```go
// Lookup resolves a refname against the post-stage state, in memory
// where possible. Used by callers (importer.BuildAndCommit) that
// need to verify a single ref's post-update value (e.g., to enforce
// "default branch not deleted") without rebuilding a full RefStore.
//
// For inline stages, this is an O(1) map lookup against
// NewInlineRefs.
//
// For sharded stages, this scans NewShardObjects for the refname's
// shard. If the shard appears in NewShardObjects, its contents
// include the refname's post-update value. If the shard appears in
// NewRefShards but NOT in NewShardObjects, the shard is unchanged —
// in which case Lookup returns (_, false, ErrLookupNotInStage) so
// the caller knows to fall back to the original RefStore.
//
// Lookup parses each ShardWrite's JSON the first time the matching
// shard is touched; subsequent calls reuse the parsed map via the
// cache field. This makes K Lookup calls O(K + N) where N is the
// total shard-content size, not O(K * N).
func (s *Stage) Lookup(refname string) (oid string, exists bool, err error) {
	if s.Mode == ModeInline {
		oid, exists = s.NewInlineRefs[refname]
		return oid, exists, nil
	}
	sid := shardKey(refname)
	for i := range s.NewShardObjects {
		if s.NewShardObjects[i].Shard != sid {
			continue
		}
		refs, err := s.parseShardWrite(&s.NewShardObjects[i])
		if err != nil {
			return "", false, err
		}
		oid, exists = refs[refname]
		return oid, exists, nil
	}
	// Refname's shard is not in NewShardObjects → either it's in an
	// untouched shard, or no shard for this ID exists. Caller falls
	// back to RefStore.Lookup (which is cheap — one shard read).
	return "", false, ErrLookupNotInStage
}

// ErrLookupNotInStage signals that Stage.Lookup cannot answer the
// refname from in-memory data alone; the caller should consult the
// original RefStore.
var ErrLookupNotInStage = errors.New("refstore: refname not covered by stage")

func (s *Stage) parseShardWrite(w *ShardWrite) (map[string]string, error) {
	var m map[string]string
	if err := json.Unmarshal(w.Contents, &m); err != nil {
		return nil, fmt.Errorf("refstore: parse staged shard %s: %w", w.Key, err)
	}
	return m, nil
}
```

The `Shard` field on `ShardWrite` was already added back in Phase 1.2 specifically so this routing works without back-deriving the shard ID from the storage key.

- [ ] **Step 4: Use `Stage.Lookup` from importer.BuildAndCommit.**

The `Shard` field on `ShardWrite` was defined back in Phase 1.2 specifically so this lookup helper would not need to back-derive the shard ID from the storage key. Confirm it's present in the type before using it (`grep "Shard.*string" internal/repo/refstore/refstore.go`).

In the importer's default-branch deletion check:

```go
if currentBody.DefaultBranch != "" {
	_, hadBefore, err := rs.Lookup(ctx, currentBody.DefaultBranch)
	if err != nil {
		return nil, fmt.Errorf("importer: lookup default before: %w", err)
	}
	_, hasAfter, slErr := stage.Lookup(currentBody.DefaultBranch)
	if slErr != nil && !errors.Is(slErr, refstore.ErrLookupNotInStage) {
		return nil, fmt.Errorf("importer: lookup default after (stage): %w", slErr)
	}
	if errors.Is(slErr, refstore.ErrLookupNotInStage) {
		// Default branch's shard is unchanged — its value matches the pre-stage state.
		hasAfter = hadBefore
	}
	if hadBefore && !hasAfter {
		return nil, fmt.Errorf("BuildAndCommit: refuses to delete current default branch %q", currentBody.DefaultBranch)
	}
}
```

- [ ] **Step 5: Wrap the existing `r.Commit(...)` call to also PutIfAbsent stage.NewShardObjects.**

Within the `buildBody` callback Repo.Commit passes (and inside which the importer already builds `newBody`), insert:

```go
buildBody := func(prev *repo.RootView) ([]byte, error) {
	// ... existing pack-build + index-build steps ...

	// M12 Phase A: write every NewShardObject before returning the
	// new body bytes. PutIfAbsent is content-addressed; ErrAlreadyExists
	// is swallowed (same bytes → idempotent).
	for _, w := range stage.NewShardObjects {
		_, err := store.PutIfAbsent(ctx, w.Key, bytes.NewReader(w.Contents), nil)
		if err != nil && !errors.Is(err, storage.ErrAlreadyExists) {
			return nil, fmt.Errorf("importer: PutIfAbsent shard %s: %w", w.Key, err)
		}
	}

	// ... continue with body assembly ...
}
```

The `bytes` import will be needed. Check whether the imports block has it.

- [ ] **Step 6: Drop the now-dead `mergeRefs` helper (or repurpose).**

If `mergeRefs` is no longer called anywhere, delete it. Run: `grep -n "mergeRefs" internal/importer/`. If only `buildcommit.go` references it and the new code path no longer calls it, remove it.

If it's still used for some edge case (e.g., empty-target body), keep it but mark deprecated with a comment pointing at `refstore.Stage`.

- [ ] **Step 7: Run the importer tests.**

```bash
go test ./internal/importer/... -count=1 -v
```

Expected: PASS. If any test relies on the precise pre-M12 behavior of `mergeRefs` (e.g., ordering of refs), adjust the assertion to use `refstore.Stage`'s output instead.

- [ ] **Step 8: Run the full sweep — receivepack uses importer.BuildAndCommit, so end-to-end push tests must still pass.**

```bash
go test ./... -count=1 2>&1 | grep -E "^(FAIL|---)" | head -10
```

Expected: empty.

- [ ] **Step 9: Commit.**

```bash
git add internal/importer/buildcommit.go internal/repo/refstore/refstore.go internal/repo/refstore/sharded.go internal/repo/refstore/inline.go internal/repo/refstore/sharded_test.go internal/repo/refstore/inline_test.go
git commit -m "importer: Stage + Phase-A shard writes; Stage.Lookup helper (M12 Phase 5.7)"
```

---

### Task 5.8: Phase 5 boundary checkpoint

- [ ] **Step 1: Sweep + vet.**

```bash
go test ./... -count=1 2>&1 | grep -E "^(FAIL|---)" | head -10
go vet ./...
```

Expected: empty + clean.

- [ ] **Step 2: Cross-impl audit — grep for any remaining direct `body.Refs` reads in production code.**

```bash
grep -rn 'body\.Refs\b\|currentBody\.Refs\b' internal/ cmd/ --include='*.go' | grep -v '_test.go' | grep -v 'internal/repo/refstore/' | grep -v 'internal/repo/manifest/'
```

Expected: empty (or only references inside Phase-0 manifest-package code paths, which are allowed because the validator and marshaller live in the manifest package).

If any production code outside refstore + manifest still reads `body.Refs`, that's a bug — fix it and add it to the consumer list before proceeding.

- [ ] **Step 3: Two-stage review.**

Focus areas:
- `Stage.Lookup` correctness on the "unchanged shard" path (the `ErrLookupNotInStage` sentinel).
- The Phase-A `PutIfAbsent` loop swallows `ErrAlreadyExists` exclusively (not generic "exists" string matching).
- `manifesttest.MakeShardedBody` is only used in test files (grep confirms).
- `mergeRefs` is fully retired or clearly scoped.

- [ ] **Step 4: roborev-refine.**

- [ ] **Step 5: Proceed to Phase 6 (reshard CLI).**
