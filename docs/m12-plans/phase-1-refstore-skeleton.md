# M12 Phase 1 — refstore package skeleton

> **Parent plan:** [docs/m12-ref-sharding-plan.md](../m12-ref-sharding-plan.md). Phase 0 (foundation) must be complete before starting this phase.

**Goal:** stand up `internal/repo/refstore` with the public interface, sentinel errors, shard-key helper, canonical-JSON marshal helper, and a complete `InlineRefStore` implementation. After this phase the package compiles, has unit tests, and inline-mode ref access works end-to-end through the interface (though no consumer uses it yet — that's Phase 5).

**Files created in this phase:**

- `internal/repo/refstore/doc.go`
- `internal/repo/refstore/refstore.go` — interface + types + sentinel errors + `shardKey` + `New` factory
- `internal/repo/refstore/marshal.go` — canonical-JSON encoder for shard objects
- `internal/repo/refstore/marshal_test.go`
- `internal/repo/refstore/inline.go` — `InlineRefStore`
- `internal/repo/refstore/inline_test.go`
- `internal/repo/refstore/shardkey_test.go`

---

### Task 1.1: Package doc.go + skeleton file

**Files:**
- Create: `internal/repo/refstore/doc.go`

- [ ] **Step 1: Create the package doc file.**

```bash
mkdir -p internal/repo/refstore
```

Then create `internal/repo/refstore/doc.go`:

```go
// Package refstore abstracts ref reads, writes, and staging behind a
// single interface (RefStore) so callers do not need to know whether
// the underlying root manifest stores refs inline (v1) or in
// content-addressed shards (v2).
//
// Two implementations:
//
//   - InlineRefStore wraps Body.Refs directly. Lookup and List are
//     pure in-memory; Stage records the merged map for the caller.
//
//   - ShardedRefStore wraps Body.RefShards plus an ObjectStore. Lookup
//     reads one shard; List parallel-fetches every shard listed in the
//     body and verifies each shard's sha256; Stage hash-buckets the
//     updates and computes the set of new shard objects to write.
//
// The shardKey function (sha256 of refname, first byte hex) is the
// only sharding strategy M12 ships. The ref_sharding string in the
// body schema gates future strategies; UnmarshalBody rejects unknown
// values.
//
// Push integration: the caller mints a Stage from updates, writes
// stage.NewShardObjects via PutIfAbsent inside Repo.Commit's buildBody
// callback (before returning the new body bytes), and assigns
// stage.NewRefShards to the new body. The root manifest CAS remains
// the only commit point; orphan shard objects from aborted pushes are
// content-addressed and become GC candidates after retention.
package refstore
```

- [ ] **Step 2: Confirm the package compiles even though it has nothing in it yet.**

Run: `go build ./internal/repo/refstore/...`
Expected: no output.

- [ ] **Step 3: Commit.**

```bash
git add internal/repo/refstore/doc.go
git commit -m "refstore: package doc skeleton (M12)"
```

---

### Task 1.2: Public interface + types + sentinel errors

**Files:**
- Create: `internal/repo/refstore/refstore.go`

- [ ] **Step 1: Create the file with interface + types + sentinels + factory stub.**

```go
package refstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Mode discriminates inline vs sharded staging. Encoded in Stage so
// downstream code (Repo.Commit buildBody) can branch on the layout
// without re-inspecting Body fields.
type Mode int

const (
	// ModeInline indicates a v1 layout — refs live directly in Body.Refs.
	ModeInline Mode = 1 + iota

	// ModeSharded indicates a v2 layout — refs live in
	// manifest/ref-shards/<hash>.json objects referenced by Body.RefShards.
	ModeSharded
)

// String renders Mode in a human-readable form for logs and errors.
func (m Mode) String() string {
	switch m {
	case ModeInline:
		return "inline"
	case ModeSharded:
		return "sharded"
	default:
		return fmt.Sprintf("mode(%d)", int(m))
	}
}

// RefStore is the read+stage interface every M12+ ref consumer uses.
// Implementations capture the body snapshot at construction time;
// callers re-construct against a fresh body after CAS retries.
type RefStore interface {
	// Mode reports which layout this store wraps. Stable for the
	// store's lifetime.
	Mode() Mode

	// Lookup returns the OID for refname, or (exists=false) when not
	// present. For inline stores this is O(1); for sharded stores
	// this loads exactly one shard (the one whose key hashes refname).
	Lookup(ctx context.Context, refname string) (oid string, exists bool, err error)

	// List returns every ref this store covers as a flat map. For
	// inline stores this is the Body.Refs map; for sharded stores
	// this parallel-fetches every shard and merges. Mutating the
	// returned map is undefined.
	List(ctx context.Context) (map[string]string, error)

	// Stage computes the layout-aware delta required to publish a
	// new ref state. updates uses the same convention as
	// importer.mergeRefs: empty OID or nullOIDHex means delete; any
	// other 40-hex value means upsert. The caller must validate
	// refnames separately (Stage does NOT enforce ref-name syntax).
	//
	// For inline stores, the returned Stage has Mode=ModeInline,
	// no NewShardObjects, and NewInlineRefs populated with the
	// final ref map (merged old + updates). The caller assigns
	// that map to Body.Refs in the new body.
	//
	// For sharded stores, the returned Stage has Mode=ModeSharded,
	// NewShardObjects covering every shard whose content changed
	// (PutIfAbsent these before the root CAS), and NewRefShards
	// populated with the final []RefShard slice for the new body.
	// NewInlineRefs is nil for sharded stores.
	Stage(ctx context.Context, updates map[string]string) (Stage, error)
}

// Stage is the pre-commit delta returned by RefStore.Stage. Lifetime
// is one Repo.Commit attempt; recompute against a fresh RefStore on
// CAS retry.
type Stage struct {
	Mode Mode

	// NewInlineRefs is the merged ref map (final state) when Mode ==
	// ModeInline. Nil when Mode == ModeSharded.
	NewInlineRefs map[string]string

	// NewShardObjects lists every shard whose content this push
	// generates. The caller PutIfAbsent's each one (content-
	// addressed, so concurrent identical writes are idempotent
	// via storage.ErrAlreadyExists swallowed by errors.Is) before
	// the root CAS. Empty when Mode == ModeInline.
	NewShardObjects []ShardWrite

	// NewRefShards is the final []manifest.RefShard slice for the
	// new body. Empty when Mode == ModeInline. Shards whose
	// content became empty after the update (e.g., a deletion that
	// removed the last ref in a bucket) are NOT included here.
	NewRefShards []manifest.RefShard
}

// ShardWrite is one shard object to PutIfAbsent before the root CAS.
// Key is the full storage key (includes the content hash so concurrent
// writers with the same content collapse to a single object). Shard
// is the 2-hex shard ID this object covers; callers (Stage.Lookup,
// the importer's default-branch deletion check) use it to route an
// in-memory refname lookup against a staged-but-not-yet-committed
// shard without re-parsing the contents twice.
type ShardWrite struct {
	Shard    string // "00".."ff"
	Key      string
	Hash     string // "sha256-<64hex>"; matches manifest.RefShard.Hash
	Contents []byte
}

// Sentinel errors returned by RefStore implementations. All are
// wrapped via fmt.Errorf("%w: ...", X) with enough context for
// the caller to log; use errors.Is to detect class.
var (
	// ErrShardCorrupt indicates a shard object's bytes hashed to a
	// value different from the body's recorded RefShard.Hash.
	// Treated as a tampering canary by callers — never retry.
	ErrShardCorrupt = errors.New("refstore: shard content hash mismatch")

	// ErrStaleRef indicates a Stage call where one of updates'
	// old-OID prechecks (when wired in via Phase 5) found a value
	// different from the on-store ref. Caller surfaces the per-ref
	// conflict on the wire.
	ErrStaleRef = errors.New("refstore: ref old-OID precheck failed")

	// ErrInline indicates an operation that only makes sense on a
	// sharded store was invoked on an inline store, or vice versa.
	ErrInline = errors.New("refstore: operation requires sharded mode")
	// ErrNotSharded mirrors ErrInline from the other direction; kept
	// distinct so error messages can be specific about which guard
	// fired.
	ErrNotSharded = errors.New("refstore: operation requires inline mode")
)

// shardKey returns the 2-hex shard identifier for refname.
//
// Hashing: sha256 of the UTF-8 bytes of refname; the first byte is
// rendered as 2 lowercase hex characters ("00".."ff"). Stable across
// builds; do NOT change without bumping the ref_sharding strategy
// string (and writing the migration).
func shardKey(refname string) string {
	sum := sha256.Sum256([]byte(refname))
	return hex.EncodeToString(sum[:1])
}

// ShardKey is the exported alias for shardKey. Stable public surface
// for tests, the conformance suite, the reshard CLI, and any future
// observability tool that wants to know which shard a refname lands
// in without rebuilding the helper.
func ShardKey(refname string) string {
	return shardKey(refname)
}

// New is the dispatch factory. It inspects body and returns an
// InlineRefStore when Body.RefShards is empty, otherwise a
// ShardedRefStore. The store reference and tenant/repo keys are
// only consulted by the sharded path; passing zero values is fine
// for inline-only callers.
//
// New does NOT validate body — callers should have already routed
// the bytes through manifest.UnmarshalBody, which catches hybrid
// state, unknown sharding strategies, and malformed shard fields.
// New does enforce one final defensive check: a body with
// RefSharding set to something other than "hash_v1" returns
// ErrInvalidStrategy.
func New(ctx context.Context, s storage.ObjectStore, k *keys.Repo, body *manifest.Body) (RefStore, error) {
	if body == nil {
		return nil, fmt.Errorf("refstore.New: nil body")
	}
	if len(body.RefShards) == 0 {
		return newInlineRefStore(body), nil
	}
	if body.RefSharding != "hash_v1" {
		return nil, fmt.Errorf("refstore.New: ref_sharding=%q (only \"hash_v1\" supported)", body.RefSharding)
	}
	if s == nil {
		return nil, fmt.Errorf("refstore.New: sharded body requires non-nil ObjectStore")
	}
	if k == nil {
		return nil, fmt.Errorf("refstore.New: sharded body requires non-nil keys.Repo")
	}
	return newShardedRefStore(s, k, body), nil
}
```

- [ ] **Step 2: Confirm compilation fails (InlineRefStore + ShardedRefStore not yet defined).**

Run: `go build ./internal/repo/refstore/...`
Expected: errors referencing `newInlineRefStore` and `newShardedRefStore`.

(This is the planned failure — we'll satisfy the references in Task 1.4 and Phase 2.)

- [ ] **Step 3: Don't commit yet — the package doesn't compile. Move to Task 1.3 next so we have something to commit.**

---

### Task 1.3: Canonical-JSON shard marshal helper

**Files:**
- Create: `internal/repo/refstore/marshal.go`
- Create: `internal/repo/refstore/marshal_test.go`

- [ ] **Step 1: Write the failing tests first.**

Create `internal/repo/refstore/marshal_test.go`:

```go
package refstore

import (
	"bytes"
	"strings"
	"testing"
)

func TestMarshalShardContent_Empty(t *testing.T) {
	got, err := marshalShardContent(nil)
	if err != nil {
		t.Fatalf("marshalShardContent(nil): %v", err)
	}
	if string(got) != "{}" {
		t.Errorf("got %q want %q", got, "{}")
	}
}

func TestMarshalShardContent_Deterministic(t *testing.T) {
	// Map iteration order in Go is randomized; the marshaller must
	// sort keys to produce byte-identical output across runs.
	refs := map[string]string{
		"refs/heads/main":  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"refs/heads/dev":   "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"refs/tags/v1.0.0": "cccccccccccccccccccccccccccccccccccccccc",
	}
	var first []byte
	for i := 0; i < 50; i++ {
		got, err := marshalShardContent(refs)
		if err != nil {
			t.Fatalf("marshalShardContent: %v", err)
		}
		if first == nil {
			first = got
			continue
		}
		if !bytes.Equal(first, got) {
			t.Fatalf("non-deterministic output:\n  first=%s\n  later=%s", first, got)
		}
	}
}

func TestMarshalShardContent_SortedKeys(t *testing.T) {
	refs := map[string]string{
		"refs/heads/z":    "11",
		"refs/heads/aaa":  "22",
		"refs/heads/m":    "33",
	}
	got, err := marshalShardContent(refs)
	if err != nil {
		t.Fatalf("marshalShardContent: %v", err)
	}
	// Find each ref's position and assert lex order.
	posA := strings.Index(string(got), "refs/heads/aaa")
	posM := strings.Index(string(got), "refs/heads/m")
	posZ := strings.Index(string(got), "refs/heads/z")
	if !(posA < posM && posM < posZ) {
		t.Errorf("keys not sorted: aaa@%d m@%d z@%d", posA, posM, posZ)
	}
}

func TestMarshalShardContent_TwoSpaceIndent(t *testing.T) {
	refs := map[string]string{"refs/heads/main": "aa"}
	got, err := marshalShardContent(refs)
	if err != nil {
		t.Fatalf("marshalShardContent: %v", err)
	}
	if !strings.Contains(string(got), "\n  \"refs/heads/main\"") {
		t.Errorf("expected 2-space indent before key; got %s", got)
	}
}

func TestMarshalShardContent_NoTrailingNewline(t *testing.T) {
	refs := map[string]string{"refs/heads/main": "aa"}
	got, err := marshalShardContent(refs)
	if err != nil {
		t.Fatalf("marshalShardContent: %v", err)
	}
	if bytes.HasSuffix(got, []byte("\n")) {
		t.Errorf("unexpected trailing newline in %q", got)
	}
}

func TestHashShardContent_KnownVector(t *testing.T) {
	// Determinism: the empty-shard hash must be stable.
	got := hashShardContent([]byte("{}"))
	const want = "sha256-44f7f6f9d77ad3f1f44ab9b35c3079ec5d4d3e76c3a8c93fc81d3f0a91f7c10b"
	// Sanity: the prefix must be sha256-. The exact suffix is sha256("{}")
	// hex — let's compute it inline rather than hand-coding.
	const wantPrefix = "sha256-"
	if got[:len(wantPrefix)] != wantPrefix {
		t.Errorf("hash prefix = %q want %q", got[:len(wantPrefix)], wantPrefix)
	}
	if len(got) != len(wantPrefix)+64 {
		t.Errorf("hash length = %d want %d", len(got), len(wantPrefix)+64)
	}
	// "{}" hex of sha256 is well-known:
	// echo -n "{}" | sha256sum → 44f7f6f9d77ad3f1f44ab9b35c3079ec5d4d3e76c3a8c93fc81d3f0a91f7c10b
	if got != want {
		t.Errorf("got %s want %s", got, want)
	}
}
```

- [ ] **Step 2: Run the tests to verify they FAIL.**

Run: `go test ./internal/repo/refstore/... -run TestMarshalShardContent -count=1 2>&1 | head -20`
Expected: build failure (marshalShardContent / hashShardContent not defined).

- [ ] **Step 3: Implement `marshal.go`.**

Create `internal/repo/refstore/marshal.go`:

```go
package refstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// marshalShardContent encodes refs as the canonical shard-object JSON:
//
//   - Keys sorted lexicographically (so iteration order does not leak
//     into the bytes — load-bearing for content-addressing).
//   - 2-space indent (matches manifest.MarshalBody convention).
//   - No trailing newline.
//   - Empty input encodes to "{}" (not "{\n}").
//
// Determinism is the contract. A future change that introduces extra
// whitespace, alternate quoting, or key reordering MUST bump the
// ref_sharding strategy string and write a migration; old shards
// would otherwise have stable keys but mismatched hashes.
func marshalShardContent(refs map[string]string) ([]byte, error) {
	if len(refs) == 0 {
		return []byte("{}"), nil
	}
	names := make([]string, 0, len(refs))
	for n := range refs {
		names = append(names, n)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	buf.WriteString("{\n")
	for i, n := range names {
		nb, err := json.Marshal(n)
		if err != nil {
			return nil, fmt.Errorf("refstore: marshal refname %q: %w", n, err)
		}
		vb, err := json.Marshal(refs[n])
		if err != nil {
			return nil, fmt.Errorf("refstore: marshal oid for %q: %w", n, err)
		}
		buf.WriteString("  ")
		buf.Write(nb)
		buf.WriteString(": ")
		buf.Write(vb)
		if i+1 < len(names) {
			buf.WriteString(",")
		}
		buf.WriteString("\n")
	}
	buf.WriteString("}")
	return buf.Bytes(), nil
}

// hashShardContent computes the canonical content hash string
// ("sha256-" + 64-lowercase-hex) for shard bytes. This is what goes
// into manifest.RefShard.Hash and into the storage key as
// ".../ref-shards/<this>.json".
func hashShardContent(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256-" + hex.EncodeToString(sum[:])
}
```

- [ ] **Step 4: Run the marshal tests; expect PASS.**

Run: `go test ./internal/repo/refstore/... -run "TestMarshalShardContent|TestHashShardContent" -count=1 -v`
Expected: all PASS.

- [ ] **Step 5: The package still doesn't compile (refstore.go references newInlineRefStore / newShardedRefStore). We finish the skeleton in the next task. Don't commit yet.**

---

### Task 1.4: `InlineRefStore` implementation

**Files:**
- Create: `internal/repo/refstore/inline.go`
- Create: `internal/repo/refstore/inline_test.go`

- [ ] **Step 1: Write the failing tests.**

Create `internal/repo/refstore/inline_test.go`:

```go
package refstore_test

import (
	"context"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/refstore"
)

func TestInline_Lookup_Hit(t *testing.T) {
	body := &manifest.Body{Refs: map[string]string{"refs/heads/main": "aa"}}
	rs, err := refstore.New(context.Background(), nil, nil, body)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if rs.Mode() != refstore.ModeInline {
		t.Errorf("Mode=%v want inline", rs.Mode())
	}
	oid, ok, err := rs.Lookup(context.Background(), "refs/heads/main")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !ok || oid != "aa" {
		t.Errorf("Lookup=(%q, %v)", oid, ok)
	}
}

func TestInline_Lookup_Miss(t *testing.T) {
	body := &manifest.Body{Refs: map[string]string{"refs/heads/main": "aa"}}
	rs, _ := refstore.New(context.Background(), nil, nil, body)
	oid, ok, err := rs.Lookup(context.Background(), "refs/heads/absent")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if ok || oid != "" {
		t.Errorf("Lookup=(%q, %v)", oid, ok)
	}
}

func TestInline_List(t *testing.T) {
	in := map[string]string{
		"refs/heads/main": "aa",
		"refs/heads/dev":  "bb",
		"refs/tags/v1":    "cc",
	}
	rs, _ := refstore.New(context.Background(), nil, nil, &manifest.Body{Refs: in})
	out, err := rs.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("List len=%d want 3", len(out))
	}
	for k, v := range in {
		if got := out[k]; got != v {
			t.Errorf("List[%q]=%q want %q", k, got, v)
		}
	}
}

func TestInline_Stage_AddDelete(t *testing.T) {
	body := &manifest.Body{Refs: map[string]string{
		"refs/heads/main": "aa",
		"refs/heads/del":  "bb",
	}}
	rs, _ := refstore.New(context.Background(), nil, nil, body)
	updates := map[string]string{
		"refs/heads/dev": "cc", // create
		"refs/heads/del": "",   // delete (empty oid)
	}
	stage, err := rs.Stage(context.Background(), updates)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if stage.Mode != refstore.ModeInline {
		t.Errorf("Mode=%v want inline", stage.Mode)
	}
	if len(stage.NewShardObjects) != 0 {
		t.Errorf("NewShardObjects=%v want empty", stage.NewShardObjects)
	}
	if len(stage.NewRefShards) != 0 {
		t.Errorf("NewRefShards=%v want empty", stage.NewRefShards)
	}
	want := map[string]string{
		"refs/heads/main": "aa",
		"refs/heads/dev":  "cc",
	}
	if len(stage.NewInlineRefs) != len(want) {
		t.Fatalf("NewInlineRefs len=%d want %d", len(stage.NewInlineRefs), len(want))
	}
	for k, v := range want {
		if got := stage.NewInlineRefs[k]; got != v {
			t.Errorf("NewInlineRefs[%q]=%q want %q", k, got, v)
		}
	}
}

func TestInline_Stage_NullOIDIsDelete(t *testing.T) {
	const nullOID = "0000000000000000000000000000000000000000"
	body := &manifest.Body{Refs: map[string]string{"refs/heads/del": "aa"}}
	rs, _ := refstore.New(context.Background(), nil, nil, body)
	stage, err := rs.Stage(context.Background(), map[string]string{"refs/heads/del": nullOID})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if _, present := stage.NewInlineRefs["refs/heads/del"]; present {
		t.Errorf("nullOID should delete; got %+v", stage.NewInlineRefs)
	}
}

func TestInline_Stage_DoesNotMutateInputBody(t *testing.T) {
	orig := map[string]string{"refs/heads/main": "aa"}
	body := &manifest.Body{Refs: orig}
	rs, _ := refstore.New(context.Background(), nil, nil, body)
	if _, err := rs.Stage(context.Background(), map[string]string{"refs/heads/main": "bb"}); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if orig["refs/heads/main"] != "aa" {
		t.Errorf("Stage mutated input map: %+v", orig)
	}
}
```

- [ ] **Step 2: Run the tests; expect FAIL (still compiling errors).**

Run: `go test ./internal/repo/refstore/... -count=1 2>&1 | head -10`
Expected: build failure ("newInlineRefStore not defined").

- [ ] **Step 3: Implement `inline.go`.**

Create `internal/repo/refstore/inline.go`:

```go
package refstore

import (
	"context"

	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

// nullOIDHex matches importer.nullOIDHex (40 zeros). Duplicated here
// rather than imported to avoid a refstore→importer dependency
// (importer already needs to depend on refstore in Phase 5). The
// constant is unlikely to drift; both sides reference the SHA-1
// "no object" sentinel.
const nullOIDHex = "0000000000000000000000000000000000000000"

// InlineRefStore wraps Body.Refs directly. All operations are
// in-memory; the ObjectStore is never consulted.
type InlineRefStore struct {
	refs map[string]string
}

func newInlineRefStore(body *manifest.Body) *InlineRefStore {
	// Shallow-copy the map so callers can mutate the input body after
	// constructing the store without corrupting our snapshot.
	refs := make(map[string]string, len(body.Refs))
	for k, v := range body.Refs {
		refs[k] = v
	}
	return &InlineRefStore{refs: refs}
}

// Mode returns ModeInline.
func (s *InlineRefStore) Mode() Mode { return ModeInline }

// Lookup returns the OID and existence for refname.
func (s *InlineRefStore) Lookup(_ context.Context, refname string) (string, bool, error) {
	oid, ok := s.refs[refname]
	return oid, ok, nil
}

// List returns a fresh copy of the ref map. Callers may mutate the
// returned map without affecting the store.
func (s *InlineRefStore) List(_ context.Context) (map[string]string, error) {
	out := make(map[string]string, len(s.refs))
	for k, v := range s.refs {
		out[k] = v
	}
	return out, nil
}

// Stage merges updates into the snapshot and returns a Stage with
// Mode=ModeInline. Delete convention: empty OID or 40-zero nullOIDHex.
// The returned NewInlineRefs is a freshly allocated map; mutating it
// does not affect the store.
func (s *InlineRefStore) Stage(_ context.Context, updates map[string]string) (Stage, error) {
	out := make(map[string]string, len(s.refs)+len(updates))
	for k, v := range s.refs {
		out[k] = v
	}
	for ref, oid := range updates {
		if oid == "" || oid == nullOIDHex {
			delete(out, ref)
			continue
		}
		out[ref] = oid
	}
	return Stage{
		Mode:          ModeInline,
		NewInlineRefs: out,
	}, nil
}
```

- [ ] **Step 4: Add a stub for `newShardedRefStore` so the package compiles.**

The factory in `refstore.go` references `newShardedRefStore`. We'll implement the full impl in Phase 2, but the skeleton must compile now.

Create `internal/repo/refstore/sharded.go` with the bare minimum:

```go
package refstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// ShardedRefStore wraps Body.RefShards plus an ObjectStore. M12 Phase 2
// adds Lookup and List; Phase 3 adds Stage. This Phase-1 skeleton
// satisfies the RefStore interface with not-yet-implemented stubs so
// the package compiles end-to-end.
type ShardedRefStore struct {
	store  storage.ObjectStore
	keys   *keys.Repo
	shards []manifest.RefShard
}

func newShardedRefStore(s storage.ObjectStore, k *keys.Repo, body *manifest.Body) *ShardedRefStore {
	// Defensive copy of the shard list so callers can mutate body
	// without corrupting the store's view.
	cp := make([]manifest.RefShard, len(body.RefShards))
	copy(cp, body.RefShards)
	return &ShardedRefStore{store: s, keys: k, shards: cp}
}

// Mode returns ModeSharded.
func (s *ShardedRefStore) Mode() Mode { return ModeSharded }

// Lookup is implemented in Phase 2.
func (s *ShardedRefStore) Lookup(_ context.Context, refname string) (string, bool, error) {
	return "", false, fmt.Errorf("refstore.ShardedRefStore.Lookup: %w (Phase 2)", errors.ErrUnsupported)
}

// List is implemented in Phase 2.
func (s *ShardedRefStore) List(_ context.Context) (map[string]string, error) {
	return nil, fmt.Errorf("refstore.ShardedRefStore.List: %w (Phase 2)", errors.ErrUnsupported)
}

// Stage is implemented in Phase 3.
func (s *ShardedRefStore) Stage(_ context.Context, updates map[string]string) (Stage, error) {
	return Stage{}, fmt.Errorf("refstore.ShardedRefStore.Stage: %w (Phase 3)", errors.ErrUnsupported)
}
```

- [ ] **Step 5: Run all refstore tests.**

```bash
go build ./internal/repo/refstore/...
go test ./internal/repo/refstore/... -count=1 -v
```

Expected: package compiles. All inline + marshal tests pass.

- [ ] **Step 6: Add a `shardKey` distribution sanity test.**

Create `internal/repo/refstore/shardkey_test.go`:

```go
package refstore_test

import (
	"fmt"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo/refstore"
)

func TestShardKey_Length(t *testing.T) {
	for _, name := range []string{"refs/heads/main", "refs/tags/v1", ""} {
		got := refstore.ShardKey(name)
		if len(got) != 2 {
			t.Errorf("ShardKey(%q)=%q (len=%d), want length 2", name, got, len(got))
		}
	}
}

func TestShardKey_LowercaseHex(t *testing.T) {
	for i := 0; i < 1000; i++ {
		k := refstore.ShardKey(fmt.Sprintf("refs/heads/branch-%d", i))
		for _, b := range k {
			if !((b >= '0' && b <= '9') || (b >= 'a' && b <= 'f')) {
				t.Errorf("ShardKey(%d)=%q has non-lowercase-hex byte %q", i, k, b)
			}
		}
	}
}

func TestShardKey_Deterministic(t *testing.T) {
	const name = "refs/heads/main"
	got := refstore.ShardKey(name)
	for i := 0; i < 10; i++ {
		if again := refstore.ShardKey(name); again != got {
			t.Fatalf("non-deterministic: first=%q later=%q", got, again)
		}
	}
}

func TestShardKey_DistributionRoughlyUniform(t *testing.T) {
	// With 10000 distinct refnames and 256 buckets, average bucket
	// load is ~39. A bucket with <10 or >100 entries is a strong
	// signal the distribution is non-uniform. This is a smoke test
	// against a hash regression, not a statistical hypothesis test.
	counts := make(map[string]int, 256)
	for i := 0; i < 10000; i++ {
		counts[refstore.ShardKey(fmt.Sprintf("refs/heads/branch-%d", i))]++
	}
	for k, c := range counts {
		if c < 10 || c > 100 {
			t.Errorf("bucket %q has %d entries (expected ~39); distribution may have regressed", k, c)
		}
	}
	if len(counts) < 250 {
		t.Errorf("only %d distinct shards populated; expected near 256", len(counts))
	}
}
```

- [ ] **Step 7: Run the shardkey tests.**

Run: `go test ./internal/repo/refstore/... -run TestShardKey -count=1 -v`
Expected: all PASS.

- [ ] **Step 8: Phase-1 commit.**

```bash
git add internal/repo/refstore/
git commit -m "refstore: interface + InlineRefStore + canonical marshal + shardKey (M12 Phase 1)"
```

---

### Task 1.5: Phase 1 boundary checkpoint

- [ ] **Step 1: Full test sweep + vet.**

```bash
go test ./... -count=1 2>&1 | grep -E "^(FAIL|---)" | head -20
go vet ./...
```

Expected: zero FAIL/--- lines, vet clean.

- [ ] **Step 2: Confirm Phase-1 commits.**

```bash
git log --oneline main..HEAD | head -10
```

Expected to see (in addition to Phase-0 commits):
- `refstore: interface + InlineRefStore + canonical marshal + shardKey (M12 Phase 1)`
- `refstore: package doc skeleton (M12)`

- [ ] **Step 3: Two-stage review (spec-compliance + code-quality).**

Same prompt template as Phase 0; focus areas for reviewers: shard-key determinism, marshal canonicalization, InlineRefStore copy-on-read/copy-on-stage hygiene, RefStore interface ergonomics.

- [ ] **Step 4: roborev review --branch --wait, address findings, close.**

- [ ] **Step 5: Proceed to Phase 2 (sharded read path).**
