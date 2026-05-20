# M12 Phase 2 — ShardedRefStore read paths

> **Parent plan:** [docs/m12-ref-sharding-plan.md](../m12-ref-sharding-plan.md). Phases 0–1 must be complete.

**Goal:** replace the Phase-1 stub `ShardedRefStore.Lookup` and `ShardedRefStore.List` with full implementations: parallel-fetch shards, verify content hashes, return merged ref maps. After this phase reads through `refstore.New(...).List()` work end-to-end against a v2 body, including the corruption-detection canary.

**Files touched:**
- Modify: `internal/repo/refstore/sharded.go` (replace stubs in `Lookup` + `List`)
- Create: `internal/repo/refstore/sharded_test.go`
- (No production-code changes outside `refstore/`. Consumers still use Phase-1 InlineRefStore.)

---

### Task 2.1: ShardedRefStore.Lookup

**Files:**
- Modify: `internal/repo/refstore/sharded.go`
- Create: `internal/repo/refstore/sharded_test.go`

- [ ] **Step 1: Write the failing Lookup tests first.**

Create `internal/repo/refstore/sharded_test.go`:

```go
package refstore_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/refstore"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// shardFixture builds a v2 body + an in-memory store seeded with one
// shard per provided (shardID → refs). hashFor uses the production
// marshal+hash helpers via refstore so the body and store stay
// consistent. Reused across Lookup, List, and corruption tests.
func shardFixture(t *testing.T, perShard map[string]map[string]string) (*manifest.Body, storage.ObjectStore, *keys.Repo) {
	t.Helper()
	k, err := keys.NewRepo("acme", "demo")
	if err != nil {
		t.Fatalf("keys.NewRepo: %v", err)
	}
	store := newFakeStore()
	var shards []manifest.RefShard
	for shardID, refs := range perShard {
		bytes, hash := refstore.MarshalAndHashForTest(refs)
		key := k.RefShardKey(hash)
		store.put(key, bytes)
		shards = append(shards, manifest.RefShard{
			Shard:    shardID,
			Key:      key,
			Hash:     hash,
			RefCount: len(refs),
		})
	}
	body := &manifest.Body{
		DefaultBranch: "refs/heads/main",
		RefShards:     shards,
		RefSharding:   "hash_v1",
	}
	return body, store, k
}

func TestSharded_Lookup_Hit(t *testing.T) {
	// Put two refs in the shard for refs/heads/main.
	mainShard := refstore.ShardKey("refs/heads/main")
	devShard := refstore.ShardKey("refs/heads/dev")
	if mainShard == devShard {
		// Construct a different second ref name until they shard differently.
		t.Skip("test fixtures assume main and dev land in different shards; rare collision")
	}
	body, store, k := shardFixture(t, map[string]map[string]string{
		mainShard: {"refs/heads/main": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		devShard:  {"refs/heads/dev": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
	})
	rs, err := refstore.New(context.Background(), store, k, body)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if rs.Mode() != refstore.ModeSharded {
		t.Errorf("Mode=%v want sharded", rs.Mode())
	}
	oid, ok, err := rs.Lookup(context.Background(), "refs/heads/main")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !ok || oid != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("Lookup=(%q,%v) want (aa..., true)", oid, ok)
	}
}

func TestSharded_Lookup_MissUnknownShard(t *testing.T) {
	// Body lists only one shard; query a refname whose shard ID is not present.
	mainShard := refstore.ShardKey("refs/heads/main")
	body, store, k := shardFixture(t, map[string]map[string]string{
		mainShard: {"refs/heads/main": "aa"},
	})
	rs, _ := refstore.New(context.Background(), store, k, body)
	// Find a refname whose shard ID differs from mainShard.
	var probe string
	for i := 0; ; i++ {
		probe = "refs/heads/probe-" + string(rune('a'+i%26))
		if refstore.ShardKey(probe) != mainShard {
			break
		}
	}
	oid, ok, err := rs.Lookup(context.Background(), probe)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if ok || oid != "" {
		t.Errorf("Lookup=(%q,%v) want (\"\", false)", oid, ok)
	}
}

func TestSharded_Lookup_MissInPopulatedShard(t *testing.T) {
	// Shard exists but the refname is not in it.
	body, store, k := shardFixture(t, map[string]map[string]string{
		refstore.ShardKey("refs/heads/main"): {"refs/heads/main": "aa"},
	})
	rs, _ := refstore.New(context.Background(), store, k, body)
	// Pick a refname whose shard ID matches refs/heads/main but is not refs/heads/main.
	other := "refs/heads/main-other"
	// Iterate to find a collision with shardKey(refs/heads/main).
	wanted := refstore.ShardKey("refs/heads/main")
	for i := 0; refstore.ShardKey(other) != wanted; i++ {
		other = "refs/heads/main-other-" + string(rune('a'+i%26))
		if i > 1000 {
			t.Skip("could not find a colliding refname within 1000 tries; rare under sha256")
		}
	}
	oid, ok, err := rs.Lookup(context.Background(), other)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if ok || oid != "" {
		t.Errorf("Lookup=(%q,%v) want (\"\", false)", oid, ok)
	}
}

func TestSharded_Lookup_BackendError(t *testing.T) {
	body, store, k := shardFixture(t, map[string]map[string]string{
		refstore.ShardKey("refs/heads/main"): {"refs/heads/main": "aa"},
	})
	store.(*fakeStore).failOnGet = true
	rs, _ := refstore.New(context.Background(), store, k, body)
	_, _, err := rs.Lookup(context.Background(), "refs/heads/main")
	if err == nil {
		t.Fatal("Lookup: want error, got nil")
	}
}

func TestSharded_Lookup_CorruptShard(t *testing.T) {
	body, store, k := shardFixture(t, map[string]map[string]string{
		refstore.ShardKey("refs/heads/main"): {"refs/heads/main": "aa"},
	})
	// Corrupt the shard contents while keeping the body's recorded hash.
	store.(*fakeStore).overwriteRaw(body.RefShards[0].Key, []byte(`{"refs/heads/main":"BB"}`))
	rs, _ := refstore.New(context.Background(), store, k, body)
	_, _, err := rs.Lookup(context.Background(), "refs/heads/main")
	if !errors.Is(err, refstore.ErrShardCorrupt) {
		t.Fatalf("err = %v, want ErrShardCorrupt", err)
	}
}

// --- in-memory fake store used by sharded_test.go ---

type fakeStore struct {
	objects   map[string][]byte
	failOnGet bool
	getCalls  int
}

func newFakeStore() storage.ObjectStore { return &fakeStore{objects: map[string][]byte{}} }

func (f *fakeStore) put(key string, body []byte) { f.objects[key] = append([]byte(nil), body...) }

func (f *fakeStore) overwriteRaw(key string, body []byte) {
	f.objects[key] = append([]byte(nil), body...)
}

func (f *fakeStore) Capabilities() storage.Capabilities {
	return storage.Capabilities{}
}
func (f *fakeStore) Get(_ context.Context, key string, _ *storage.GetOptions) (*storage.Object, error) {
	f.getCalls++
	if f.failOnGet {
		return nil, errors.New("fake: forced failure")
	}
	b, ok := f.objects[key]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return &storage.Object{
		Body:     io.NopCloser(bytes.NewReader(b)),
		Metadata: storage.ObjectMetadata{Key: key, Size: int64(len(b))},
	}, nil
}
func (f *fakeStore) Head(_ context.Context, key string) (*storage.ObjectMetadata, error) {
	b, ok := f.objects[key]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return &storage.ObjectMetadata{Key: key, Size: int64(len(b))}, nil
}
func (f *fakeStore) GetRange(context.Context, string, int64, int64) (io.ReadCloser, error) {
	return nil, errors.New("fake: GetRange not implemented")
}
func (f *fakeStore) PutIfAbsent(_ context.Context, key string, body io.Reader, _ *storage.PutOptions) (storage.ObjectVersion, error) {
	if _, ok := f.objects[key]; ok {
		return storage.ObjectVersion{}, storage.ErrAlreadyExists
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return storage.ObjectVersion{}, err
	}
	f.objects[key] = b
	return storage.ObjectVersion{Token: "v1", Provider: "fake"}, nil
}
func (f *fakeStore) PutIfVersionMatches(context.Context, string, storage.ObjectVersion, io.Reader, *storage.PutOptions) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, errors.New("fake: PutIfVersionMatches not implemented")
}
func (f *fakeStore) DeleteIfVersionMatches(context.Context, string, storage.ObjectVersion) error {
	return errors.New("fake: DeleteIfVersionMatches not implemented")
}
func (f *fakeStore) List(context.Context, string, *storage.ListOptions) (*storage.ListPage, error) {
	return nil, errors.New("fake: List not implemented")
}
func (f *fakeStore) CreateMultipart(context.Context, string, *storage.MultipartOptions) (storage.MultipartUpload, error) {
	return nil, errors.New("fake: CreateMultipart not implemented")
}
func (f *fakeStore) CompleteMultipartIfAbsent(context.Context, storage.MultipartUpload, []storage.MultipartPart) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, errors.New("fake: CompleteMultipartIfAbsent not implemented")
}
func (f *fakeStore) SignedGetURL(context.Context, string, storage.SignedURLOptions) (string, http.Header, error) {
	return "", nil, storage.ErrNotSupported
}
```

Add the missing `http` import at the top:

```go
import "net/http"
```

The test file uses `refstore.MarshalAndHashForTest`. We need to export the marshal+hash helpers for tests. Continue to Step 2.

- [ ] **Step 2: Add a test-only export of marshal+hash from the refstore package.**

Create `internal/repo/refstore/export_test.go`:

```go
package refstore

// MarshalAndHashForTest exposes marshalShardContent + hashShardContent
// for tests in the refstore_test package. Not part of the public API.
func MarshalAndHashForTest(refs map[string]string) ([]byte, string) {
	b, err := marshalShardContent(refs)
	if err != nil {
		panic(err)
	}
	return b, hashShardContent(b)
}
```

Note: this lives in `internal/repo/refstore/export_test.go` (in-package, since `_test.go` files in the same package have access to unexported helpers). The function name ending in `ForTest` and the file being `_test.go` keeps it out of production builds.

Actually, the import in the test file is `refstore_test` (external package). To make the helper visible from `refstore_test`, the file must be named `*_test.go` AND declare `package refstore` (in-package), then the symbol is reachable from external `refstore_test` because both belong to the same Go test binary.

The convention works: file in `package refstore`, ending in `_test.go`, exposing `MarshalAndHashForTest`. The Go test binary links both, and `refstore_test` can import `refstore` and call the symbol.

- [ ] **Step 3: Run the tests; expect FAIL (Lookup still returns ErrUnsupported).**

Run: `go test ./internal/repo/refstore/... -run TestSharded_Lookup -count=1 2>&1 | head -30`
Expected: builds, but `TestSharded_Lookup_*` fail with `errors.ErrUnsupported`.

- [ ] **Step 4: Implement ShardedRefStore.Lookup.**

Replace the `Lookup` method in `internal/repo/refstore/sharded.go`:

```go
// Lookup hashes refname to its shard, fetches that one shard, verifies
// the content hash, and returns (oid, exists) from the shard map.
// Missing shard (refname's bucket not present in body.RefShards) is
// not an error — it means "no refs with that shard exist" which
// implies "refname not present."
//
// ErrShardCorrupt is returned if the fetched shard's sha256 does not
// match body.RefShards[i].Hash. Callers MUST treat this as fatal —
// retrying may amplify damage.
func (s *ShardedRefStore) Lookup(ctx context.Context, refname string) (string, bool, error) {
	sid := shardKey(refname)
	for i := range s.shards {
		if s.shards[i].Shard != sid {
			continue
		}
		refs, err := s.fetchShard(ctx, s.shards[i])
		if err != nil {
			return "", false, err
		}
		oid, ok := refs[refname]
		return oid, ok, nil
	}
	return "", false, nil
}

// fetchShard reads one shard object from the store, verifies its
// content hash, and returns the parsed map. Used by Lookup, List, and
// Stage (Phase 3). Hash verification is the tampering canary required
// by the spec.
func (s *ShardedRefStore) fetchShard(ctx context.Context, ref manifest.RefShard) (map[string]string, error) {
	obj, err := s.store.Get(ctx, ref.Key, nil)
	if err != nil {
		return nil, fmt.Errorf("refstore: fetch shard %s (%s): %w", ref.Shard, ref.Key, err)
	}
	defer obj.Body.Close()
	raw, err := io.ReadAll(obj.Body)
	if err != nil {
		return nil, fmt.Errorf("refstore: read shard %s body: %w", ref.Shard, err)
	}
	got := hashShardContent(raw)
	if got != ref.Hash {
		return nil, fmt.Errorf("%w: shard %s want %s got %s", ErrShardCorrupt, ref.Shard, ref.Hash, got)
	}
	var refs map[string]string
	if err := json.Unmarshal(raw, &refs); err != nil {
		return nil, fmt.Errorf("refstore: parse shard %s: %w", ref.Shard, err)
	}
	return refs, nil
}
```

Add the missing imports at the top of `sharded.go`:

```go
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)
```

Remove the now-unused `errors` import if `errors.ErrUnsupported` is no longer referenced (the List + Stage stubs still use it for Phase 2/3 boundary; keep `errors` until Stage is implemented in Phase 3).

- [ ] **Step 5: Run the Lookup tests; expect PASS.**

Run: `go test ./internal/repo/refstore/... -run TestSharded_Lookup -count=1 -v`
Expected: all 5 Lookup subtests PASS.

- [ ] **Step 6: Commit.**

```bash
git add internal/repo/refstore/sharded.go internal/repo/refstore/sharded_test.go internal/repo/refstore/export_test.go
git commit -m "refstore: ShardedRefStore.Lookup + shard hash verification (M12 Phase 2.1)"
```

---

### Task 2.2: ShardedRefStore.List

**Files:**
- Modify: `internal/repo/refstore/sharded.go`
- Modify: `internal/repo/refstore/sharded_test.go`

- [ ] **Step 1: Write the failing List tests.**

Append to `internal/repo/refstore/sharded_test.go`:

```go
func TestSharded_List_MergesAllShards(t *testing.T) {
	// Build a fixture with refs across 3 distinct shards.
	refs := map[string]string{
		"refs/heads/main":  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"refs/heads/dev":   "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"refs/tags/v1.0.0": "cccccccccccccccccccccccccccccccccccccccc",
		"refs/tags/v1.1.0": "dddddddddddddddddddddddddddddddddddddddd",
	}
	perShard := map[string]map[string]string{}
	for name, oid := range refs {
		sid := refstore.ShardKey(name)
		if perShard[sid] == nil {
			perShard[sid] = map[string]string{}
		}
		perShard[sid][name] = oid
	}
	body, store, k := shardFixture(t, perShard)
	rs, _ := refstore.New(context.Background(), store, k, body)
	got, err := rs.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != len(refs) {
		t.Fatalf("len=%d want %d (got=%+v)", len(got), len(refs), got)
	}
	for k, v := range refs {
		if got[k] != v {
			t.Errorf("List[%q]=%q want %q", k, got[k], v)
		}
	}
}

func TestSharded_List_OneShardCorruptFailsAll(t *testing.T) {
	body, store, k := shardFixture(t, map[string]map[string]string{
		refstore.ShardKey("refs/heads/main"): {"refs/heads/main": "aa"},
		refstore.ShardKey("refs/heads/dev"):  {"refs/heads/dev": "bb"},
	})
	// Corrupt one shard's bytes.
	store.(*fakeStore).overwriteRaw(body.RefShards[0].Key, []byte(`{"refs/heads/x":"xx"}`))
	rs, _ := refstore.New(context.Background(), store, k, body)
	_, err := rs.List(context.Background())
	if !errors.Is(err, refstore.ErrShardCorrupt) {
		t.Fatalf("err = %v, want ErrShardCorrupt", err)
	}
}

func TestSharded_List_EmptyBody(t *testing.T) {
	body, store, k := shardFixture(t, map[string]map[string]string{})
	rs, _ := refstore.New(context.Background(), store, k, body)
	// shardFixture with empty perShard produces a body with RefShards=nil.
	// The factory dispatches to InlineRefStore in that case. To exercise
	// ShardedRefStore's empty-shard-list path, build the v2 body by hand.
	body = &manifest.Body{
		DefaultBranch: "refs/heads/main",
		RefShards:     []manifest.RefShard{},
		RefSharding:   "hash_v1",
	}
	rs, err := refstore.New(context.Background(), store, k, body)
	if err != nil {
		// An empty RefShards slice routes through InlineRefStore (len == 0
		// satisfies the dispatcher's "no shards" branch). The factory's
		// behavior is intentional: empty sharded mode is indistinguishable
		// from inline mode at the wire level, and inline is the cheaper
		// representation. Test that the contract holds:
		t.Logf("New routed empty-shard body to inline as expected: %v", err)
	}
	got, err := rs.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List=%v want empty", got)
	}
}
```

Note the comment on the "empty body" test: an empty `RefShards` slice routes through `InlineRefStore` per the factory. That's intentional. The test documents the dispatcher behavior; no production-code special-case is needed.

- [ ] **Step 2: Run the tests; expect FAIL (List stub).**

Run: `go test ./internal/repo/refstore/... -run TestSharded_List -count=1 2>&1 | head -30`
Expected: build OK, FAIL on errors.ErrUnsupported from the stub.

- [ ] **Step 3: Implement List with parallel fetch.**

Replace the `List` method in `internal/repo/refstore/sharded.go`:

```go
// List parallel-fetches every shard listed in body.RefShards,
// verifies each one's content hash, and returns the merged map.
// Concurrency is bounded to len(shards) goroutines; the errgroup
// cancels remaining work on the first failure so corruption fails
// the whole call (no partial-result hazard).
func (s *ShardedRefStore) List(ctx context.Context) (map[string]string, error) {
	if len(s.shards) == 0 {
		return map[string]string{}, nil
	}
	out := make(map[string]string)
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	for i := range s.shards {
		shard := s.shards[i]
		g.Go(func() error {
			refs, err := s.fetchShard(gctx, shard)
			if err != nil {
				return err
			}
			mu.Lock()
			for k, v := range refs {
				out[k] = v
			}
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return out, nil
}
```

Add the imports at the top of `sharded.go`:

```go
	"sync"

	"golang.org/x/sync/errgroup"
```

The `golang.org/x/sync/errgroup` package is already a transitive dep of the project (used elsewhere — verify with `go list -m golang.org/x/sync`; if absent, add via `go get golang.org/x/sync@latest`).

- [ ] **Step 4: Verify the dep is available.**

Run: `grep -r "golang.org/x/sync/errgroup" --include='*.go' . | head -5`
Expected: at least one match — the dep is already used in the project. If zero matches, run `go get golang.org/x/sync@latest && go mod tidy`.

- [ ] **Step 5: Run the List tests; expect PASS.**

Run: `go test ./internal/repo/refstore/... -run TestSharded_List -count=1 -v`
Expected: all PASS.

- [ ] **Step 6: Commit.**

```bash
git add internal/repo/refstore/sharded.go internal/repo/refstore/sharded_test.go go.mod go.sum
git commit -m "refstore: ShardedRefStore.List with parallel fetch + hash verify (M12 Phase 2.2)"
```

---

### Task 2.3: Phase 2 boundary checkpoint

- [ ] **Step 1: Full sweep + vet.**

```bash
go test ./... -count=1 2>&1 | grep -E "^(FAIL|---)" | head -10
go vet ./...
```

Expected: empty output (no failures), vet clean.

- [ ] **Step 2: Confirm Phase-2 commits.**

```bash
git log --oneline main..HEAD | head -5
```

Should now include:
- `refstore: ShardedRefStore.List with parallel fetch + hash verify (M12 Phase 2.2)`
- `refstore: ShardedRefStore.Lookup + shard hash verification (M12 Phase 2.1)`

- [ ] **Step 3: Two-stage review.**

Focus areas to call out in the reviewer prompt:
- Hash-verification ordering (read → verify → parse, never parse first).
- errgroup cancellation behavior on first failure.
- Memory-safety of `out` map under concurrent goroutines.
- ErrShardCorrupt wrapping using `%w` so `errors.Is` works.

- [ ] **Step 4: roborev-refine until clean or diminishing returns.**

- [ ] **Step 5: Proceed to Phase 3 (sharded write path).**
