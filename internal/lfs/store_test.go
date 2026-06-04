package lfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/proxiedurl"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

type fakeStore struct {
	headFn func(context.Context, string) (*storage.ObjectMetadata, error)
	signFn func(context.Context, string, storage.SignedURLOptions) (string, error)
	// signHdr lets tests inject the backend-returned http.Header so the
	// PresignPut merge in store.go can be exercised without standing up
	// a real Azure SAS path.
	signHdr http.Header
}

func (f *fakeStore) Name() string { return "fake" }
func (f *fakeStore) Capabilities() storage.Capabilities {
	return storage.Capabilities{SignedURLs: true}
}
func (f *fakeStore) Get(context.Context, string, *storage.GetOptions) (*storage.Object, error) {
	return nil, errors.New("nope")
}
func (f *fakeStore) Head(ctx context.Context, key string) (*storage.ObjectMetadata, error) {
	return f.headFn(ctx, key)
}
func (f *fakeStore) GetRange(context.Context, string, int64, int64) (io.ReadCloser, error) {
	return nil, errors.New("nope")
}
func (f *fakeStore) PutIfAbsent(context.Context, string, io.Reader, *storage.PutOptions) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, errors.New("nope")
}
func (f *fakeStore) PutIfVersionMatches(context.Context, string, storage.ObjectVersion, io.Reader, *storage.PutOptions) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, errors.New("nope")
}
func (f *fakeStore) DeleteIfVersionMatches(context.Context, string, storage.ObjectVersion) error {
	return errors.New("nope")
}
func (f *fakeStore) List(context.Context, string, *storage.ListOptions) (*storage.ListPage, error) {
	return nil, errors.New("nope")
}
func (f *fakeStore) CreateMultipart(context.Context, string, *storage.MultipartOptions) (storage.MultipartUpload, error) {
	return nil, errors.New("nope")
}
func (f *fakeStore) CompleteMultipartIfAbsent(context.Context, storage.MultipartUpload, []storage.MultipartPart) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, errors.New("nope")
}
func (f *fakeStore) SignedGetURL(ctx context.Context, key string, opts storage.SignedURLOptions) (string, http.Header, error) {
	url, err := f.signFn(ctx, key, opts)
	return url, f.signHdr, err
}

func TestStore_Key(t *testing.T) {
	s := NewStore(&fakeStore{}, "tenants/acme/repos/x/lfs/objects/")
	got := s.Key("abc123")
	if got != "tenants/acme/repos/x/lfs/objects/abc123" {
		t.Fatalf("Key=%q", got)
	}
}

func TestStore_Key_AddsTrailingSlash(t *testing.T) {
	// NewStore should normalize a missing trailing slash.
	s := NewStore(&fakeStore{}, "p")
	if got := s.Key("abc"); got != "p/abc" {
		t.Fatalf("Key=%q want p/abc", got)
	}
}

func TestStore_Head_Found(t *testing.T) {
	fake := &fakeStore{headFn: func(_ context.Context, key string) (*storage.ObjectMetadata, error) {
		if key != "p/abc" {
			t.Fatalf("key=%q", key)
		}
		return &storage.ObjectMetadata{Size: 42}, nil
	}}
	s := NewStore(fake, "p/")
	size, ok, err := s.Head(context.Background(), "abc")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if !ok || size != 42 {
		t.Fatalf("ok=%v size=%d", ok, size)
	}
}

func TestStore_Head_NotFound(t *testing.T) {
	fake := &fakeStore{headFn: func(_ context.Context, _ string) (*storage.ObjectMetadata, error) {
		return nil, storage.ErrNotFound
	}}
	s := NewStore(fake, "p/")
	_, ok, err := s.Head(context.Background(), "abc")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if ok {
		t.Fatalf("ok=true, want false")
	}
}

func TestStore_Head_BackendError(t *testing.T) {
	boom := errors.New("boom")
	fake := &fakeStore{headFn: func(_ context.Context, _ string) (*storage.ObjectMetadata, error) {
		return nil, boom
	}}
	s := NewStore(fake, "p/")
	_, _, err := s.Head(context.Background(), "abc")
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v want boom", err)
	}
}

func TestStore_PresignPut(t *testing.T) {
	fake := &fakeStore{signFn: func(_ context.Context, key string, opts storage.SignedURLOptions) (string, error) {
		if key != "p/abc" {
			t.Fatalf("key=%q", key)
		}
		if opts.Method != "PUT" {
			t.Fatalf("Method=%q", opts.Method)
		}
		if opts.Expires != 5*time.Minute {
			t.Fatalf("Expires=%v", opts.Expires)
		}
		return "https://signed/PUT", nil
	}}
	s := NewStore(fake, "p/")
	url, hdr, err := s.PresignPut(context.Background(), "abc", 42, 5*time.Minute)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	if url != "https://signed/PUT" {
		t.Errorf("url=%q", url)
	}
	if hdr == nil || hdr.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("Content-Type=%q want application/octet-stream", hdr.Get("Content-Type"))
	}
}

// TestStore_PresignPut_ForwardsBackendHeader exercises the merge at
// store.go:81-85: backend-required headers (e.g., Azure's x-ms-blob-type)
// must travel through Store.PresignPut alongside the LFS-supplied
// Content-Type. The Azure smoke covers this end-to-end against Azurite;
// this unit test gives fast feedback if the merge regresses.
func TestStore_PresignPut_ForwardsBackendHeader(t *testing.T) {
	fake := &fakeStore{
		signFn: func(_ context.Context, _ string, _ storage.SignedURLOptions) (string, error) {
			return "https://signed/PUT", nil
		},
		signHdr: http.Header{"X-Ms-Blob-Type": []string{"BlockBlob"}},
	}
	s := NewStore(fake, "p/")
	_, hdr, err := s.PresignPut(context.Background(), "abc", 1, time.Minute)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	if got := hdr.Get("x-ms-blob-type"); got != "BlockBlob" {
		t.Errorf("x-ms-blob-type=%q want BlockBlob", got)
	}
	if got := hdr.Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type=%q want application/octet-stream", got)
	}
}

// TestStore_PresignPut_BackendContentTypeWins exercises the
// collision policy documented at store.go:PresignPut: if the backend
// returns its own Content-Type, the LFS default is replaced (not
// appended-behind-it). No adapter does this today, but the policy
// guards against a silent-drop hazard if a future one starts to.
func TestStore_PresignPut_BackendContentTypeWins(t *testing.T) {
	fake := &fakeStore{
		signFn: func(_ context.Context, _ string, _ storage.SignedURLOptions) (string, error) {
			return "https://signed/PUT", nil
		},
		signHdr: http.Header{"Content-Type": []string{"application/x-custom"}},
	}
	s := NewStore(fake, "p/")
	_, hdr, err := s.PresignPut(context.Background(), "abc", 1, time.Minute)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	if got := hdr.Get("Content-Type"); got != "application/x-custom" {
		t.Errorf("Content-Type=%q want backend value application/x-custom", got)
	}
	if vs := hdr.Values("Content-Type"); len(vs) != 1 {
		t.Errorf("Content-Type values = %v, want exactly one", vs)
	}
}

// TestStore_PresignGet_ForwardsBackendHeader mirrors the PUT-side
// merge test for the GET path. No backend currently sets GET headers,
// but PresignGet has been a verbatim pass-through since M13.2 — this
// guards against an accidental nil-return regression.
func TestStore_PresignGet_ForwardsBackendHeader(t *testing.T) {
	fake := &fakeStore{
		signFn: func(_ context.Context, _ string, _ storage.SignedURLOptions) (string, error) {
			return "https://signed/GET", nil
		},
		signHdr: http.Header{"X-Custom": []string{"forwarded"}},
	}
	s := NewStore(fake, "p/")
	_, hdr, err := s.PresignGet(context.Background(), "abc", time.Minute)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	if got := hdr.Get("X-Custom"); got != "forwarded" {
		t.Errorf("X-Custom=%q want forwarded", got)
	}
}

func TestStore_PresignGet(t *testing.T) {
	fake := &fakeStore{signFn: func(_ context.Context, _ string, opts storage.SignedURLOptions) (string, error) {
		if opts.Method != "GET" {
			t.Fatalf("Method=%q", opts.Method)
		}
		return "https://signed/GET", nil
	}}
	s := NewStore(fake, "p/")
	url, _, err := s.PresignGet(context.Background(), "abc", time.Minute)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	if url != "https://signed/GET" {
		t.Errorf("url=%q", url)
	}
}

func TestStore_PresignPut_PropagatesNotSupported(t *testing.T) {
	fake := &fakeStore{signFn: func(_ context.Context, _ string, _ storage.SignedURLOptions) (string, error) {
		return "", storage.ErrNotSupported
	}}
	s := NewStore(fake, "p/")
	_, _, err := s.PresignPut(context.Background(), "abc", 1, time.Minute)
	if !errors.Is(err, storage.ErrNotSupported) {
		t.Fatalf("err=%v want ErrNotSupported", err)
	}
}

func TestStore_ProxiedPutURL_Stub(t *testing.T) {
	// Stub for P2: returns empty URL today.
	s := NewStore(&fakeStore{}, "p/")
	url, hdr := s.ProxiedPutURL("abc", 1, time.Minute)
	if url != "" || hdr != nil {
		t.Errorf("stub should return empty URL and nil header; got %q %v", url, hdr)
	}
}

func TestStore_ProxiedGetURL_Stub(t *testing.T) {
	s := NewStore(&fakeStore{}, "p/")
	url, hdr := s.ProxiedGetURL("abc", time.Minute)
	if url != "" || hdr != nil {
		t.Errorf("stub should return empty URL and nil header; got %q %v", url, hdr)
	}
}

func TestStore_WithProxied_PUT_URL(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	s := NewStore(&fakeStore{}, "tenants/acme/repos/foo/lfs/objects/").
		WithProxied(key, "https://gw.example", "acme", "foo")
	oid := strings.Repeat("a", 64)
	url, hdr := s.ProxiedPutURL(oid, 100, time.Minute)
	if url == "" {
		t.Fatal("expected non-empty proxied URL")
	}
	if !strings.HasPrefix(url, "https://gw.example/_lfs/acme/foo/"+oid+"?token=") {
		t.Errorf("URL prefix wrong: %s", url)
	}
	if hdr == nil || hdr.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("missing/wrong Content-Type header")
	}
}

func TestStore_WithProxied_GET_URL(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	s := NewStore(&fakeStore{}, "p/").
		WithProxied(key, "https://gw.example", "acme", "foo")
	oid := strings.Repeat("a", 64)
	url, hdr := s.ProxiedGetURL(oid, time.Minute)
	if !strings.HasPrefix(url, "https://gw.example/_lfs/acme/foo/"+oid+"?token=") {
		t.Errorf("URL prefix wrong: %s", url)
	}
	if hdr != nil {
		t.Errorf("expected nil header on GET; got %+v", hdr)
	}
}

func TestStore_NoProxiedConfig_ReturnsEmpty(t *testing.T) {
	// Without WithProxied, the methods are stubs (preserve P0/P1 behavior).
	s := NewStore(&fakeStore{}, "p/")
	oid := strings.Repeat("a", 64)
	if url, _ := s.ProxiedPutURL(oid, 100, time.Minute); url != "" {
		t.Errorf("expected empty URL without WithProxied; got %q", url)
	}
	if url, _ := s.ProxiedGetURL(oid, time.Minute); url != "" {
		t.Errorf("expected empty URL without WithProxied; got %q", url)
	}
	if url, _ := s.ProxiedVerifyURL(oid, time.Minute); url != "" {
		t.Errorf("expected empty URL without WithProxied; got %q", url)
	}
}

func TestStore_WithProxied_VERIFY_URL(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	s := NewStore(&fakeStore{}, "p/").
		WithProxied(key, "https://gw.example", "acme", "foo")
	oid := strings.Repeat("a", 64)
	url, hdr := s.ProxiedVerifyURL(oid, time.Minute)
	if !strings.HasPrefix(url, "https://gw.example/_lfs/acme/foo/"+oid+"?token=") {
		t.Errorf("URL prefix wrong: %s", url)
	}
	authz := hdr.Get("Authorization")
	if !strings.HasPrefix(authz, "Bearer bvtv_") {
		t.Errorf("Authorization=%q, want Bearer bvtv_<...>", authz)
	}
}

func TestStore_ProxiedVerifyURL_Stub(t *testing.T) {
	s := NewStore(&fakeStore{}, "p/")
	url, hdr := s.ProxiedVerifyURL("abc", time.Minute)
	if url != "" || hdr != nil {
		t.Errorf("stub should return empty URL and nil header; got %q %v", url, hdr)
	}
}

func TestStore_ProxiedVerifyURL_TokenIsVerifiable(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	s := NewStore(&fakeStore{}, "p/").
		WithProxied(key, "https://gw.example", "acme", "foo")
	oid := strings.Repeat("a", 64)
	rawURL, _ := s.ProxiedVerifyURL(oid, time.Minute)
	// Extract ?token=... and Verify with kind=lfs-verify, hash=acme/foo/<oid>.
	idx := strings.Index(rawURL, "?token=")
	if idx < 0 {
		t.Fatal("missing ?token= in URL")
	}
	tok := rawURL[idx+len("?token="):]
	got, err := proxiedurl.Verify(key, tok, "lfs-verify", "acme/foo/"+oid, time.Now())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Kind != "lfs-verify" {
		t.Errorf("kind=%q", got.Kind)
	}
}

func TestStore_WithProxied_TokenIsVerifiable(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	s := NewStore(&fakeStore{}, "p/").
		WithProxied(key, "https://gw.example", "acme", "foo")
	oid := strings.Repeat("a", 64)
	url, _ := s.ProxiedPutURL(oid, 100, time.Minute)
	u, err := neturl.Parse(url)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tok := u.Query().Get("token")
	if tok == "" {
		t.Fatal("token missing")
	}
	expectedHash := "acme/foo/" + oid
	decoded, err := proxiedurl.Verify(key, tok, "lfs-put", expectedHash, time.Now())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if decoded.Kind != "lfs-put" || decoded.Hash != expectedHash {
		t.Errorf("decoded=%+v", decoded)
	}
}

func TestStore_WithProxied_PanicsOnShortKey(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on short signing key")
		}
	}()
	NewStore(&fakeStore{}, "p/").WithProxied([]byte{0x01}, "https://gw", "acme", "foo")
}

func TestStore_WithProxied_AcceptsEmptyKey(t *testing.T) {
	// An empty key is the "not configured" signal and must NOT panic.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("unexpected panic: %v", r)
		}
	}()
	s := NewStore(&fakeStore{}, "p/").WithProxied(nil, "", "", "")
	if url, _ := s.ProxiedPutURL("oid", 1, time.Minute); url != "" {
		t.Errorf("expected empty URL with nil key; got %q", url)
	}
}
