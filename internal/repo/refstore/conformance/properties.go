package conformance

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sort"
	"sync"
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
		b, h, err := refstore.MarshalAndHash(r)
		if err != nil {
			t.Fatalf("MarshalAndHash: %v", err)
		}
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
	for _, size := range []int{1, 10, 100, 1000} {
		for _, seed := range []int64{1, 2, 3} {
			t.Run(fmt.Sprintf("size=%d/seed=%d", size, seed), func(t *testing.T) {
				refs := genRefs(seed, size)
				inline, err := buildInline(t, refs).List(context.Background())
				if err != nil {
					t.Fatalf("inline List: %v", err)
				}
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
	mu      sync.Mutex
	objects map[string][]byte
}

func newMemoryStore() *memoryStore { return &memoryStore{objects: map[string][]byte{}} }

func (m *memoryStore) put(key string, body []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = append([]byte(nil), body...)
}

func (m *memoryStore) Capabilities() storage.Capabilities { return storage.Capabilities{} }

func (m *memoryStore) Get(_ context.Context, key string, _ *storage.GetOptions) (*storage.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
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
	m.mu.Lock()
	defer m.mu.Unlock()
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
	m.mu.Lock()
	defer m.mu.Unlock()
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
					b, h, err := refstore.MarshalAndHash(r)
					if err != nil {
						t.Fatalf("MarshalAndHash: %v", err)
					}
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
				// If all refs were deleted, NewRefShards may be empty; in that
				// case the body reverts to v1 (no RefSharding tag).
				newBody := &manifest.Body{
					RefShards: stage.NewRefShards,
				}
				if len(stage.NewRefShards) > 0 {
					newBody.RefSharding = "hash_v1"
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
	// Sort the existing keys so per-ref random decisions reproduce
	// deterministically from seed alone. Without this, Go's
	// randomized map iteration would assign different (delete,
	// update, keep) outcomes to the same refnames across runs and
	// failures would not reproduce.
	names := make([]string, 0, len(existing))
	for name := range existing {
		names = append(names, name)
	}
	sort.Strings(names)
	// Pick up to 10% of names to update, 5% to delete.
	for _, name := range names {
		switch r.Intn(20) {
		case 0:
			out[name] = ""
		case 1, 2:
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
					b, h, err := refstore.MarshalAndHash(r)
					if err != nil {
						t.Fatalf("MarshalAndHash: %v", err)
					}
					first[sid] = stamp{bytes: b, hash: h}
				}
				// Repeat the marshal a few times and assert byte-identical output.
				for iter := 0; iter < 25; iter++ {
					for sid, r := range perShard {
						b, h, err := refstore.MarshalAndHash(r)
						if err != nil {
							t.Fatalf("MarshalAndHash iter %d: %v", iter, err)
						}
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
