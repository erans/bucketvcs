package refstore_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
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
func shardFixture(t *testing.T, perShard map[string]map[string]string) (*manifest.Body, *fakeStore, *keys.Repo) {
	t.Helper()
	k, err := keys.NewRepo("acme", "demo")
	if err != nil {
		t.Fatalf("keys.NewRepo: %v", err)
	}
	store := newFakeStore()
	var shards []manifest.RefShard
	for shardID, refs := range perShard {
		raw, hash, err := refstore.MarshalAndHash(refs)
		if err != nil {
			t.Fatalf("MarshalAndHash: %v", err)
		}
		key := k.RefShardKey(hash)
		store.put(key, raw)
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
	// Put two refs in different shards to verify Lookup fetches the right one.
	mainName := "refs/heads/main"
	mainShard := refstore.ShardKey(mainName)
	devName := refNotInShard("refs/heads/dev", mainShard)
	devShard := refstore.ShardKey(devName)
	body, store, k := shardFixture(t, map[string]map[string]string{
		mainShard: {mainName: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		devShard:  {devName: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
	})
	rs, err := refstore.New(context.Background(), store, k, body)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if rs.Mode() != refstore.ModeSharded {
		t.Errorf("Mode=%v want sharded", rs.Mode())
	}
	oid, ok, err := rs.Lookup(context.Background(), mainName)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !ok || oid != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("Lookup=(%q,%v) want (aa..., true)", oid, ok)
	}
	store.mu.Lock()
	gc := store.getCalls
	store.mu.Unlock()
	if gc != 1 {
		t.Errorf("getCalls=%d want 1 (Lookup should fetch exactly one shard)", gc)
	}
}

func TestSharded_Lookup_MissUnknownShard(t *testing.T) {
	// Body lists only one shard; query a refname whose shard ID is not present.
	mainShard := refstore.ShardKey("refs/heads/main")
	body, store, k := shardFixture(t, map[string]map[string]string{
		mainShard: {"refs/heads/main": "aa"},
	})
	rs, _ := refstore.New(context.Background(), store, k, body)
	probe := refNotInShard("refs/heads/probe", mainShard)
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
	mainName := "refs/heads/main"
	mainShard := refstore.ShardKey(mainName)
	body, store, k := shardFixture(t, map[string]map[string]string{
		mainShard: {mainName: "aa"},
	})
	rs, err := refstore.New(context.Background(), store, k, body)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Use deterministic helper to guarantee a same-shard sibling (no t.Skip).
	sibling := refWithShard("refs/heads/main-other", mainShard)
	oid, ok, err := rs.Lookup(context.Background(), sibling)
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
	store.failOnGet = true
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
	store.overwriteRaw(body.RefShards[0].Key, []byte(`{"refs/heads/main":"BB"}`))
	rs, _ := refstore.New(context.Background(), store, k, body)
	_, _, err := rs.Lookup(context.Background(), "refs/heads/main")
	if !errors.Is(err, refstore.ErrShardCorrupt) {
		t.Fatalf("err = %v, want ErrShardCorrupt", err)
	}
}

func TestSharded_Lookup_CorruptShard_HashBeforeParse(t *testing.T) {
	// Inject bytes that are NOT valid JSON. If fetchShard did parse-then-hash,
	// we'd get a json syntax error instead of ErrShardCorrupt — this asserts
	// the hash check runs first.
	body, store, k := shardFixture(t, map[string]map[string]string{
		refstore.ShardKey("refs/heads/main"): {"refs/heads/main": "aa"},
	})
	store.overwriteRaw(body.RefShards[0].Key, []byte("not json at all"))
	rs, err := refstore.New(context.Background(), store, k, body)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, _, err = rs.Lookup(context.Background(), "refs/heads/main")
	if !errors.Is(err, refstore.ErrShardCorrupt) {
		t.Fatalf("err = %v, want ErrShardCorrupt (hash check must precede parse)", err)
	}
}

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
	// The good shard returns nil, so ErrShardCorrupt is the only non-nil
	// error in the group. errgroup.Wait returns the first non-nil error,
	// which here is unambiguous.
	store.overwriteRaw(body.RefShards[0].Key, []byte(`{"refs/heads/x":"xx"}`))
	rs, _ := refstore.New(context.Background(), store, k, body)
	_, err := rs.List(context.Background())
	if !errors.Is(err, refstore.ErrShardCorrupt) {
		t.Fatalf("err = %v, want ErrShardCorrupt", err)
	}
}

func TestNew_EmptyShardsBodyRoutesToInline(t *testing.T) {
	// A body with explicit empty RefShards routes through InlineRefStore per
	// the factory dispatcher (empty sharded mode is indistinguishable from
	// inline at the wire level, and inline is cheaper). This test pins the
	// dispatcher contract; List on the resulting store must return empty.
	k, err := keys.NewRepo("acme", "demo")
	if err != nil {
		t.Fatalf("keys.NewRepo: %v", err)
	}
	body := &manifest.Body{
		DefaultBranch: "refs/heads/main",
		RefShards:     []manifest.RefShard{},
		RefSharding:   "hash_v1",
	}
	rs, err := refstore.New(context.Background(), newFakeStore(), k, body)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if rs.Mode() != refstore.ModeInline {
		t.Errorf("Mode=%v want inline (empty RefShards must route to inline)", rs.Mode())
	}
	got, err := rs.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List=%v want empty", got)
	}
}

// refWithShard returns a refname under `prefix` whose ShardKey equals `target`.
// Panics if none found in 10000 attempts (unreachable under sha256 with 256 buckets).
func refWithShard(prefix, target string) string {
	for i := 0; i < 10000; i++ {
		cand := fmt.Sprintf("%s-%d", prefix, i)
		if refstore.ShardKey(cand) == target {
			return cand
		}
	}
	panic("refWithShard: no match in 10000 tries (unreachable)")
}

// refNotInShard returns a refname under `prefix` whose ShardKey is NOT `avoid`.
// Panics if none found in 10000 attempts (unreachable under sha256 with 256 buckets).
func refNotInShard(prefix, avoid string) string {
	for i := 0; i < 10000; i++ {
		cand := fmt.Sprintf("%s-%d", prefix, i)
		if refstore.ShardKey(cand) != avoid {
			return cand
		}
	}
	panic("refNotInShard: no match in 10000 tries (unreachable)")
}

func TestSharded_Stage_AddOneRef_DifferentShard(t *testing.T) {
	// The new ref lands in a shard that's NOT main's shard.
	// Result: 2 NewRefShards (unchanged main shard forwarded + new feature shard),
	// 1 NewShardObjects (only the new shard is PUT).
	mainName := "refs/heads/main"
	mainShard := refstore.ShardKey(mainName)
	featName := refNotInShard("refs/heads/feature", mainShard)
	body, store, k := shardFixture(t, map[string]map[string]string{
		mainShard: {mainName: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	})
	rs, _ := refstore.New(context.Background(), store, k, body)
	stage, err := rs.Stage(context.Background(), map[string]string{
		featName: "ffffffffffffffffffffffffffffffffffffffff",
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
	if len(stage.NewShardObjects) != 1 {
		t.Fatalf("NewShardObjects len=%d want 1 (only the new feature shard rewrites)", len(stage.NewShardObjects))
	}
	if len(stage.NewRefShards) != 2 {
		t.Fatalf("NewRefShards len=%d want 2", len(stage.NewRefShards))
	}
}

func TestSharded_Stage_AddOneRef_SameShard(t *testing.T) {
	// The new ref lands in the SAME shard as main.
	// Result: 1 NewRefShards (the touched shard with both refs),
	// 1 NewShardObjects (the touched shard re-PUT with new content).
	mainName := "refs/heads/main"
	mainShard := refstore.ShardKey(mainName)
	siblingName := refWithShard("refs/heads/main-sibling", mainShard)
	body, store, k := shardFixture(t, map[string]map[string]string{
		mainShard: {mainName: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	})
	rs, _ := refstore.New(context.Background(), store, k, body)
	stage, err := rs.Stage(context.Background(), map[string]string{
		siblingName: "ffffffffffffffffffffffffffffffffffffffff",
	})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if len(stage.NewShardObjects) != 1 {
		t.Fatalf("NewShardObjects len=%d want 1", len(stage.NewShardObjects))
	}
	if len(stage.NewRefShards) != 1 {
		t.Fatalf("NewRefShards len=%d want 1", len(stage.NewRefShards))
	}
	// The single new shard must reflect BOTH refs in its RefCount.
	if got := stage.NewRefShards[0].RefCount; got != 2 {
		t.Errorf("NewRefShards[0].RefCount=%d want 2 (main + sibling)", got)
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
	}
	if len(byShard) < 3 {
		t.Fatalf("could not find 3 distinct shards among %d candidates (unreachable under sha256)", len(tries))
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
	aName := "refs/heads/a"
	aShard := refstore.ShardKey(aName)
	bName := refNotInShard("refs/heads/b", aShard)
	bShard := refstore.ShardKey(bName)
	body, store, k := shardFixture(t, map[string]map[string]string{
		aShard: {aName: "aa"},
		bShard: {bName: "bb"},
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
	stage, err := rs.Stage(context.Background(), map[string]string{aName: "cc"})
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
	store.mu.Lock()
	store.failOnGet = true
	store.mu.Unlock()
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
			gw := again.NewShardObjects[j]
			if w.Shard != gw.Shard {
				t.Errorf("non-deterministic Shard order j=%d first=%q later=%q", j, w.Shard, gw.Shard)
			}
			if w.Hash != gw.Hash {
				t.Errorf("non-deterministic Hash j=%d first=%s later=%s", j, w.Hash, gw.Hash)
			}
			if w.Key != gw.Key {
				t.Errorf("non-deterministic Key j=%d first=%s later=%s", j, w.Key, gw.Key)
			}
			if !bytes.Equal(w.Contents, gw.Contents) {
				t.Errorf("non-deterministic Contents j=%d first=%q later=%q", j, w.Contents, gw.Contents)
			}
		}
	}
}

// --- in-memory fake store used by sharded_test.go ---

type fakeStore struct {
	mu        sync.Mutex
	objects   map[string][]byte
	failOnGet bool
	getCalls  int
}

func newFakeStore() *fakeStore { return &fakeStore{objects: map[string][]byte{}} }

func (f *fakeStore) put(key string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = append([]byte(nil), body...)
}

func (f *fakeStore) overwriteRaw(key string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = append([]byte(nil), body...)
}

func (f *fakeStore) Name() string { return "fake" }

func (f *fakeStore) Capabilities() storage.Capabilities {
	return storage.Capabilities{}
}
func (f *fakeStore) Get(_ context.Context, key string, _ *storage.GetOptions) (*storage.Object, error) {
	f.mu.Lock()
	f.getCalls++
	failed := f.failOnGet
	b, ok := f.objects[key]
	f.mu.Unlock()
	if failed {
		return nil, errors.New("fake: forced failure")
	}
	if !ok {
		return nil, storage.ErrNotFound
	}
	// Body bytes already copied at put-time; safe to wrap without further lock.
	return &storage.Object{
		Body:     io.NopCloser(bytes.NewReader(b)),
		Metadata: storage.ObjectMetadata{Key: key, Size: int64(len(b))},
	}, nil
}
func (f *fakeStore) Head(_ context.Context, key string) (*storage.ObjectMetadata, error) {
	f.mu.Lock()
	b, ok := f.objects[key]
	f.mu.Unlock()
	if !ok {
		return nil, storage.ErrNotFound
	}
	return &storage.ObjectMetadata{Key: key, Size: int64(len(b))}, nil
}
func (f *fakeStore) GetRange(context.Context, string, int64, int64) (io.ReadCloser, error) {
	return nil, errors.New("fake: GetRange not implemented")
}
func (f *fakeStore) PutIfAbsent(_ context.Context, key string, body io.Reader, _ *storage.PutOptions) (storage.ObjectVersion, error) {
	b, err := io.ReadAll(body)
	if err != nil {
		return storage.ObjectVersion{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.objects[key]; ok {
		return storage.ObjectVersion{}, storage.ErrAlreadyExists
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
