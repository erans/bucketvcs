package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/proxiedurl"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestProxiedRoute_Bundle_OK(t *testing.T) {
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rkeys, err := keys.NewRepo("ten", "rep")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("BUNDLE BYTES")
	bundleKey := rkeys.BundleKey("sha256-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899")
	if _, err := store.PutIfAbsent(context.Background(), bundleKey, strings.NewReader(string(body)), nil); err != nil {
		t.Fatal(err)
	}

	key := []byte("0123456789abcdef0123456789abcdef")
	tok, err := proxiedurl.Mint(key, "bundle", "sha256-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", proxiedKeyResolver{rkeys: rkeys}, nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/_bundle/sha256-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899?token=" + tok)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != string(body) {
		t.Errorf("body = %q, want %q", got, body)
	}
}

func TestProxiedRoute_Bundle_Expired_403(t *testing.T) {
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rkeys, err := keys.NewRepo("ten", "rep")
	if err != nil {
		t.Fatal(err)
	}
	bundleKey := rkeys.BundleKey("sha256-cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
	if _, err := store.PutIfAbsent(context.Background(), bundleKey, strings.NewReader("X"), nil); err != nil {
		t.Fatal(err)
	}

	key := []byte("0123456789abcdef0123456789abcdef")
	tok, err := proxiedurl.Mint(key, "bundle", "sha256-cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", proxiedKeyResolver{rkeys: rkeys}, nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/_bundle/sha256-cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc?token=" + tok)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestProxiedRoute_Bundle_Range(t *testing.T) {
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rkeys, err := keys.NewRepo("ten", "rep")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("0123456789ABCDEF")
	bundleKey := rkeys.BundleKey("sha256-1111111111111111111111111111111111111111111111111111111111111111")
	if _, err := store.PutIfAbsent(context.Background(), bundleKey, strings.NewReader(string(body)), nil); err != nil {
		t.Fatal(err)
	}

	key := []byte("0123456789abcdef0123456789abcdef")
	tok, err := proxiedurl.Mint(key, "bundle", "sha256-1111111111111111111111111111111111111111111111111111111111111111", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", proxiedKeyResolver{rkeys: rkeys}, nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/_bundle/sha256-1111111111111111111111111111111111111111111111111111111111111111?token="+tok, nil)
	req.Header.Set("Range", "bytes=4-7")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if string(got) != "4567" {
		t.Errorf("range body = %q, want \"4567\"", got)
	}
	if resp.StatusCode != http.StatusPartialContent {
		t.Errorf("status = %d, want 206", resp.StatusCode)
	}
}

// TestProxiedRoute_Bundle_TamperedToken_403 verifies that a token with a
// corrupted final byte is rejected with 403. This guards the
// security-critical HMAC verification path (the other tests verify happy
// path + expiry, not signature integrity).
func TestProxiedRoute_Bundle_TamperedToken_403(t *testing.T) {
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rkeys, err := keys.NewRepo("ten", "rep")
	if err != nil {
		t.Fatal(err)
	}
	bundleKey := rkeys.BundleKey("sha256-7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a")
	if _, err := store.PutIfAbsent(context.Background(), bundleKey, strings.NewReader("Y"), nil); err != nil {
		t.Fatal(err)
	}

	key := []byte("0123456789abcdef0123456789abcdef")
	tok, err := proxiedurl.Mint(key, "bundle", "sha256-7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	// Flip a middle base64 character — the last character of a no-padding
	// base64url-encoded token may have unused trailing bits (when the
	// input byte count is not a multiple of 3), so swapping only the
	// top-bit half of the base64 alphabet at the end can produce a
	// "tampered" token whose decoded bytes are identical. Swapping a
	// middle character avoids that — every interior char encodes 6
	// useful bits, and we pick a target from a different base64-alphabet
	// quadrant than the source to guarantee at least one decoded bit
	// flips.
	if len(tok) < 4 {
		t.Fatalf("token too short to tamper: len=%d", len(tok))
	}
	mid := len(tok) / 2
	orig := tok[mid]
	// Pick a replacement char that's guaranteed to differ from orig
	// across all 6 bits' top half (so the decoded bytes change even if
	// orig happens to share the bottom bits with the replacement).
	swap := byte('A')
	if orig == 'A' {
		swap = '_' // 63 = 111111 — differs from 'A' (000000) in every bit
	}
	bad := tok[:mid] + string(swap) + tok[mid+1:]

	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", proxiedKeyResolver{rkeys: rkeys}, nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/_bundle/sha256-7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a?token=" + bad)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// proxiedKeyResolver is a test-side implementation of the resolver
// interface that maps (kind, hash) -> storage key for a single repo.
type proxiedKeyResolver struct {
	rkeys *keys.Repo
}

func (p proxiedKeyResolver) BundleKey(hash string) (string, bool) {
	return p.rkeys.BundleKey(hash), true
}
func (p proxiedKeyResolver) PackKey(hash string) (string, bool) {
	return p.rkeys.CanonicalPackKey(hash), true
}

// rejectingResolver simulates a resolver that does not advertise the
// requested hash (e.g., gateway scoped to a different repo). Both
// methods return ok=false; the handler must respond 404 in both cases
// without ever touching the store.
type rejectingResolver struct{}

func (rejectingResolver) BundleKey(string) (string, bool) { return "", false }
func (rejectingResolver) PackKey(string) (string, bool)   { return "", false }

func TestProxiedRoute_Pack_OK(t *testing.T) {
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rkeys, err := keys.NewRepo("ten", "rep")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("PACK BYTES")
	packHash := "0123456789abcdef0123456789abcdef01234567" // 40-hex pack-checksum
	packKey := rkeys.CanonicalPackKey(packHash)
	if _, err := store.PutIfAbsent(context.Background(), packKey, strings.NewReader(string(body)), nil); err != nil {
		t.Fatal(err)
	}

	key := []byte("0123456789abcdef0123456789abcdef")
	tok, _ := proxiedurl.Mint(key, "pack", packHash, time.Now().Add(time.Minute))

	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", proxiedKeyResolver{rkeys: rkeys}, nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/_pack/" + packHash + "?token=" + tok)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != string(body) {
		t.Errorf("body = %q, want %q", got, body)
	}
}

func TestProxiedRoute_Bundle_UnadvertisedHash_404(t *testing.T) {
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	key := []byte("0123456789abcdef0123456789abcdef")
	tok, _ := proxiedurl.Mint(key, "bundle", "sha256-deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef", time.Now().Add(time.Minute))

	// Resolver refuses to map any hash; handler must 404 before any
	// storage call — note we never seeded the object.
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", rejectingResolver{}, nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/_bundle/sha256-deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef?token=" + tok)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestProxiedRoute_Post_405 — only GET and HEAD are advertised; POST and
// other verbs return 405 Method Not Allowed without reaching the
// resolver or the store.
func TestProxiedRoute_Post_405(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	store, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", rejectingResolver{}, nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/_bundle/sha256-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899", "application/octet-stream", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

// TestProxiedRoute_MissingToken_403 — a request without ?token= is
// rejected with 403 even when the hash format and resolver would both
// otherwise pass. Required so an unauthenticated probe cannot tell
// "advertised hash" from "unknown hash" by status code alone.
func TestProxiedRoute_MissingToken_403(t *testing.T) {
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rkeys, _ := keys.NewRepo("ten", "rep")
	hash := "sha256-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	bundleKey := rkeys.BundleKey(hash)
	if _, err := store.PutIfAbsent(context.Background(), bundleKey, strings.NewReader("X"), nil); err != nil {
		t.Fatal(err)
	}

	key := []byte("0123456789abcdef0123456789abcdef")
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", proxiedKeyResolver{rkeys: rkeys}, nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/_bundle/" + hash)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// TestProxiedRoute_HEAD_NoBody — HEAD on a known bundle returns the
// Content-Length without the body, so v2 clients can probe object size
// before issuing a ranged GET.
func TestProxiedRoute_HEAD_NoBody(t *testing.T) {
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rkeys, _ := keys.NewRepo("ten", "rep")
	body := []byte("HEAD BODY BYTES")
	hash := "sha256-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	bundleKey := rkeys.BundleKey(hash)
	if _, err := store.PutIfAbsent(context.Background(), bundleKey, strings.NewReader(string(body)), nil); err != nil {
		t.Fatal(err)
	}

	key := []byte("0123456789abcdef0123456789abcdef")
	tok, _ := proxiedurl.Mint(key, "bundle", hash, time.Now().Add(time.Minute))

	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", proxiedKeyResolver{rkeys: rkeys}, nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodHead, srv.URL+"/_bundle/"+hash+"?token="+tok, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Length"); got != "15" {
		t.Errorf("Content-Length = %q, want %q", got, "15")
	}
	got, _ := io.ReadAll(resp.Body)
	if len(got) != 0 {
		t.Errorf("HEAD body = %q, want empty", got)
	}
}

// TestProxiedRoute_CrossKindToken_403 — a token minted for kind="pack"
// presented at /_bundle/<hash> with a matching hash is rejected with
// 403 (proxiedurl.ErrKindMismatch). Stops a "swap the endpoint, reuse
// the token" attack.
func TestProxiedRoute_CrossKindToken_403(t *testing.T) {
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rkeys, _ := keys.NewRepo("ten", "rep")
	// 40-hex hash is valid for BOTH pack and bundle-after-prefix-strip,
	// but the bundle handler requires "sha256-<64-hex>" so we use a
	// distinct hash that passes the bundle handler's format check.
	hash := "sha256-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	bundleKey := rkeys.BundleKey(hash)
	if _, err := store.PutIfAbsent(context.Background(), bundleKey, strings.NewReader("Z"), nil); err != nil {
		t.Fatal(err)
	}

	key := []byte("0123456789abcdef0123456789abcdef")
	// Mint a token with kind="pack" but for the bundle's hash. Note
	// that Mint accepts any hash regardless of kind; verification at
	// the bundle endpoint compares the path-derived kind to the
	// token's kind.
	tok, _ := proxiedurl.Mint(key, "pack", hash, time.Now().Add(time.Minute))

	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", proxiedKeyResolver{rkeys: rkeys}, nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/_bundle/" + hash + "?token=" + tok)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// TestProxiedRoute_RangeBeyondObject_416 — a range request whose start
// exceeds the object size is mapped to 416 via the
// storage.ErrInvalidArgument sentinel.
func TestProxiedRoute_RangeBeyondObject_416(t *testing.T) {
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rkeys, _ := keys.NewRepo("ten", "rep")
	hash := "sha256-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	bundleKey := rkeys.BundleKey(hash)
	// 16-byte object.
	if _, err := store.PutIfAbsent(context.Background(), bundleKey, strings.NewReader("0123456789ABCDEF"), nil); err != nil {
		t.Fatal(err)
	}

	key := []byte("0123456789abcdef0123456789abcdef")
	tok, _ := proxiedurl.Mint(key, "bundle", hash, time.Now().Add(time.Minute))

	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", proxiedKeyResolver{rkeys: rkeys}, nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/_bundle/"+hash+"?token="+tok, nil)
	// start=200 against a 16-byte object: the handler's Head-then-bound
	// check (proxied_routes.go) fires BEFORE the storage adapter is
	// consulted, so this test exercises the handler's preflight 416 path,
	// not the writeStoreError ErrInvalidArgument mapping. The mapping
	// itself is covered by TestProxiedRoute_RangeAdapterErrInvalidArg_416
	// below via a fake store.
	req.Header.Set("Range", "bytes=200-300")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want 416", resp.StatusCode)
	}
}

func TestNewServer_ProxiedURL_RejectsMissingResolver(t *testing.T) {
	store, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_, err = NewServer(store, Options{
		MirrorDir:            t.TempDir(),
		Version:              "0.1-test",
		AuthStore:            newAnonymousTestAuthStore(t, "acme", "demo", true),
		ProxiedURLSigningKey: []byte("0123456789abcdef0123456789abcdef"),
	})
	if err == nil {
		t.Fatal("want error; got nil")
	}
	if !strings.Contains(err.Error(), "ProxiedKeyResolver") {
		t.Errorf("error = %q; want it to mention ProxiedKeyResolver", err)
	}
}

func TestNewServer_ProxiedURL_RejectsShortKey(t *testing.T) {
	store, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	rkeys, _ := keys.NewRepo("ten", "rep")
	_, err = NewServer(store, Options{
		MirrorDir:            t.TempDir(),
		Version:              "0.1-test",
		AuthStore:            newAnonymousTestAuthStore(t, "acme", "demo", true),
		ProxiedURLSigningKey: []byte("short"),
		ProxiedKeyResolver:   proxiedKeyResolver{rkeys: rkeys},
	})
	if err == nil {
		t.Fatal("want error; got nil")
	}
	if !strings.Contains(err.Error(), "too short") {
		t.Errorf("error = %q; want it to mention too short", err)
	}
}

// fakeRangeStore returns a synthetic Head with a 1000-byte size and a
// caller-injected error from GetRange. Used to exercise the
// writeStoreError mapping without depending on a specific adapter's
// behavior. All other ObjectStore methods are unimplemented (zero-value
// interface embedding) — the handler must not call them on the tested
// paths.
type fakeRangeStore struct {
	storage.ObjectStore
	size       int64
	rangeError error
}

func (f *fakeRangeStore) Head(ctx context.Context, key string) (*storage.ObjectMetadata, error) {
	return &storage.ObjectMetadata{Key: key, Size: f.size}, nil
}
func (f *fakeRangeStore) GetRange(ctx context.Context, key string, start, end int64) (io.ReadCloser, error) {
	return nil, f.rangeError
}

// fakeBundleResolver always reports the same constant key, regardless of
// hash, so we can use it with fakeRangeStore (which doesn't actually
// store anything).
type fakeBundleResolver struct{ key string }

func (f fakeBundleResolver) BundleKey(string) (string, bool) { return f.key, true }
func (f fakeBundleResolver) PackKey(string) (string, bool)   { return f.key, true }

// TestProxiedRoute_RangeAdapterErrInvalidArg_416 covers the
// writeStoreError mapping that the preflight bound check would
// otherwise hide on real adapters. Inject a fake whose Head reports
// size=1000 (so the preflight passes) but whose GetRange returns
// storage.ErrInvalidArgument; assert 416.
func TestProxiedRoute_RangeAdapterErrInvalidArg_416(t *testing.T) {
	hash := "sha256-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	store := &fakeRangeStore{size: 1000, rangeError: storage.ErrInvalidArgument}

	key := []byte("0123456789abcdef0123456789abcdef")
	tok, _ := proxiedurl.Mint(key, "bundle", hash, time.Now().Add(time.Minute))

	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", fakeBundleResolver{key: "fake/key"}, nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/_bundle/"+hash+"?token="+tok, nil)
	req.Header.Set("Range", "bytes=4-7")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want 416", resp.StatusCode)
	}
}

// TestProxiedRoute_RangeAdapterErrNotFound_404 covers the second
// writeStoreError branch via the same fake-store trick.
func TestProxiedRoute_RangeAdapterErrNotFound_404(t *testing.T) {
	hash := "sha256-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	store := &fakeRangeStore{size: 1000, rangeError: storage.ErrNotFound}

	key := []byte("0123456789abcdef0123456789abcdef")
	tok, _ := proxiedurl.Mint(key, "bundle", hash, time.Now().Add(time.Minute))

	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", fakeBundleResolver{key: "fake/key"}, nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/_bundle/"+hash+"?token="+tok, nil)
	req.Header.Set("Range", "bytes=4-7")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestProxiedRoute_RangeAdapterUnknownErr_500 covers the default branch
// of writeStoreError (anything that isn't ErrNotFound or
// ErrInvalidArgument is genuinely unexpected and maps to 500).
func TestProxiedRoute_RangeAdapterUnknownErr_500(t *testing.T) {
	hash := "sha256-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	store := &fakeRangeStore{size: 1000, rangeError: errors.New("synthetic transient")}

	key := []byte("0123456789abcdef0123456789abcdef")
	tok, _ := proxiedurl.Mint(key, "bundle", hash, time.Now().Add(time.Minute))

	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", fakeBundleResolver{key: "fake/key"}, nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/_bundle/"+hash+"?token="+tok, nil)
	req.Header.Set("Range", "bytes=4-7")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

// TestProxiedRoute_MalformedHash_404 exercises validProxiedHash. Each
// case must 404 without ever touching the resolver — the format gate
// rejects before resolver dispatch. We use rejectingResolver so a
// resolver bypass would 200/206 (because rejectingResolver's
// PackKey/BundleKey return ok=false → 404 anyway; a 404 here doesn't
// prove the format gate fired). To make the test definitive, we use a
// resolver that PANICS on call so any leak past the format gate fails
// loudly.
type panickingResolver struct{}

func (panickingResolver) BundleKey(string) (string, bool) {
	panic("validProxiedHash should have rejected this hash before resolver")
}
func (panickingResolver) PackKey(string) (string, bool) {
	panic("validProxiedHash should have rejected this hash before resolver")
}

func TestProxiedRoute_MalformedHash_404(t *testing.T) {
	cases := []struct {
		name, path string
	}{
		{"bundle_wrong_prefix", "/_bundle/blake3-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"},
		{"bundle_short_hash", "/_bundle/sha256-aabbcc"},
		{"bundle_non_hex", "/_bundle/sha256-zzzzccddeeff00112233445566778899aabbccddeeff00112233445566778899"},
		{"bundle_too_long", "/_bundle/sha256-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899ff"},
		{"pack_short", "/_pack/0123abc"},
		{"pack_non_hex", "/_pack/zzzz456789abcdef0123456789abcdef01234567"},
		{"pack_too_long", "/_pack/0123456789abcdef0123456789abcdef0123456789"},
		{"pack_with_dotdot", "/_pack/..0123456789abcdef0123456789abcdef0123"},
	}
	store, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := []byte("0123456789abcdef0123456789abcdef")
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", panickingResolver{}, nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get(srv.URL + tc.path)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("path %q: status = %d, want 404", tc.path, resp.StatusCode)
			}
		})
	}
}

// --- observability helpers + tests ---

// captureLogBuf returns a *bytes.Buffer that receives JSON log lines and the
// *slog.Logger that writes to it. Each log record occupies exactly one line.
func captureLogBuf() (*bytes.Buffer, *slog.Logger) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &buf, logger
}

// logLines splits the captured buffer into individual JSON log lines,
// skipping empty lines (the last element after splitting on '\n').
func logLines(buf *bytes.Buffer) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(buf.String(), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		out = append(out, m)
	}
	return out
}

// findLog returns the first log record matching all key=value pairs, or nil.
func findLog(lines []map[string]any, kvs ...any) map[string]any {
	for _, line := range lines {
		match := true
		for i := 0; i+1 < len(kvs); i += 2 {
			k, _ := kvs[i].(string)
			want := kvs[i+1]
			got, ok := line[k]
			if !ok {
				match = false
				break
			}
			// JSON numbers unmarshal to float64; compare as strings if needed.
			switch v := want.(type) {
			case int:
				if got != float64(v) {
					match = false
				}
			case int64:
				if got != float64(v) {
					match = false
				}
			case float64:
				if got != v {
					match = false
				}
			case bool:
				if got != v {
					match = false
				}
			default:
				if got != want {
					match = false
				}
			}
			if !match {
				break
			}
		}
		if match {
			return line
		}
	}
	return nil
}

// TestProxiedHandler_BundleGetSuccess_EmitsServedMetricsAndAudit verifies that
// a successful full-object bundle GET emits:
//   - bundle_uri_served_total{via=proxied} value=1
//   - bundle_uri_served_bytes{via=proxied} value=<object-size>
//   - proxied.url.served audit event {kind=bundle, status_code=200, range_request=false}
func TestProxiedHandler_BundleGetSuccess_EmitsServedMetricsAndAudit(t *testing.T) {
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rkeys, err := keys.NewRepo("ten", "rep")
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte("B"), 1024)
	hash := "sha256-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	bundleKey := rkeys.BundleKey(hash)
	if _, err := store.PutIfAbsent(context.Background(), bundleKey, bytes.NewReader(body), nil); err != nil {
		t.Fatal(err)
	}

	key := []byte("0123456789abcdef0123456789abcdef")
	tok, err := proxiedurl.Mint(key, "bundle", hash, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	buf, logger := captureLogBuf()
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", proxiedKeyResolver{rkeys: rkeys}, logger)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/_bundle/" + hash + "?token=" + tok)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	got, _ := io.ReadAll(resp.Body)
	if len(got) != 1024 {
		t.Errorf("body length = %d, want 1024", len(got))
	}

	lines := logLines(buf)

	// bundle_uri_served_total{via=proxied} value=1
	if rec := findLog(lines, "msg", "metric", "metric_name", "bundle_uri_served_total", "via", "proxied", "value", int64(1)); rec == nil {
		t.Errorf("missing bundle_uri_served_total metric in logs; got:\n%s", buf.String())
	}
	// bundle_uri_served_bytes{via=proxied} value=1024
	if rec := findLog(lines, "msg", "metric", "metric_name", "bundle_uri_served_bytes", "via", "proxied", "value", int64(1024)); rec == nil {
		t.Errorf("missing bundle_uri_served_bytes metric (value=1024) in logs; got:\n%s", buf.String())
	}
	// proxied.url.served audit event
	if rec := findLog(lines, "event", "proxied.url.served", "kind", "bundle", "status_code", int(200), "range_request", false); rec == nil {
		t.Errorf("missing proxied.url.served audit event in logs; got:\n%s", buf.String())
	}
}

// TestProxiedHandler_PackRangeGetSuccess_EmitsServedMetricsAndAudit verifies
// that a successful pack range GET emits:
//   - pack_uri_served_total{via=proxied} value=1
//   - pack_uri_served_bytes{via=proxied} value=<range-size>
//   - proxied.url.served audit event {kind=pack, status_code=206, range_request=true}
func TestProxiedHandler_PackRangeGetSuccess_EmitsServedMetricsAndAudit(t *testing.T) {
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rkeys, err := keys.NewRepo("ten", "rep")
	if err != nil {
		t.Fatal(err)
	}
	// 100-byte object; we'll request bytes=10-99 (90 bytes).
	body := bytes.Repeat([]byte("P"), 100)
	packHash := "0123456789abcdef0123456789abcdef01234567" // 40-hex
	packKey := rkeys.CanonicalPackKey(packHash)
	if _, err := store.PutIfAbsent(context.Background(), packKey, bytes.NewReader(body), nil); err != nil {
		t.Fatal(err)
	}

	key := []byte("0123456789abcdef0123456789abcdef")
	tok, err := proxiedurl.Mint(key, "pack", packHash, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	buf, logger := captureLogBuf()
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", proxiedKeyResolver{rkeys: rkeys}, logger)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/_pack/"+packHash+"?token="+tok, nil)
	req.Header.Set("Range", "bytes=10-99")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	got, _ := io.ReadAll(resp.Body)
	if len(got) != 90 {
		t.Errorf("body length = %d, want 90", len(got))
	}

	lines := logLines(buf)

	// pack_uri_served_total{via=proxied} value=1
	if rec := findLog(lines, "msg", "metric", "metric_name", "pack_uri_served_total", "via", "proxied", "value", int64(1)); rec == nil {
		t.Errorf("missing pack_uri_served_total metric in logs; got:\n%s", buf.String())
	}
	// pack_uri_served_bytes{via=proxied} value=90
	if rec := findLog(lines, "msg", "metric", "metric_name", "pack_uri_served_bytes", "via", "proxied", "value", int64(90)); rec == nil {
		t.Errorf("missing pack_uri_served_bytes metric (value=90) in logs; got:\n%s", buf.String())
	}
	// proxied.url.served audit event
	if rec := findLog(lines, "event", "proxied.url.served", "kind", "pack", "status_code", int(206), "range_request", true); rec == nil {
		t.Errorf("missing proxied.url.served audit event in logs; got:\n%s", buf.String())
	}
}

// TestProxiedHandler_ErrorPath_DoesNotEmitServedMetrics verifies that 403/404
// error paths do NOT emit served-* metrics or proxied.url.served audit events.
func TestProxiedHandler_ErrorPath_DoesNotEmitServedMetrics(t *testing.T) {
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	hash := "sha256-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"

	key := []byte("0123456789abcdef0123456789abcdef")

	buf, logger := captureLogBuf()
	// Use a resolver that never maps anything, so we get 404 after token validation.
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", rejectingResolver{}, logger)
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Mint a valid token so we pass token verification and reach the 404 resolver path.
	tok, _ := proxiedurl.Mint(key, "bundle", hash, time.Now().Add(time.Minute))
	resp, err := http.Get(srv.URL + "/_bundle/" + hash + "?token=" + tok)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}

	lines := logLines(buf)

	// No served-* metric must appear.
	if rec := findLog(lines, "msg", "metric", "metric_name", "bundle_uri_served_total"); rec != nil {
		t.Errorf("unexpected bundle_uri_served_total metric on error path; got:\n%s", buf.String())
	}
	if rec := findLog(lines, "msg", "metric", "metric_name", "bundle_uri_served_bytes"); rec != nil {
		t.Errorf("unexpected bundle_uri_served_bytes metric on error path; got:\n%s", buf.String())
	}
	// No audit event must appear.
	if rec := findLog(lines, "event", "proxied.url.served"); rec != nil {
		t.Errorf("unexpected proxied.url.served audit on error path; got:\n%s", buf.String())
	}
}

// TestProxiedHandler_TokenInvalid_EmitsMetric_Expired verifies that a token
// past its expiry emits proxied_url_token_invalid_total{reason=expired} and
// returns 403 with body "token expired".
func TestProxiedHandler_TokenInvalid_EmitsMetric_Expired(t *testing.T) {
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rkeys, err := keys.NewRepo("ten", "rep")
	if err != nil {
		t.Fatal(err)
	}
	hash := "sha256-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	bundleKey := rkeys.BundleKey(hash)
	if _, err := store.PutIfAbsent(context.Background(), bundleKey, strings.NewReader("X"), nil); err != nil {
		t.Fatal(err)
	}

	key := []byte("0123456789abcdef0123456789abcdef")
	// Mint a token that expired one minute ago.
	tok, err := proxiedurl.Mint(key, "bundle", hash, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	buf, logger := captureLogBuf()
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", proxiedKeyResolver{rkeys: rkeys}, logger)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/_bundle/" + hash + "?token=" + tok)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if got := strings.TrimRight(string(body), "\n"); got != "token expired" {
		t.Errorf("body = %q, want %q", got, "token expired")
	}

	lines := logLines(buf)
	if rec := findLog(lines, "msg", "metric", "metric_name", "proxied_url_token_invalid_total", "reason", "expired", "value", int64(1)); rec == nil {
		t.Errorf("missing proxied_url_token_invalid_total{reason=expired} metric in logs; got:\n%s", buf.String())
	}
}

// TestProxiedHandler_TokenInvalid_EmitsMetric_KindMismatch verifies that a
// token minted for kind="pack" but presented at a /_bundle/ route emits
// proxied_url_token_invalid_total{reason=kind_mismatch} and returns 403 with
// body "invalid token" (NOT "kind mismatch" — we don't leak the reason).
func TestProxiedHandler_TokenInvalid_EmitsMetric_KindMismatch(t *testing.T) {
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rkeys, err := keys.NewRepo("ten", "rep")
	if err != nil {
		t.Fatal(err)
	}
	hash := "sha256-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	bundleKey := rkeys.BundleKey(hash)
	if _, err := store.PutIfAbsent(context.Background(), bundleKey, strings.NewReader("X"), nil); err != nil {
		t.Fatal(err)
	}

	key := []byte("0123456789abcdef0123456789abcdef")
	// Mint a token with kind="pack" but for the bundle's hash — mismatch at the bundle endpoint.
	tok, err := proxiedurl.Mint(key, "pack", hash, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	buf, logger := captureLogBuf()
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", proxiedKeyResolver{rkeys: rkeys}, logger)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/_bundle/" + hash + "?token=" + tok)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if got := strings.TrimRight(string(body), "\n"); got != "invalid token" {
		t.Errorf("body = %q, want %q (must not leak kind_mismatch reason)", got, "invalid token")
	}

	lines := logLines(buf)
	if rec := findLog(lines, "msg", "metric", "metric_name", "proxied_url_token_invalid_total", "reason", "kind_mismatch", "value", int64(1)); rec == nil {
		t.Errorf("missing proxied_url_token_invalid_total{reason=kind_mismatch} metric in logs; got:\n%s", buf.String())
	}
}

// TestProxiedHandler_TokenInvalid_EmitsMetric_OtherInvalid verifies that a
// token with a corrupted HMAC signature emits
// proxied_url_token_invalid_total{reason=invalid} and returns 403 with body
// "invalid token".
func TestProxiedHandler_TokenInvalid_EmitsMetric_OtherInvalid(t *testing.T) {
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rkeys, err := keys.NewRepo("ten", "rep")
	if err != nil {
		t.Fatal(err)
	}
	hash := "sha256-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	bundleKey := rkeys.BundleKey(hash)
	if _, err := store.PutIfAbsent(context.Background(), bundleKey, strings.NewReader("X"), nil); err != nil {
		t.Fatal(err)
	}

	key := []byte("0123456789abcdef0123456789abcdef")
	tok, err := proxiedurl.Mint(key, "bundle", hash, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	// Corrupt a middle character of the token to produce an HMAC mismatch.
	if len(tok) < 4 {
		t.Fatalf("token too short to tamper: len=%d", len(tok))
	}
	mid := len(tok) / 2
	orig := tok[mid]
	swap := byte('A')
	if orig == 'A' {
		swap = '_'
	}
	bad := tok[:mid] + string(swap) + tok[mid+1:]

	buf, logger := captureLogBuf()
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", proxiedKeyResolver{rkeys: rkeys}, logger)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/_bundle/" + hash + "?token=" + bad)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if got := strings.TrimRight(string(body), "\n"); got != "invalid token" {
		t.Errorf("body = %q, want %q", got, "invalid token")
	}

	lines := logLines(buf)
	if rec := findLog(lines, "msg", "metric", "metric_name", "proxied_url_token_invalid_total", "reason", "invalid", "value", int64(1)); rec == nil {
		t.Errorf("missing proxied_url_token_invalid_total{reason=invalid} metric in logs; got:\n%s", buf.String())
	}
}

// TestProxiedHandler_TokenInvalid_EmitsMetric_Missing verifies that a request
// with no ?token= query parameter emits
// proxied_url_token_invalid_total{reason=missing} and returns 403 with body
// "missing token". Distinct reason from "invalid" because the operational
// remediation (client/integration bug, stale tool, misconfigured proxy
// stripping query params) differs from HMAC/sig/hash failures.
func TestProxiedHandler_TokenInvalid_EmitsMetric_Missing(t *testing.T) {
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rkeys, err := keys.NewRepo("ten", "rep")
	if err != nil {
		t.Fatal(err)
	}
	hash := "sha256-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	bundleKey := rkeys.BundleKey(hash)
	if _, err := store.PutIfAbsent(context.Background(), bundleKey, strings.NewReader("X"), nil); err != nil {
		t.Fatal(err)
	}

	key := []byte("0123456789abcdef0123456789abcdef")

	buf, logger := captureLogBuf()
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", proxiedKeyResolver{rkeys: rkeys}, logger)
	srv := httptest.NewServer(h)
	defer srv.Close()

	// No ?token= query param.
	resp, err := http.Get(srv.URL + "/_bundle/" + hash)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if got := strings.TrimRight(string(body), "\n"); got != "missing token" {
		t.Errorf("body = %q, want %q", got, "missing token")
	}

	lines := logLines(buf)
	if rec := findLog(lines, "msg", "metric", "metric_name", "proxied_url_token_invalid_total", "reason", "missing", "value", int64(1)); rec == nil {
		t.Errorf("missing proxied_url_token_invalid_total{reason=missing} metric in logs; got:\n%s", buf.String())
	}
}

// TestProxiedHandler_HeadRequest_DoesNotEmitServedMetrics verifies that HEAD
// probes (both full-object and range) do not emit served-* metrics or audit.
func TestProxiedHandler_HeadRequest_DoesNotEmitServedMetrics(t *testing.T) {
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rkeys, err := keys.NewRepo("ten", "rep")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("HEAD TEST BYTES")
	hash := "sha256-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	bundleKey := rkeys.BundleKey(hash)
	if _, err := store.PutIfAbsent(context.Background(), bundleKey, bytes.NewReader(body), nil); err != nil {
		t.Fatal(err)
	}

	key := []byte("0123456789abcdef0123456789abcdef")
	tok, _ := proxiedurl.Mint(key, "bundle", hash, time.Now().Add(time.Minute))

	buf, logger := captureLogBuf()
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", proxiedKeyResolver{rkeys: rkeys}, logger)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodHead, srv.URL+"/_bundle/"+hash+"?token="+tok, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	lines := logLines(buf)

	// No served-* metric must appear.
	if rec := findLog(lines, "msg", "metric", "metric_name", "bundle_uri_served_total"); rec != nil {
		t.Errorf("unexpected bundle_uri_served_total on HEAD; got:\n%s", buf.String())
	}
	if rec := findLog(lines, "msg", "metric", "metric_name", "bundle_uri_served_bytes"); rec != nil {
		t.Errorf("unexpected bundle_uri_served_bytes on HEAD; got:\n%s", buf.String())
	}
	if rec := findLog(lines, "event", "proxied.url.served"); rec != nil {
		t.Errorf("unexpected proxied.url.served audit on HEAD; got:\n%s", buf.String())
	}
}
