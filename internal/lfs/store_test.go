package lfs

import (
	"bytes"
	"context"
	"errors"
	"io"
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
}

func (f *fakeStore) Capabilities() storage.Capabilities { return storage.Capabilities{SignedURLs: true} }
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
func (f *fakeStore) SignedGetURL(ctx context.Context, key string, opts storage.SignedURLOptions) (string, error) {
	return f.signFn(ctx, key, opts)
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
