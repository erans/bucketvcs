# M12 Phase 4 — refstore conformance suite

> **Parent plan:** [docs/m12-ref-sharding-plan.md](../m12-ref-sharding-plan.md). Phases 0–3 must be complete.

**Goal:** add `internal/repo/refstore/conformance` — property tests asserting the dual-impl invariants the spec requires:

1. **Equivalence:** `InlineRefStore(R).List() == ShardedRefStore(shard(R)).List()` for arbitrary R.
2. **Round-trip:** applying a Stage and rebuilding the store yields the merged ref map.
3. **Determinism:** sharding the same ref set twice produces byte-identical shard objects.

The unit tests in Phases 1–3 cover small, hand-built cases. The conformance suite generates random ref sets and verifies the cross-cutting properties hold across distributions, sizes, and update patterns. Mirrors the `internal/storage/conformance/` pattern.

**Files created:**
- `internal/repo/refstore/conformance/doc.go`
- `internal/repo/refstore/conformance/suite.go`
- `internal/repo/refstore/conformance/properties.go`
- `internal/repo/refstore/conformance/conformance_test.go`

---

### Task 4.1: Conformance package skeleton

**Files:**
- Create: `internal/repo/refstore/conformance/doc.go`
- Create: `internal/repo/refstore/conformance/suite.go`

- [ ] **Step 1: Create the directory.**

```bash
mkdir -p internal/repo/refstore/conformance
```

- [ ] **Step 2: Create the package doc.**

`internal/repo/refstore/conformance/doc.go`:

```go
// Package conformance contains property-style cross-implementation
// tests for the RefStore interface. Mirrors the pattern in
// internal/storage/conformance: the test bodies live here and are
// invoked from a single _test.go entry point in the consuming
// package, so a future second consumer (e.g., a remote cache that
// implements RefStore) can re-run the same suite by supplying its
// own Factory.
//
// Three properties asserted:
//
//   - Equivalence: for any ref set R, InlineRefStore(R).List() ==
//     ShardedRefStore(shard(R)).List().
//   - Round-trip: applying a Stage and reconstructing the store
//     equals expected(oldRefs, updates).
//   - Determinism: marshalling the same ref set twice yields
//     byte-identical shard objects.
//
// Random ref-set generation is seeded explicitly so failures
// reproduce. Sizes scaled to keep the suite under a second.
package conformance
```

- [ ] **Step 3: Create the Factory + RunSuite entry point.**

`internal/repo/refstore/conformance/suite.go`:

```go
package conformance

import (
	"testing"
)

// RunSuite executes every property test against the InlineRefStore
// and ShardedRefStore pair the package exposes via internal helpers.
// Future consumers can add Factory-based parameters here; M12 only
// needs the built-in dual-impl matrix.
func RunSuite(t *testing.T) {
	t.Helper()
	t.Run("Equivalence", testEquivalence)
	t.Run("RoundTrip", testRoundTrip)
	t.Run("Determinism", testDeterminism)
}
```

- [ ] **Step 4: Commit the skeleton.**

```bash
git add internal/repo/refstore/conformance/
git commit -m "refstore/conformance: skeleton (M12 Phase 4 skeleton)"
```

---

### Task 4.2: Equivalence property

**Files:**
- Create: `internal/repo/refstore/conformance/properties.go`

- [ ] **Step 1: Build the property-test infrastructure + equivalence test.**

Create `internal/repo/refstore/conformance/properties.go`:

```go
package conformance

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"math/rand"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/refstore"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// genRefs deterministically generates n random refnames + 40-hex OIDs
// for the given seed. Used by every property test so failures
// reproduce by seed alone.
func genRefs(seed int64, n int) map[string]string {
	r := rand.New(rand.NewSource(seed))
	out := make(map[string]string, n)
	for i := 0; i < n; i++ {
		// Mix branches, tags, and PR-style refs so distribution is realistic.
		var name string
		switch r.Intn(3) {
		case 0:
			name = fmt.Sprintf("refs/heads/feature-%d-%d", seed, i)
		case 1:
			name = fmt.Sprintf("refs/tags/v%d.%d", seed%100, i)
		default:
			name = fmt.Sprintf("refs/pull/%d/head", i)
		}
		var oidBytes [20]byte
		r.Read(oidBytes[:])
		out[name] = hex.EncodeToString(oidBytes[:])
	}
	return out
}

// buildInline constructs an InlineRefStore over the given ref set.
// Wrapped to keep buildSharded's signature symmetric (both take t for
// future Factory parameterization).
func buildInline(t *testing.T, refs map[string]string) refstore.RefStore {
	t.Helper()
	body := &manifest.Body{Refs: refs}
	rs, err := refstore.New(context.Background(), nil, nil, body)
	if err != nil {
		t.Fatalf("buildInline: %v", err)
	}
	return rs
}

// buildSharded constructs a ShardedRefStore over the given ref set,
// seeding the in-memory store with shard objects produced by the
// production marshal+hash helpers.
func buildSharded(t *testing.T, refs map[string]string) refstore.RefStore {
	t.Helper()
	k, err := keys.NewRepo("acme", "demo")
	if err != nil {
		t.Fatalf("keys.NewRepo: %v", err)
	}
	store := newMemoryStore()
	// Bucket refs into shards.
	perShard := map[string]map[string]string{}
	for name, oid := range refs {
		sid := refstore.ShardKey(name)
		if perShard[sid] == nil {
			perShard[sid] = map[string]string{}
		}
		perShard[sid][name] = oid
	}
	var shards []manifest.RefShard
	for sid, r := range perShard {
		b, h := refstore.MarshalAndHashForTest(r)
		key := k.RefShardKey(h)
		store.put(key, b)
		shards = append(shards, manifest.RefShard{
			Shard:    sid,
			Key:      key,
			Hash:     h,
			RefCount: len(r),
		})
	}
	body := &manifest.Body{
		RefShards:   shards,
		RefSharding: "hash_v1",
	}
	rs, err := refstore.New(context.Background(), store, k, body)
	if err != nil {
		t.Fatalf("buildSharded: %v", err)
	}
	return rs
}

// testEquivalence asserts that for random ref sets of varied sizes,
// the inline and sharded implementations return identical Lists.
func testEquivalence(t *testing.T) {
	// Sizes chosen to span a single-shard case (tiny), a "every shard
	// likely populated" case (medium), and a busy case (large).
	for _, size := range []int{0, 1, 10, 100, 1000} {
		for _, seed := range []int64{1, 2, 3} {
			t.Run(fmt.Sprintf("size=%d/seed=%d", size, seed), func(t *testing.T) {
				refs := genRefs(seed, size)
				inline, _ := buildInline(t, refs).List(context.Background())
				sharded, err := buildSharded(t, refs).List(context.Background())
				if err != nil {
					t.Fatalf("sharded List: %v", err)
				}
				if len(inline) != len(sharded) {
					t.Fatalf("len differs: inline=%d sharded=%d", len(inline), len(sharded))
				}
				for k, v := range inline {
					if got := sharded[k]; got != v {
						t.Errorf("ref %q: inline=%q sharded=%q", k, v, got)
					}
				}
			})
		}
	}
}

// --- memory store for conformance use (matches the fakeStore in sharded_test.go) ---

type memoryStore struct {
	objects map[string][]byte
}

func newMemoryStore() *memoryStore { return &memoryStore{objects: map[string][]byte{}} }

func (m *memoryStore) put(key string, body []byte) {
	m.objects[key] = append([]byte(nil), body...)
}

func (m *memoryStore) Capabilities() storage.Capabilities { return storage.Capabilities{} }

func (m *memoryStore) Get(_ context.Context, key string, _ *storage.GetOptions) (*storage.Object, error) {
	b, ok := m.objects[key]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return &storage.Object{
		Body:     readCloser(b),
		Metadata: storage.ObjectMetadata{Key: key, Size: int64(len(b))},
	}, nil
}

func (m *memoryStore) Head(_ context.Context, key string) (*storage.ObjectMetadata, error) {
	b, ok := m.objects[key]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return &storage.ObjectMetadata{Key: key, Size: int64(len(b))}, nil
}

func (m *memoryStore) GetRange(context.Context, string, int64, int64) (io.ReadCloser, error) {
	return nil, fmt.Errorf("memoryStore: GetRange not supported")
}

func (m *memoryStore) PutIfAbsent(_ context.Context, key string, body io.Reader, _ *storage.PutOptions) (storage.ObjectVersion, error) {
	if _, ok := m.objects[key]; ok {
		return storage.ObjectVersion{}, storage.ErrAlreadyExists
	}
	buf, err := io.ReadAll(body)
	if err != nil {
		return storage.ObjectVersion{}, err
	}
	m.objects[key] = buf
	return storage.ObjectVersion{Token: "v1", Provider: "mem"}, nil
}

func (m *memoryStore) PutIfVersionMatches(context.Context, string, storage.ObjectVersion, io.Reader, *storage.PutOptions) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, fmt.Errorf("not supported")
}
func (m *memoryStore) DeleteIfVersionMatches(context.Context, string, storage.ObjectVersion) error {
	return fmt.Errorf("not supported")
}
func (m *memoryStore) List(context.Context, string, *storage.ListOptions) (*storage.ListPage, error) {
	return nil, fmt.Errorf("not supported")
}
func (m *memoryStore) CreateMultipart(context.Context, string, *storage.MultipartOptions) (storage.MultipartUpload, error) {
	return nil, fmt.Errorf("not supported")
}
func (m *memoryStore) CompleteMultipartIfAbsent(context.Context, storage.MultipartUpload, []storage.MultipartPart) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, fmt.Errorf("not supported")
}
func (m *memoryStore) SignedGetURL(context.Context, string, storage.SignedURLOptions) (string, http.Header, error) {
	return "", nil, storage.ErrNotSupported
}

func readCloser(b []byte) io.ReadCloser { return io.NopCloser(bytes.NewReader(b)) }
```

Add the missing imports at the top:

```go
import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/refstore"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)
```

(Remove `bytes` if `readCloser` is the only use — it is, so keep it.)

- [ ] **Step 2: Create the test driver file.**

`internal/repo/refstore/conformance/conformance_test.go`:

```go
package conformance

import "testing"

// TestRefStoreConformance is the single test driver Go's tool picks
// up; it dispatches into RunSuite which contains the property
// subtests. This split lets future consumers reuse RunSuite from
// their own _test.go file without forking the body.
func TestRefStoreConformance(t *testing.T) {
	RunSuite(t)
}
```

- [ ] **Step 3: Run only the equivalence subtest (round-trip and determinism are not yet implemented).**

The skeleton of `testRoundTrip` and `testDeterminism` doesn't exist yet — the suite.go references them. We'll add stubs to make the package compile, then implement them in Task 4.3.

Insert at the bottom of `properties.go`:

```go
// testRoundTrip is implemented in Task 4.3.
func testRoundTrip(t *testing.T) {
	t.Skip("implemented in Phase 4 Task 4.3")
}

// testDeterminism is implemented in Task 4.4.
func testDeterminism(t *testing.T) {
	t.Skip("implemented in Phase 4 Task 4.4")
}
```

- [ ] **Step 4: Run the conformance suite.**

```bash
go test ./internal/repo/refstore/conformance/... -count=1 -v
```

Expected: TestRefStoreConformance/Equivalence/size=X/seed=Y all PASS; TestRefStoreConformance/RoundTrip and Determinism report SKIP.

- [ ] **Step 5: Commit.**

```bash
git add internal/repo/refstore/conformance/
git commit -m "refstore/conformance: Equivalence property (M12 Phase 4.2)"
```

---

### Task 4.3: Round-trip property

**Files:**
- Modify: `internal/repo/refstore/conformance/properties.go`

- [ ] **Step 1: Replace the `testRoundTrip` stub with the real implementation.**

In `internal/repo/refstore/conformance/properties.go`, replace the stub:

```go
// testRoundTrip asserts: applying a Stage from sharded mode against
// random updates and rebuilding the store produces the same refs an
// inline merge would.
func testRoundTrip(t *testing.T) {
	for _, size := range []int{1, 10, 100, 500} {
		for _, seed := range []int64{11, 22, 33} {
			t.Run(fmt.Sprintf("size=%d/seed=%d", size, seed), func(t *testing.T) {
				refs := genRefs(seed, size)
				updates := genUpdates(seed+1000, refs)

				// Expected: inline-mode merge.
				expected := make(map[string]string, len(refs))
				for k, v := range refs {
					expected[k] = v
				}
				const nullOIDHex = "0000000000000000000000000000000000000000"
				for k, v := range updates {
					if v == "" || v == nullOIDHex {
						delete(expected, k)
					} else {
						expected[k] = v
					}
				}

				// Actual: build a sharded store, Stage updates, simulate the
				// commit by writing NewShardObjects to a fresh store and
				// constructing a new ShardedRefStore over NewRefShards, then
				// List.
				k, err := keys.NewRepo("acme", "demo")
				if err != nil {
					t.Fatalf("keys.NewRepo: %v", err)
				}
				store := newMemoryStore()
				// Seed initial shards.
				perShard := map[string]map[string]string{}
				for name, oid := range refs {
					sid := refstore.ShardKey(name)
					if perShard[sid] == nil {
						perShard[sid] = map[string]string{}
					}
					perShard[sid][name] = oid
				}
				var initialShards []manifest.RefShard
				for sid, r := range perShard {
					b, h := refstore.MarshalAndHashForTest(r)
					key := k.RefShardKey(h)
					store.put(key, b)
					initialShards = append(initialShards, manifest.RefShard{
						Shard: sid, Key: key, Hash: h, RefCount: len(r),
					})
				}
				body := &manifest.Body{RefShards: initialShards, RefSharding: "hash_v1"}
				rs, err := refstore.New(context.Background(), store, k, body)
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				stage, err := rs.Stage(context.Background(), updates)
				if err != nil {
					t.Fatalf("Stage: %v", err)
				}
				// Simulate commit: PutIfAbsent every new shard object.
				for _, w := range stage.NewShardObjects {
					if _, perr := store.PutIfAbsent(context.Background(), w.Key, readCloser(w.Contents), nil); perr != nil && !errors.Is(perr, storage.ErrAlreadyExists) {
						t.Fatalf("PutIfAbsent %s: %v", w.Key, perr)
					}
				}
				// Construct a new sharded store over the updated body.
				newBody := &manifest.Body{
					RefShards:   stage.NewRefShards,
					RefSharding: "hash_v1",
				}
				rs2, err := refstore.New(context.Background(), store, k, newBody)
				if err != nil {
					t.Fatalf("New (post-stage): %v", err)
				}
				got, err := rs2.List(context.Background())
				if err != nil {
					t.Fatalf("List (post-stage): %v", err)
				}
				if len(got) != len(expected) {
					t.Fatalf("len: got=%d expected=%d", len(got), len(expected))
				}
				for kk, vv := range expected {
					if got[kk] != vv {
						t.Errorf("ref %q: got=%q expected=%q", kk, got[kk], vv)
					}
				}
			})
		}
	}
}

// genUpdates samples ~10% of existing refnames to mutate (random new
// OID) and ~5% to delete (empty OID), plus injects a handful of
// brand-new refs. Seed-driven so reproducible.
func genUpdates(seed int64, existing map[string]string) map[string]string {
	r := rand.New(rand.NewSource(seed))
	out := map[string]string{}
	// Pick up to 10% of existing to update, 5% to delete.
	for name := range existing {
		switch r.Intn(20) {
		case 0:
			// delete.
			out[name] = ""
		case 1, 2:
			// update with new random OID.
			var b [20]byte
			r.Read(b[:])
			out[name] = hex.EncodeToString(b[:])
		}
	}
	// Add 1–3 brand-new refs.
	n := 1 + r.Intn(3)
	for i := 0; i < n; i++ {
		var b [20]byte
		r.Read(b[:])
		out[fmt.Sprintf("refs/heads/new-%d-%d", seed, i)] = hex.EncodeToString(b[:])
	}
	return out
}
```

Add `errors` to the import block.

- [ ] **Step 2: Run round-trip subtests.**

```bash
go test ./internal/repo/refstore/conformance/... -run 'TestRefStoreConformance/RoundTrip' -count=1 -v
```

Expected: every RoundTrip subtest PASSES.

- [ ] **Step 3: Commit.**

```bash
git add internal/repo/refstore/conformance/properties.go
git commit -m "refstore/conformance: RoundTrip property (M12 Phase 4.3)"
```

---

### Task 4.4: Determinism property

**Files:**
- Modify: `internal/repo/refstore/conformance/properties.go`

- [ ] **Step 1: Replace the `testDeterminism` stub.**

In `internal/repo/refstore/conformance/properties.go`, replace the stub:

```go
// testDeterminism asserts that marshalling the same ref set repeatedly
// produces byte-identical shard objects. Determinism is what makes
// content-addressing work; a regression here would silently inflate
// storage and break PutIfAbsent idempotency.
func testDeterminism(t *testing.T) {
	for _, size := range []int{1, 10, 100, 500} {
		for _, seed := range []int64{1, 2, 3} {
			t.Run(fmt.Sprintf("size=%d/seed=%d", size, seed), func(t *testing.T) {
				refs := genRefs(seed, size)
				// Bucket and marshal each shard; capture the first run.
				perShard := map[string]map[string]string{}
				for name, oid := range refs {
					sid := refstore.ShardKey(name)
					if perShard[sid] == nil {
						perShard[sid] = map[string]string{}
					}
					perShard[sid][name] = oid
				}
				type stamp struct {
					bytes []byte
					hash  string
				}
				first := map[string]stamp{}
				for sid, r := range perShard {
					b, h := refstore.MarshalAndHashForTest(r)
					first[sid] = stamp{bytes: b, hash: h}
				}
				// Repeat the marshal a few times and assert byte-identical output.
				for iter := 0; iter < 25; iter++ {
					for sid, r := range perShard {
						b, h := refstore.MarshalAndHashForTest(r)
						if !bytes.Equal(b, first[sid].bytes) {
							t.Fatalf("shard %s non-deterministic bytes on iter %d:\n  first=%s\n  later=%s",
								sid, iter, first[sid].bytes, b)
						}
						if h != first[sid].hash {
							t.Fatalf("shard %s non-deterministic hash on iter %d: first=%s later=%s",
								sid, iter, first[sid].hash, h)
						}
					}
				}
			})
		}
	}
}
```

- [ ] **Step 2: Run Determinism subtests.**

```bash
go test ./internal/repo/refstore/conformance/... -run 'TestRefStoreConformance/Determinism' -count=1 -v
```

Expected: all PASS.

- [ ] **Step 3: Run the entire suite + full sweep.**

```bash
go test ./internal/repo/refstore/conformance/... -count=1
go test ./... -count=1 2>&1 | grep -E "^(FAIL|---)" | head -10
```

Expected: conformance suite passes; no failures elsewhere.

- [ ] **Step 4: Commit.**

```bash
git add internal/repo/refstore/conformance/properties.go
git commit -m "refstore/conformance: Determinism property (M12 Phase 4.4)"
```

---

### Task 4.5: Phase 4 boundary checkpoint

- [ ] **Step 1: Full sweep + vet.**

```bash
go test ./... -count=1 2>&1 | grep -E "^(FAIL|---)" | head -10
go vet ./...
```

Expected: empty + clean.

- [ ] **Step 2: Two-stage review.**

Focus areas for reviewers:
- Seed-driven determinism (failures must reproduce by seed).
- Coverage breadth (sizes from 0 to 1000, multiple seeds).
- The Determinism test repeats 25 iterations per fixture — adequate? (Go's map randomization is per-process, so 25 distinct hash-seeded iterations are enough to surface a non-deterministic marshal in practice.)
- Whether the Stage round-trip simulates the production commit faithfully (it does: PutIfAbsent every NewShardObject, then rebuild RefStore over NewRefShards).

- [ ] **Step 3: roborev-refine.**

- [ ] **Step 4: Proceed to Phase 5 (switch ref consumers).**
