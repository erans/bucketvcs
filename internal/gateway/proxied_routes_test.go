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
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/proxiedurl"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// proxiedTestTenant / proxiedTestRepo are the canonical (tenant, repo)
// pair used by the proxied-route tests in this file. They satisfy
// routenames.ValidateName and keys.NewRepo and are short enough to keep
// the test URLs readable.
const (
	proxiedTestTenant = "ten"
	proxiedTestRepo   = "rep"
)

// proxiedTestComposite returns the composite hash string
// "<tenant>/<repo>/<hash>" that proxiedurl.Mint binds the HMAC to. The
// gateway must verify against the same composite — see
// proxied_routes.go::ServeHTTP.
func proxiedTestComposite(tenant, repo, hash string) string {
	return tenant + "/" + repo + "/" + hash
}

func TestProxiedRoute_Bundle_OK(t *testing.T) {
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rkeys, err := keys.NewRepo(proxiedTestTenant, proxiedTestRepo)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("BUNDLE BYTES")
	hash := "sha256-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	bundleKey := rkeys.BundleKey(hash)
	if _, err := store.PutIfAbsent(context.Background(), bundleKey, strings.NewReader(string(body)), nil); err != nil {
		t.Fatal(err)
	}

	key := []byte("0123456789abcdef0123456789abcdef")
	tok, err := proxiedurl.Mint(key, "bundle", proxiedTestComposite(proxiedTestTenant, proxiedTestRepo, hash), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/_bundle/" + proxiedTestTenant + "/" + proxiedTestRepo + "/" + hash + "?token=" + tok)
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
	rkeys, err := keys.NewRepo(proxiedTestTenant, proxiedTestRepo)
	if err != nil {
		t.Fatal(err)
	}
	hash := "sha256-cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	bundleKey := rkeys.BundleKey(hash)
	if _, err := store.PutIfAbsent(context.Background(), bundleKey, strings.NewReader("X"), nil); err != nil {
		t.Fatal(err)
	}

	key := []byte("0123456789abcdef0123456789abcdef")
	tok, err := proxiedurl.Mint(key, "bundle", proxiedTestComposite(proxiedTestTenant, proxiedTestRepo, hash), time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/_bundle/" + proxiedTestTenant + "/" + proxiedTestRepo + "/" + hash + "?token=" + tok)
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
	rkeys, err := keys.NewRepo(proxiedTestTenant, proxiedTestRepo)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("0123456789ABCDEF")
	hash := "sha256-1111111111111111111111111111111111111111111111111111111111111111"
	bundleKey := rkeys.BundleKey(hash)
	if _, err := store.PutIfAbsent(context.Background(), bundleKey, strings.NewReader(string(body)), nil); err != nil {
		t.Fatal(err)
	}

	key := []byte("0123456789abcdef0123456789abcdef")
	tok, err := proxiedurl.Mint(key, "bundle", proxiedTestComposite(proxiedTestTenant, proxiedTestRepo, hash), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/_bundle/"+proxiedTestTenant+"/"+proxiedTestRepo+"/"+hash+"?token="+tok, nil)
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
	rkeys, err := keys.NewRepo(proxiedTestTenant, proxiedTestRepo)
	if err != nil {
		t.Fatal(err)
	}
	hash := "sha256-7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a"
	bundleKey := rkeys.BundleKey(hash)
	if _, err := store.PutIfAbsent(context.Background(), bundleKey, strings.NewReader("Y"), nil); err != nil {
		t.Fatal(err)
	}

	key := []byte("0123456789abcdef0123456789abcdef")
	tok, err := proxiedurl.Mint(key, "bundle", proxiedTestComposite(proxiedTestTenant, proxiedTestRepo, hash), time.Now().Add(time.Minute))
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

	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/_bundle/" + proxiedTestTenant + "/" + proxiedTestRepo + "/" + hash + "?token=" + bad)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestProxiedRoute_Pack_OK(t *testing.T) {
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rkeys, err := keys.NewRepo(proxiedTestTenant, proxiedTestRepo)
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
	tok, _ := proxiedurl.Mint(key, "pack", proxiedTestComposite(proxiedTestTenant, proxiedTestRepo, packHash), time.Now().Add(time.Minute))

	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/_pack/" + proxiedTestTenant + "/" + proxiedTestRepo + "/" + packHash + "?token=" + tok)
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

// TestProxiedRoute_Bundle_UnstoredHash_404 — token verifies, but the
// object was never written (or has been GC'd). The handler computes the
// storage key and the store returns ErrNotFound -> 404. Replaces the
// pre-M19 "unadvertised by resolver" test now that the key constructor
// is direct (there is no resolver to refuse).
func TestProxiedRoute_Bundle_UnstoredHash_404(t *testing.T) {
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	hash := "sha256-deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

	key := []byte("0123456789abcdef0123456789abcdef")
	tok, _ := proxiedurl.Mint(key, "bundle", proxiedTestComposite(proxiedTestTenant, proxiedTestRepo, hash), time.Now().Add(time.Minute))

	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	// We never seeded the object — Head returns ErrNotFound -> 404.
	resp, err := http.Get(srv.URL + "/_bundle/" + proxiedTestTenant + "/" + proxiedTestRepo + "/" + hash + "?token=" + tok)
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
// store.
func TestProxiedRoute_Post_405(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	store, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/_bundle/"+proxiedTestTenant+"/"+proxiedTestRepo+"/sha256-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899", "application/octet-stream", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

// TestProxiedRoute_MissingToken_403 — a request without ?token= is
// rejected with 403 even when the hash format passes. Required so an
// unauthenticated probe cannot tell "advertised hash" from "unknown
// hash" by status code alone.
func TestProxiedRoute_MissingToken_403(t *testing.T) {
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rkeys, _ := keys.NewRepo(proxiedTestTenant, proxiedTestRepo)
	hash := "sha256-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	bundleKey := rkeys.BundleKey(hash)
	if _, err := store.PutIfAbsent(context.Background(), bundleKey, strings.NewReader("X"), nil); err != nil {
		t.Fatal(err)
	}

	key := []byte("0123456789abcdef0123456789abcdef")
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/_bundle/" + proxiedTestTenant + "/" + proxiedTestRepo + "/" + hash)
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
	rkeys, _ := keys.NewRepo(proxiedTestTenant, proxiedTestRepo)
	body := []byte("HEAD BODY BYTES")
	hash := "sha256-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	bundleKey := rkeys.BundleKey(hash)
	if _, err := store.PutIfAbsent(context.Background(), bundleKey, strings.NewReader(string(body)), nil); err != nil {
		t.Fatal(err)
	}

	key := []byte("0123456789abcdef0123456789abcdef")
	tok, _ := proxiedurl.Mint(key, "bundle", proxiedTestComposite(proxiedTestTenant, proxiedTestRepo, hash), time.Now().Add(time.Minute))

	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodHead, srv.URL+"/_bundle/"+proxiedTestTenant+"/"+proxiedTestRepo+"/"+hash+"?token="+tok, nil)
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
// presented at /_bundle/<t>/<r>/<hash> with a matching hash is rejected
// with 403 (proxiedurl.ErrKindMismatch). Stops a "swap the endpoint,
// reuse the token" attack.
func TestProxiedRoute_CrossKindToken_403(t *testing.T) {
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rkeys, _ := keys.NewRepo(proxiedTestTenant, proxiedTestRepo)
	hash := "sha256-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	bundleKey := rkeys.BundleKey(hash)
	if _, err := store.PutIfAbsent(context.Background(), bundleKey, strings.NewReader("Z"), nil); err != nil {
		t.Fatal(err)
	}

	key := []byte("0123456789abcdef0123456789abcdef")
	// Mint a token with kind="pack" but for the bundle's hash. Mint accepts
	// any hash regardless of kind; verification at the bundle endpoint
	// compares the path-derived kind to the token's kind.
	tok, _ := proxiedurl.Mint(key, "pack", proxiedTestComposite(proxiedTestTenant, proxiedTestRepo, hash), time.Now().Add(time.Minute))

	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/_bundle/" + proxiedTestTenant + "/" + proxiedTestRepo + "/" + hash + "?token=" + tok)
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
	rkeys, _ := keys.NewRepo(proxiedTestTenant, proxiedTestRepo)
	hash := "sha256-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	bundleKey := rkeys.BundleKey(hash)
	// 16-byte object.
	if _, err := store.PutIfAbsent(context.Background(), bundleKey, strings.NewReader("0123456789ABCDEF"), nil); err != nil {
		t.Fatal(err)
	}

	key := []byte("0123456789abcdef0123456789abcdef")
	tok, _ := proxiedurl.Mint(key, "bundle", proxiedTestComposite(proxiedTestTenant, proxiedTestRepo, hash), time.Now().Add(time.Minute))

	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/_bundle/"+proxiedTestTenant+"/"+proxiedTestRepo+"/"+hash+"?token="+tok, nil)
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

func TestNewServer_ProxiedURL_RejectsShortKey(t *testing.T) {
	store, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_, err = NewServer(store, Options{
		MirrorDir:            t.TempDir(),
		Version:              "0.1-test",
		AuthStore:            newAnonymousTestAuthStore(t, "acme", "demo", true),
		ProxiedURLSigningKey: []byte("short"),
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

// TestProxiedRoute_RangeAdapterErrInvalidArg_416 covers the
// writeStoreError mapping that the preflight bound check would
// otherwise hide on real adapters. Inject a fake whose Head reports
// size=1000 (so the preflight passes) but whose GetRange returns
// storage.ErrInvalidArgument; assert 416.
func TestProxiedRoute_RangeAdapterErrInvalidArg_416(t *testing.T) {
	hash := "sha256-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	store := &fakeRangeStore{size: 1000, rangeError: storage.ErrInvalidArgument}

	key := []byte("0123456789abcdef0123456789abcdef")
	tok, _ := proxiedurl.Mint(key, "bundle", proxiedTestComposite(proxiedTestTenant, proxiedTestRepo, hash), time.Now().Add(time.Minute))

	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/_bundle/"+proxiedTestTenant+"/"+proxiedTestRepo+"/"+hash+"?token="+tok, nil)
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
	tok, _ := proxiedurl.Mint(key, "bundle", proxiedTestComposite(proxiedTestTenant, proxiedTestRepo, hash), time.Now().Add(time.Minute))

	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/_bundle/"+proxiedTestTenant+"/"+proxiedTestRepo+"/"+hash+"?token="+tok, nil)
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
	tok, _ := proxiedurl.Mint(key, "bundle", proxiedTestComposite(proxiedTestTenant, proxiedTestRepo, hash), time.Now().Add(time.Minute))

	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/_bundle/"+proxiedTestTenant+"/"+proxiedTestRepo+"/"+hash+"?token="+tok, nil)
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

func TestProxiedRoute_MalformedHash_404(t *testing.T) {
	cases := []struct {
		name, path string
	}{
		{"bundle_wrong_prefix", "/_bundle/" + proxiedTestTenant + "/" + proxiedTestRepo + "/blake3-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"},
		{"bundle_short_hash", "/_bundle/" + proxiedTestTenant + "/" + proxiedTestRepo + "/sha256-aabbcc"},
		{"bundle_non_hex", "/_bundle/" + proxiedTestTenant + "/" + proxiedTestRepo + "/sha256-zzzzccddeeff00112233445566778899aabbccddeeff00112233445566778899"},
		{"bundle_too_long", "/_bundle/" + proxiedTestTenant + "/" + proxiedTestRepo + "/sha256-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899ff"},
		{"pack_short", "/_pack/" + proxiedTestTenant + "/" + proxiedTestRepo + "/0123abc"},
		{"pack_non_hex", "/_pack/" + proxiedTestTenant + "/" + proxiedTestRepo + "/zzzz456789abcdef0123456789abcdef01234567"},
		{"pack_too_long", "/_pack/" + proxiedTestTenant + "/" + proxiedTestRepo + "/0123456789abcdef0123456789abcdef0123456789"},
		{"pack_dotdot_hash", "/_pack/" + proxiedTestTenant + "/" + proxiedTestRepo + "/..0123456789abcdef0123456789abcdef0123456789"},
	}
	store, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Wrap the store so the test fails if validProxiedHash fails to reject
	// any of these inputs and dispatch escapes to a store lookup. Pre-M19
	// the test used a panickingResolver for this purpose; with the resolver
	// removed (M19 Task 4), key computation is pure and would silently
	// return 404 from the store. The wrapping store gives us the ordering
	// proof back.
	storeFail := &failOnAccessStore{ObjectStore: store, t: t}
	key := []byte("0123456789abcdef0123456789abcdef")
	h := NewProxiedHandler(storeFail, key, "/_bundle/", "/_pack/", nil)
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

// failOnAccessStore wraps a real ObjectStore and fails the test if any
// access method (Head/Get/GetRange) is invoked. Used by
// TestProxiedRoute_MalformedHash_404 to prove the validProxiedHash format
// gate rejects requests BEFORE dispatch — without this wrapping a missing
// gate would silently return 404-from-storage indistinguishably from
// 404-from-format-rejection.
type failOnAccessStore struct {
	storage.ObjectStore
	t *testing.T
}

func (f *failOnAccessStore) Head(ctx context.Context, key string) (*storage.ObjectMetadata, error) {
	f.t.Errorf("store.Head(%q) called — malformed hash escaped the format gate", key)
	return nil, storage.ErrNotFound
}

func (f *failOnAccessStore) Get(ctx context.Context, key string, opts *storage.GetOptions) (*storage.Object, error) {
	f.t.Errorf("store.Get(%q) called — malformed hash escaped the format gate", key)
	return nil, storage.ErrNotFound
}

func (f *failOnAccessStore) GetRange(ctx context.Context, key string, start, end int64) (io.ReadCloser, error) {
	f.t.Errorf("store.GetRange(%q) called — malformed hash escaped the format gate", key)
	return nil, storage.ErrNotFound
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
	tok, err := proxiedurl.Mint(key, "bundle", proxiedTestComposite(proxiedTestTenant, proxiedTestRepo, hash), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	buf, logger := captureLogBuf()
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", logger)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/_bundle/" + proxiedTestTenant + "/" + proxiedTestRepo + "/" + hash + "?token=" + tok)
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
	tok, err := proxiedurl.Mint(key, "pack", proxiedTestComposite(proxiedTestTenant, proxiedTestRepo, packHash), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	buf, logger := captureLogBuf()
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", logger)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/_pack/"+proxiedTestTenant+"/"+proxiedTestRepo+"/"+packHash+"?token="+tok, nil)
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
	// The object is never written; a valid token passes verification and
	// the storage Head returns ErrNotFound -> 404.
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", logger)
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Mint a valid token so we pass token verification and reach the 404 store path.
	tok, _ := proxiedurl.Mint(key, "bundle", proxiedTestComposite(proxiedTestTenant, proxiedTestRepo, hash), time.Now().Add(time.Minute))
	resp, err := http.Get(srv.URL + "/_bundle/" + proxiedTestTenant + "/" + proxiedTestRepo + "/" + hash + "?token=" + tok)
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
	tok, err := proxiedurl.Mint(key, "bundle", proxiedTestComposite(proxiedTestTenant, proxiedTestRepo, hash), time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	buf, logger := captureLogBuf()
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", logger)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/_bundle/" + proxiedTestTenant + "/" + proxiedTestRepo + "/" + hash + "?token=" + tok)
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
	tok, err := proxiedurl.Mint(key, "pack", proxiedTestComposite(proxiedTestTenant, proxiedTestRepo, hash), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	buf, logger := captureLogBuf()
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", logger)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/_bundle/" + proxiedTestTenant + "/" + proxiedTestRepo + "/" + hash + "?token=" + tok)
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
	tok, err := proxiedurl.Mint(key, "bundle", proxiedTestComposite(proxiedTestTenant, proxiedTestRepo, hash), time.Now().Add(time.Minute))
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
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", logger)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/_bundle/" + proxiedTestTenant + "/" + proxiedTestRepo + "/" + hash + "?token=" + bad)
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
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", logger)
	srv := httptest.NewServer(h)
	defer srv.Close()

	// No ?token= query param.
	resp, err := http.Get(srv.URL + "/_bundle/" + proxiedTestTenant + "/" + proxiedTestRepo + "/" + hash)
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
	tok, _ := proxiedurl.Mint(key, "bundle", proxiedTestComposite(proxiedTestTenant, proxiedTestRepo, hash), time.Now().Add(time.Minute))

	buf, logger := captureLogBuf()
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", logger)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodHead, srv.URL+"/_bundle/"+proxiedTestTenant+"/"+proxiedTestRepo+"/"+hash+"?token="+tok, nil)
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

// --- M19 multi-tenant tests ---
//
// These pin the M19 URL shape /_<kind>/<tenant>/<repo>/<hash> and the
// composite-hash HMAC binding. The handler must (a) parse exactly 3
// non-empty path segments after the prefix, (b) run routenames.ValidateName
// on tenant + repo BEFORE any token verify, (c) verify the token against
// the composite "<tenant>/<repo>/<hash>", and (d) compute the storage key
// directly via keys.NewRepo(tenant, repo).BundleKey/CanonicalPackKey.

// TestProxiedHandler_BundleMultiTenantURL_OK pins that the M19 URL shape
// /_bundle/<tenant>/<repo>/<hash> with a token bound to the composite
// "<tenant>/<repo>/<hash>" serves the storage key
// keys.NewRepo(tenant, repo).BundleKey(hash).
func TestProxiedHandler_BundleMultiTenantURL_OK(t *testing.T) {
	tenant, repo := "acme", "site"
	hash := "sha256-" + strings.Repeat("ab", 32)
	composite := tenant + "/" + repo + "/" + hash

	key := bytes.Repeat([]byte{0x55}, 32)
	exp := time.Now().Add(time.Hour)
	tok, err := proxiedurl.Mint(key, "bundle", composite, exp)
	if err != nil {
		t.Fatal(err)
	}

	rkeys, err := keys.NewRepo(tenant, repo)
	if err != nil {
		t.Fatal(err)
	}
	storageKey := rkeys.BundleKey(hash)
	bodyBytes := []byte("bundle-payload")
	store, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutIfAbsent(context.Background(), storageKey, bytes.NewReader(bodyBytes), nil); err != nil {
		t.Fatal(err)
	}

	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/_bundle/"+tenant+"/"+repo+"/"+hash+"?token="+url.QueryEscape(tok), nil)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d, body=%s", w.Code, w.Body.String())
	}
	if !bytes.Equal(w.Body.Bytes(), bodyBytes) {
		t.Errorf("body mismatch: got %d bytes, want %d", w.Body.Len(), len(bodyBytes))
	}
}

// TestProxiedHandler_PackMultiTenantURL_OK is the pack analogue.
func TestProxiedHandler_PackMultiTenantURL_OK(t *testing.T) {
	tenant, repo := "acme", "site"
	hash := strings.Repeat("cd", 20) // 40 hex
	composite := tenant + "/" + repo + "/" + hash

	key := bytes.Repeat([]byte{0x56}, 32)
	tok, _ := proxiedurl.Mint(key, "pack", composite, time.Now().Add(time.Hour))
	rkeys, err := keys.NewRepo(tenant, repo)
	if err != nil {
		t.Fatal(err)
	}
	storageKey := rkeys.CanonicalPackKey(hash)
	bodyBytes := []byte("pack-payload-bytes")
	store, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutIfAbsent(context.Background(), storageKey, bytes.NewReader(bodyBytes), nil); err != nil {
		t.Fatal(err)
	}

	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/_pack/"+tenant+"/"+repo+"/"+hash+"?token="+url.QueryEscape(tok), nil)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d, body=%s", w.Code, w.Body.String())
	}
}

// TestProxiedHandler_TamperedTenant_Rejected pins that a token minted
// for (acme, site, hash) cannot be replayed against (other, site, hash) —
// the HMAC binds the composite.
func TestProxiedHandler_TamperedTenant_Rejected(t *testing.T) {
	hash := "sha256-" + strings.Repeat("ab", 32)
	composite := "acme/site/" + hash
	key := bytes.Repeat([]byte{0x66}, 32)
	tok, _ := proxiedurl.Mint(key, "bundle", composite, time.Now().Add(time.Hour))
	store, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/_bundle/other/site/"+hash+"?token="+url.QueryEscape(tok), nil)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("code=%d, want 403 (HMAC binds tenant)", w.Code)
	}
}

// TestProxiedHandler_TamperedRepo_Rejected — repo segment swap.
func TestProxiedHandler_TamperedRepo_Rejected(t *testing.T) {
	hash := "sha256-" + strings.Repeat("ab", 32)
	composite := "acme/site/" + hash
	key := bytes.Repeat([]byte{0x77}, 32)
	tok, _ := proxiedurl.Mint(key, "bundle", composite, time.Now().Add(time.Hour))
	store, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/_bundle/acme/elsewhere/"+hash+"?token="+url.QueryEscape(tok), nil)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("code=%d, want 403 (HMAC binds repo)", w.Code)
	}
}

// TestProxiedHandler_PackTamperedTenant_Rejected — pack analogue of
// TestProxiedHandler_TamperedTenant_Rejected. Token binding is kind-agnostic
// (same Verify path) but the kind label is part of the HMAC input, so this
// proves the same protection applies on the /_pack/ endpoint.
func TestProxiedHandler_PackTamperedTenant_Rejected(t *testing.T) {
	hash := strings.Repeat("ab", 20) // 40 hex
	composite := "acme/site/" + hash
	key := bytes.Repeat([]byte{0xA1}, 32)
	tok, _ := proxiedurl.Mint(key, "pack", composite, time.Now().Add(time.Hour))
	store, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/_pack/other/site/"+hash+"?token="+url.QueryEscape(tok), nil)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("code=%d, want 403 (HMAC binds tenant on pack endpoint)", w.Code)
	}
}

// TestProxiedHandler_PackTamperedRepo_Rejected — repo swap, pack endpoint.
func TestProxiedHandler_PackTamperedRepo_Rejected(t *testing.T) {
	hash := strings.Repeat("cd", 20)
	composite := "acme/site/" + hash
	key := bytes.Repeat([]byte{0xA2}, 32)
	tok, _ := proxiedurl.Mint(key, "pack", composite, time.Now().Add(time.Hour))
	store, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/_pack/acme/elsewhere/"+hash+"?token="+url.QueryEscape(tok), nil)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("code=%d, want 403 (HMAC binds repo on pack endpoint)", w.Code)
	}
}

// TestProxiedHandler_BadTenantName_Rejected — routenames.ValidateName must
// filter ".." etc. before any store lookup. Even a HMAC-valid token is
// irrelevant because the name validation runs BEFORE token verify; we
// expect a non-200 (typically 404 from http.NotFound).
func TestProxiedHandler_BadTenantName_Rejected(t *testing.T) {
	hash := "sha256-" + strings.Repeat("ab", 32)
	key := bytes.Repeat([]byte{0x88}, 32)
	store, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", nil)
	// Token doesn't matter; we just want to confirm the bad name is rejected.
	tok, _ := proxiedurl.Mint(key, "bundle", "../site/"+hash, time.Now().Add(time.Hour))
	// We send the literal ".." segment in the URL — but ServeMux/RequestURI
	// will normalize on the net/http server side. Use http.NewRequest with
	// a raw path so the handler sees the literal value.
	r := httptest.NewRequest("GET", "/_bundle/dummy/dummy/dummy", nil)
	r.URL.Path = "/_bundle/../site/" + hash
	r.URL.RawQuery = "token=" + url.QueryEscape(tok)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == http.StatusOK {
		t.Errorf("bad tenant should be rejected, got 200")
	}
}

// TestProxiedHandler_MissingPathSegments — /_bundle/<just-tenant>/<just-repo>
// has only 2 of 3 required segments. The handler must reject with non-200.
func TestProxiedHandler_MissingPathSegments(t *testing.T) {
	key := bytes.Repeat([]byte{0x99}, 32)
	store, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/_bundle/acme/site", nil) // no hash segment
	h.ServeHTTP(w, r)
	if w.Code == http.StatusOK {
		t.Errorf("missing hash segment should reject; got 200")
	}
}

// TestProxiedHandler_AuditIncludesTenantRepo asserts the proxied.url.served
// audit event AND the bundle_uri_served_total / bundle_uri_served_bytes
// metrics carry tenant + repo labels (M19 Task 6 multi-tenant observability).
// Without these labels operators cannot attribute serve volume to a specific
// (tenant, repo); the labels gain meaning now that the multi-tenant URL shape
// allows one gateway to serve many tenants.
func TestProxiedHandler_AuditIncludesTenantRepo(t *testing.T) {
	tenant, repo := "acme", "site"
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rkeys, err := keys.NewRepo(tenant, repo)
	if err != nil {
		t.Fatal(err)
	}
	hash := "sha256-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	bundleKey := rkeys.BundleKey(hash)
	body := []byte("data")
	if _, err := store.PutIfAbsent(context.Background(), bundleKey, bytes.NewReader(body), nil); err != nil {
		t.Fatal(err)
	}

	key := []byte("0123456789abcdef0123456789abcdef")
	tok, err := proxiedurl.Mint(key, "bundle", proxiedTestComposite(tenant, repo, hash), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	buf, logger := captureLogBuf()
	h := NewProxiedHandler(store, key, "/_bundle/", "/_pack/", logger)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/_bundle/"+tenant+"/"+repo+"/"+hash+"?token="+url.QueryEscape(tok), nil)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("setup: code=%d body=%s", w.Code, w.Body.String())
	}

	logged := buf.String()
	if !strings.Contains(logged, `"tenant":"acme"`) {
		t.Errorf("logs missing tenant=acme. Output:\n%s", logged)
	}
	if !strings.Contains(logged, `"repo":"site"`) {
		t.Errorf("logs missing repo=site. Output:\n%s", logged)
	}

	// Verify the audit event specifically carries tenant + repo.
	lines := logLines(buf)
	if rec := findLog(lines, "event", "proxied.url.served", "tenant", "acme", "repo", "site"); rec == nil {
		t.Errorf("proxied.url.served audit missing tenant/repo attrs; got:\n%s", logged)
	}
	// Verify served-total metric carries tenant + repo.
	if rec := findLog(lines, "msg", "metric", "metric_name", "bundle_uri_served_total", "tenant", "acme", "repo", "site"); rec == nil {
		t.Errorf("bundle_uri_served_total metric missing tenant/repo labels; got:\n%s", logged)
	}
	// Verify served-bytes metric carries tenant + repo.
	if rec := findLog(lines, "msg", "metric", "metric_name", "bundle_uri_served_bytes", "tenant", "acme", "repo", "site"); rec == nil {
		t.Errorf("bundle_uri_served_bytes metric missing tenant/repo labels; got:\n%s", logged)
	}
}
