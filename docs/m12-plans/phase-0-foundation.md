# M12 Phase 0 — Foundation

> **Parent plan:** [docs/m12-ref-sharding-plan.md](../m12-ref-sharding-plan.md). Read it before starting this phase.

**Goal of this phase:** add the schema and validation primitives M12 will build on top of, without yet introducing any sharded behavior. After this phase the codebase still produces only v1 manifests, but the `Body` struct knows about `RefShards`, the `UnmarshalBody` validator rejects hybrids, and `CurrentSchemaVersion` is 2 (so a future build emitting v2 will be accepted while pre-M12 builds reject).

**Why this phase is first:** every subsequent phase depends on the `RefShard` type, the `UnmarshalBody` validator, or the bumped schema version. Doing it standalone keeps the diff small and reviewable.

**Files touched in this phase:**

- Create: `internal/repo/manifest/unmarshal.go`, `internal/repo/manifest/unmarshal_test.go`
- Modify: `internal/repo/repoerrs/errors.go`, `internal/repo/manifest/body.go`, `internal/repo/manifest/schema.go`, `internal/repo/manifest/schema_test.go`, `internal/repo/manifest/body_test.go`

---

### Task 0.1: Add `ErrInvalidManifest` sentinel

**Files:**
- Modify: `internal/repo/repoerrs/errors.go`

- [ ] **Step 1: Read the current error block to find the right insertion point.**

Run: `sed -n '15,30p' internal/repo/repoerrs/errors.go`

Expected: the `var ( ... )` block listing `ErrRepoExists`, `ErrRepoNotFound`, `ErrUnsupportedSchema`, `ErrCallbackFailed`, `ErrInvalidTenantID`, `ErrInvalidRepoID`.

- [ ] **Step 2: Add `ErrInvalidManifest` to the block.**

Edit `internal/repo/repoerrs/errors.go` to add this entry inside the existing `var ( ... )` block, after `ErrInvalidRepoID`:

```go
	// ErrInvalidManifest signals a manifest body whose structure violates
	// an invariant the body schema enforces (hybrid v1/v2 ref state,
	// unknown ref-sharding strategy, malformed shard hash, etc.). M12+.
	ErrInvalidManifest = errors.New("repo: manifest body invariant violation")
```

- [ ] **Step 3: Confirm the file still compiles.**

Run: `go build ./internal/repo/repoerrs/...`
Expected: no output (success).

- [ ] **Step 4: Commit.**

```bash
git add internal/repo/repoerrs/errors.go
git commit -m "repoerrs: add ErrInvalidManifest sentinel for M12"
```

---

### Task 0.2: Add `RefShard` type + body fields

**Files:**
- Modify: `internal/repo/manifest/body.go`

- [ ] **Step 1: Add the `RefShard` struct.**

Open `internal/repo/manifest/body.go`. After the `BundleEntry` block (currently ending around line 60), before the `PackEntry` block, insert:

```go
// RefShard references one immutable ref-shard object under
// manifest/ref-shards/<hash>.json. Present in v2 manifests only;
// absent (nil slice) in v1.
//
// Content-addressing: Key includes Hash, so a PutIfAbsent on Key is
// idempotent — two writers minting the same shard contents collapse
// to a single object.
type RefShard struct {
	// Shard is the 2-hex shard identifier ("00".."ff"), the first byte
	// of sha256(refname) for every ref this shard contains.
	Shard string `json:"shard"`

	// Key is the full object-store key for this shard
	// (tenants/<t>/repos/<r>/manifest/ref-shards/<hash>.json).
	// Already includes the content hash so it round-trips through GC.
	Key string `json:"key"`

	// Hash is the sha256 of the shard's canonical JSON, formatted
	// "sha256-<64-lowercase-hex>". Verified at read time.
	Hash string `json:"hash"`

	// RefCount is informational (paper-trail; not load-bearing). Used
	// by operators to gauge shard distribution after a reshard.
	RefCount int `json:"ref_count"`
}
```

- [ ] **Step 2: Update the `Body` struct to add the M12 fields.**

In the same file, modify the `Body` struct to look exactly like:

```go
type Body struct {
	DefaultBranch string            `json:"default_branch"`
	Refs          map[string]string `json:"refs,omitempty"`         // v1; mutually exclusive with RefShards
	RefShards     []RefShard        `json:"ref_shards,omitempty"`   // v2; mutually exclusive with Refs
	RefSharding   string            `json:"ref_sharding,omitempty"` // v2; "hash_v1" today
	Packs         []PackEntry       `json:"packs"`
	Indexes       Indexes           `json:"indexes"`
	Bundles       []BundleEntry     `json:"bundles"`
}
```

The change vs. before: `Refs` gains `,omitempty`; two new fields `RefShards` and `RefSharding` added.

- [ ] **Step 3: Update `MarshalBody` to NOT normalize nil `Refs` to empty map when the body is v2.**

The current `MarshalBody` always normalizes `b.Refs == nil` to `map[string]string{}`. With `omitempty` this would force `"refs":{}` on the wire even for v2 bodies, defeating the purpose of `omitempty`.

Modify the top of `MarshalBody` to:

```go
func MarshalBody(b Body) ([]byte, error) {
	// Only normalize nil Refs to empty map for v1 bodies (RefShards == nil).
	// For v2 bodies (RefShards != nil), leave Refs nil so the omitempty
	// JSON tag drops it from the wire form.
	if b.Refs == nil && b.RefShards == nil {
		b.Refs = map[string]string{}
	}
	if b.Packs == nil {
		b.Packs = []PackEntry{}
	}
	if b.Bundles == nil {
		b.Bundles = []BundleEntry{}
	}
	if b.Indexes.Reachability != nil && b.Indexes.Reachability.Deltas == nil {
		rcopy := *b.Indexes.Reachability
		rcopy.Deltas = []IndexRef{}
		b.Indexes.Reachability = &rcopy
	}
	out, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("manifest: marshal body: %w", err)
	}
	return out, nil
}
```

- [ ] **Step 4: Confirm compilation.**

Run: `go build ./internal/repo/manifest/...`
Expected: no output.

- [ ] **Step 5: Run existing manifest tests.**

Run: `go test ./internal/repo/manifest/... -run TestBody -count=1`
Expected: PASS (existing v1 round-trip tests should still pass with the omitempty addition — the on-wire shape for v1 bodies is unchanged because `Refs` is always populated in those tests).

- [ ] **Step 6: Commit.**

```bash
git add internal/repo/manifest/body.go
git commit -m "manifest: add RefShard type + RefShards/RefSharding body fields (M12 schema)"
```

---

### Task 0.3: Add `UnmarshalBody` validator

**Files:**
- Create: `internal/repo/manifest/unmarshal.go`
- Create: `internal/repo/manifest/unmarshal_test.go`

- [ ] **Step 1: Write the failing tests first.**

Create `internal/repo/manifest/unmarshal_test.go`:

```go
package manifest_test

import (
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/repoerrs"
)

func TestUnmarshalBody_V1_RoundTrip(t *testing.T) {
	raw := []byte(`{"default_branch":"refs/heads/main","refs":{"refs/heads/main":"abc"},"packs":[],"indexes":{},"bundles":[]}`)
	b, err := manifest.UnmarshalBody(raw)
	if err != nil {
		t.Fatalf("UnmarshalBody: %v", err)
	}
	if b.DefaultBranch != "refs/heads/main" {
		t.Errorf("DefaultBranch = %q", b.DefaultBranch)
	}
	if got := b.Refs["refs/heads/main"]; got != "abc" {
		t.Errorf("Refs[main] = %q want abc", got)
	}
	if len(b.RefShards) != 0 {
		t.Errorf("RefShards = %v want empty", b.RefShards)
	}
}

func TestUnmarshalBody_V2_RoundTrip(t *testing.T) {
	raw := []byte(`{
  "default_branch": "refs/heads/main",
  "ref_shards": [
    {"shard":"00","key":"tenants/t/repos/r/manifest/ref-shards/sha256-aa.json","hash":"sha256-aa00000000000000000000000000000000000000000000000000000000000000","ref_count":1}
  ],
  "ref_sharding": "hash_v1",
  "packs": [],
  "indexes": {},
  "bundles": []
}`)
	b, err := manifest.UnmarshalBody(raw)
	if err != nil {
		t.Fatalf("UnmarshalBody: %v", err)
	}
	if b.RefSharding != "hash_v1" {
		t.Errorf("RefSharding = %q", b.RefSharding)
	}
	if len(b.RefShards) != 1 || b.RefShards[0].Shard != "00" {
		t.Errorf("RefShards = %+v", b.RefShards)
	}
	if len(b.Refs) != 0 {
		t.Errorf("Refs = %v want empty", b.Refs)
	}
}

func TestUnmarshalBody_RejectsHybrid(t *testing.T) {
	raw := []byte(`{"default_branch":"refs/heads/main","refs":{"refs/heads/main":"abc"},"ref_shards":[{"shard":"00","key":"k","hash":"sha256-aa00000000000000000000000000000000000000000000000000000000000000","ref_count":0}],"ref_sharding":"hash_v1","packs":[],"indexes":{},"bundles":[]}`)
	_, err := manifest.UnmarshalBody(raw)
	if !errors.Is(err, repoerrs.ErrInvalidManifest) {
		t.Fatalf("err = %v, want ErrInvalidManifest", err)
	}
}

func TestUnmarshalBody_RejectsUnknownSharding(t *testing.T) {
	raw := []byte(`{"default_branch":"refs/heads/main","ref_shards":[{"shard":"00","key":"k","hash":"sha256-aa00000000000000000000000000000000000000000000000000000000000000","ref_count":0}],"ref_sharding":"namespace_hash_v1","packs":[],"indexes":{},"bundles":[]}`)
	_, err := manifest.UnmarshalBody(raw)
	if !errors.Is(err, repoerrs.ErrInvalidManifest) {
		t.Fatalf("err = %v, want ErrInvalidManifest", err)
	}
}

func TestUnmarshalBody_RejectsBadShardID(t *testing.T) {
	for _, badShard := range []string{"", "0", "abc", "0g", "FF"} {
		raw := []byte(`{"default_branch":"refs/heads/main","ref_shards":[{"shard":"` + badShard + `","key":"k","hash":"sha256-aa00000000000000000000000000000000000000000000000000000000000000","ref_count":0}],"ref_sharding":"hash_v1","packs":[],"indexes":{},"bundles":[]}`)
		_, err := manifest.UnmarshalBody(raw)
		if !errors.Is(err, repoerrs.ErrInvalidManifest) {
			t.Errorf("shard=%q: err = %v, want ErrInvalidManifest", badShard, err)
		}
	}
}

func TestUnmarshalBody_RejectsDuplicateShard(t *testing.T) {
	raw := []byte(`{"default_branch":"refs/heads/main","ref_shards":[
{"shard":"00","key":"k1","hash":"sha256-aa00000000000000000000000000000000000000000000000000000000000000","ref_count":0},
{"shard":"00","key":"k2","hash":"sha256-bb00000000000000000000000000000000000000000000000000000000000000","ref_count":0}
],"ref_sharding":"hash_v1","packs":[],"indexes":{},"bundles":[]}`)
	_, err := manifest.UnmarshalBody(raw)
	if !errors.Is(err, repoerrs.ErrInvalidManifest) {
		t.Fatalf("err = %v, want ErrInvalidManifest", err)
	}
}

func TestUnmarshalBody_RejectsBadHash(t *testing.T) {
	// Wrong prefix.
	for _, bad := range []string{"sha1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "sha256-XYZ", "sha256-", "deadbeef"} {
		raw := []byte(`{"default_branch":"refs/heads/main","ref_shards":[{"shard":"00","key":"k","hash":"` + bad + `","ref_count":0}],"ref_sharding":"hash_v1","packs":[],"indexes":{},"bundles":[]}`)
		_, err := manifest.UnmarshalBody(raw)
		if !errors.Is(err, repoerrs.ErrInvalidManifest) {
			t.Errorf("hash=%q: err = %v, want ErrInvalidManifest", bad, err)
		}
	}
}

func TestUnmarshalBody_RejectsV2WithoutSharding(t *testing.T) {
	// RefShards populated but RefSharding empty.
	raw := []byte(`{"default_branch":"refs/heads/main","ref_shards":[{"shard":"00","key":"k","hash":"sha256-aa00000000000000000000000000000000000000000000000000000000000000","ref_count":0}],"packs":[],"indexes":{},"bundles":[]}`)
	_, err := manifest.UnmarshalBody(raw)
	if !errors.Is(err, repoerrs.ErrInvalidManifest) {
		t.Fatalf("err = %v, want ErrInvalidManifest", err)
	}
}

func TestUnmarshalBody_RejectsShardingWithoutShards(t *testing.T) {
	// RefSharding set but no RefShards. Treated as malformed.
	raw := []byte(`{"default_branch":"refs/heads/main","ref_sharding":"hash_v1","packs":[],"indexes":{},"bundles":[]}`)
	_, err := manifest.UnmarshalBody(raw)
	if !errors.Is(err, repoerrs.ErrInvalidManifest) {
		t.Fatalf("err = %v, want ErrInvalidManifest", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they FAIL.**

Run: `go test ./internal/repo/manifest/... -run TestUnmarshalBody -count=1`
Expected: build failure ("UnmarshalBody not declared") OR FAIL with all 8 subtests missing.

- [ ] **Step 3: Create the `UnmarshalBody` implementation.**

Create `internal/repo/manifest/unmarshal.go`:

```go
package manifest

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/repo/repoerrs"
)

// SupportedRefShardingStrategies lists the ref_sharding strategy strings
// this build recognizes. M12 ships only "hash_v1". Future strategies
// (e.g., "namespace_hash_v1") extend this list, gated by the
// ref_sharding string at read time so old binaries fail-closed.
var SupportedRefShardingStrategies = map[string]struct{}{
	"hash_v1": {},
}

// UnmarshalBody parses a root-manifest body, then enforces M12's
// structural invariants:
//
//   - Refs and RefShards are mutually exclusive (no hybrid v1/v2 state).
//   - A v2 body (RefShards non-empty) must have RefSharding set to a
//     supported strategy string.
//   - A v1 body (Refs populated or both empty) must have RefSharding == "".
//   - Each RefShard.Shard is 2 lowercase hex ("00".."ff"); shard IDs
//     are unique within the slice.
//   - Each RefShard.Hash matches "sha256-" + 64 lowercase hex.
//
// Returns repoerrs.ErrInvalidManifest (wrapped with detail) for any
// violation. Returns a json.UnmarshalTypeError-class error for raw
// JSON parse failures; callers MAY but need not wrap.
//
// UnmarshalBody is the canonical body-parse entry point. Consumers
// SHOULD use it in preference to json.Unmarshal(view.Body, &body) so
// invariant violations are caught at the read boundary.
func UnmarshalBody(raw []byte) (Body, error) {
	var b Body
	if err := json.Unmarshal(raw, &b); err != nil {
		return Body{}, fmt.Errorf("manifest: unmarshal body: %w", err)
	}
	if err := validateBody(&b); err != nil {
		return Body{}, err
	}
	return b, nil
}

func validateBody(b *Body) error {
	hasRefs := len(b.Refs) > 0
	hasShards := len(b.RefShards) > 0
	hasShardingTag := b.RefSharding != ""

	// Hybrid state.
	if hasRefs && hasShards {
		return fmt.Errorf("%w: hybrid v1/v2 ref state (both refs and ref_shards populated)", repoerrs.ErrInvalidManifest)
	}

	if hasShards {
		// v2 path.
		if !hasShardingTag {
			return fmt.Errorf("%w: v2 body has ref_shards but ref_sharding is empty", repoerrs.ErrInvalidManifest)
		}
		if _, ok := SupportedRefShardingStrategies[b.RefSharding]; !ok {
			return fmt.Errorf("%w: unsupported ref_sharding strategy %q", repoerrs.ErrInvalidManifest, b.RefSharding)
		}
		if err := validateRefShards(b.RefShards); err != nil {
			return err
		}
	} else {
		// v1 path (or empty repo). RefSharding without RefShards is malformed.
		if hasShardingTag {
			return fmt.Errorf("%w: ref_sharding=%q set without ref_shards", repoerrs.ErrInvalidManifest, b.RefSharding)
		}
	}
	return nil
}

func validateRefShards(shards []RefShard) error {
	seen := make(map[string]struct{}, len(shards))
	for i, s := range shards {
		if !isShardID(s.Shard) {
			return fmt.Errorf("%w: ref_shards[%d].shard = %q (want 2 lowercase hex)", repoerrs.ErrInvalidManifest, i, s.Shard)
		}
		if _, dup := seen[s.Shard]; dup {
			return fmt.Errorf("%w: ref_shards[%d].shard = %q is duplicated", repoerrs.ErrInvalidManifest, i, s.Shard)
		}
		seen[s.Shard] = struct{}{}
		if !isShardHash(s.Hash) {
			return fmt.Errorf("%w: ref_shards[%d].hash = %q (want sha256-<64hex>)", repoerrs.ErrInvalidManifest, i, s.Hash)
		}
	}
	return nil
}

// isShardID reports whether s is exactly two lowercase hex characters.
func isShardID(s string) bool {
	if len(s) != 2 {
		return false
	}
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 1 {
		return false
	}
	// Disallow uppercase. hex.DecodeString accepts both cases; we want
	// lowercase only so shard IDs are canonical.
	if s != strings.ToLower(s) {
		return false
	}
	return true
}

// isShardHash reports whether h is "sha256-" + 64 lowercase hex.
func isShardHash(h string) bool {
	const prefix = "sha256-"
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	rest := h[len(prefix):]
	if len(rest) != 64 {
		return false
	}
	if rest != strings.ToLower(rest) {
		return false
	}
	if _, err := hex.DecodeString(rest); err != nil {
		return false
	}
	return true
}
```

- [ ] **Step 4: Run the tests; expect PASS.**

Run: `go test ./internal/repo/manifest/... -run TestUnmarshalBody -count=1 -v`
Expected: all 8 tests PASS.

- [ ] **Step 5: Run the full manifest package to confirm no regression.**

Run: `go test ./internal/repo/manifest/... -count=1`
Expected: PASS.

- [ ] **Step 6: Commit.**

```bash
git add internal/repo/manifest/unmarshal.go internal/repo/manifest/unmarshal_test.go
git commit -m "manifest: UnmarshalBody validator with M12 invariants"
```

---

### Task 0.4: Bump `CurrentSchemaVersion` to 2

**Files:**
- Modify: `internal/repo/manifest/schema.go`
- Modify: `internal/repo/manifest/schema_test.go`

- [ ] **Step 1: Bump the constant.**

In `internal/repo/manifest/schema.go`, change line 14 from:

```go
	CurrentSchemaVersion = 1
```

to:

```go
	CurrentSchemaVersion = 2
```

Also update the comment immediately above it (currently around line 11-13) to mention M12:

```go
	// CurrentSchemaVersion is the schema_version this build emits and
	// the highest schema_version this build accepts. Per §43.7 the gate
	// is asymmetric: future versions fail closed. M12 bumped 1 → 2 to
	// introduce sharded refs (Body.RefShards).
	CurrentSchemaVersion = 2
```

- [ ] **Step 2: Read the existing schema test to see what to update.**

Run: `sed -n '24,46p' internal/repo/manifest/schema_test.go`

Expected: the `TestSchemaGate_RejectsFutureSchemaVersion` test that uses `SchemaVersion: 2`.

- [ ] **Step 3: Update the rejection test to use SchemaVersion: 3 (the new "future").**

In `internal/repo/manifest/schema_test.go`, find `TestSchemaGate_RejectsFutureSchemaVersion` and update both occurrences of `SchemaVersion: 2` to `SchemaVersion: 3`. Also rename comments or local variables that refer to "v2" as a future version. The test should now look like:

```go
func TestSchemaGate_RejectsFutureSchemaVersion(t *testing.T) {
	h := manifest.RootHeader{SchemaVersion: 3, MinReaderVersion: "0.1.0"}
	if err := manifest.SchemaGate(h); !errors.Is(err, repo.ErrUnsupportedSchema) {
		t.Errorf("want ErrUnsupportedSchema, got %v", err)
	}
}
```

- [ ] **Step 4: Add a new acceptance test for v2.**

Add this new test in `internal/repo/manifest/schema_test.go`, near the existing happy-path cases (which were `SchemaVersion: 1`):

```go
func TestSchemaGate_AcceptsV2(t *testing.T) {
	h := manifest.RootHeader{SchemaVersion: 2, MinReaderVersion: ""}
	if err := manifest.SchemaGate(h); err != nil {
		t.Errorf("want nil, got %v", err)
	}
}
```

- [ ] **Step 5: Run the schema tests.**

Run: `go test ./internal/repo/manifest/... -run TestSchemaGate -count=1 -v`
Expected: all PASS, including the new `TestSchemaGate_AcceptsV2`.

- [ ] **Step 6: Run the full test sweep.**

Run: `go test ./... -count=1 2>&1 | tail -20`
Expected: ALL PASS. Critically: existing tests that create manifests via `repo.Create` will now emit `SchemaVersion: 2` (because the constant changed). The body shape stays v1 (Refs populated, RefShards nil), which is valid v2 — sharded mode is opt-in. SchemaGate accepts both. Validators in UnmarshalBody accept v1-shaped bodies regardless of header SchemaVersion (the body invariants only care about field state, not the header version). Result: no regression.

If a test fails because it hard-codes `SchemaVersion: 1` in an assertion, update that one assertion to expect `2`.

- [ ] **Step 7: Commit.**

```bash
git add internal/repo/manifest/schema.go internal/repo/manifest/schema_test.go
git commit -m "manifest: bump CurrentSchemaVersion 1→2 for M12 sharded refs"
```

---

### Task 0.5: Phase 0 boundary checkpoint

- [ ] **Step 1: Run the full test sweep + vet to confirm a clean phase boundary.**

```bash
go test ./... -count=1 2>&1 | grep -E "^(FAIL|ok|---)" | tail -50
go vet ./...
```

Expected: every line begins with `ok`. Zero FAILs. `go vet` clean.

- [ ] **Step 2: Verify the four Phase-0 commits land cleanly.**

Run: `git log --oneline main..HEAD`
Expected output (commit SHAs differ):

```
<sha> manifest: bump CurrentSchemaVersion 1→2 for M12 sharded refs
<sha> manifest: UnmarshalBody validator with M12 invariants
<sha> manifest: add RefShard type + RefShards/RefSharding body fields (M12 schema)
<sha> repoerrs: add ErrInvalidManifest sentinel for M12
```

- [ ] **Step 3: Dispatch the two-stage Phase-0 review.**

Per the M1+ review protocol (see parent plan), dispatch a spec-compliance reviewer and a code-quality reviewer in parallel. Prompt template:

> Review the four Phase-0 commits in this worktree. Phase-0 covers `docs/m12-plans/phase-0-foundation.md`. Report findings as HIGH/MEDIUM/LOW with file:line citations. Verify in particular: the omitempty/normalization change in MarshalBody does not break any existing test's expected wire format; UnmarshalBody validator rejects every documented invariant; the SchemaVersion bump does not silently break any v1-only assertion.

Fix HIGH+MEDIUM findings inline, commit fixups, then proceed.

- [ ] **Step 4: Run roborev-refine for one round (or until clean).**

```bash
roborev review --branch --wait
```

Address findings, commit, comment + close. Repeat if Round 2 finds new issues.

- [ ] **Step 5: Proceed to Phase 1.**

Open `docs/m12-plans/phase-1-refstore-skeleton.md`.
