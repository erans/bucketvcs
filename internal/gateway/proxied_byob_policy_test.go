package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/proxiedurl"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// Wave-2 U-5 regression coverage: proxied bundle/pack store selection must
// fail closed on resolver error (never serve the operator store), while
// genuinely-absent bindings (ErrNoSuchBinding) keep the operator fallback.

func TestProxiedHandler_ResolverError_BundleFailsClosed_500(t *testing.T) {
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	rkeys, err := keys.NewRepo(proxiedTestTenant, proxiedTestRepo)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("OPERATOR STORE BUNDLE BYTES — must never leak on resolver error")
	hash := "sha256-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	if _, err := store.PutIfAbsent(context.Background(), rkeys.BundleKey(hash), strings.NewReader(string(body)), nil); err != nil {
		t.Fatal(err)
	}

	key := []byte("0123456789abcdef0123456789abcdef")
	tok, err := proxiedurl.Mint(key, "bundle", proxiedTestComposite(proxiedTestTenant, proxiedTestRepo, hash), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	resErr := errors.New("byob: binding for ten: authdb connection refused")
	h := NewProxiedHandlerWithResolver(store, &errResolver{err: resErr}, key, "/_bundle/", "/_pack/", nil, nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/_bundle/" + proxiedTestTenant + "/" + proxiedTestRepo + "/" + hash + "?token=" + tok)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("resolver error must fail closed: status=%d, want 500 (U-5 fail-open served operator store content); body=%q", resp.StatusCode, got)
	}
	if strings.Contains(string(got), "OPERATOR STORE") {
		t.Fatalf("operator store bytes leaked on resolver error: %q", got)
	}
}

func TestProxiedHandler_ResolverError_BundleAbsentFromOperator_500(t *testing.T) {
	// Same as above but the hash is NOT in the operator store: the fail-open
	// previously surfaced as a 404 that protocol-v2 clients mis-read as
	// "bundle GC'd", triggering fallback full-clone storms.
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	hash := "sha256-deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	key := []byte("0123456789abcdef0123456789abcdef")
	tok, err := proxiedurl.Mint(key, "bundle", proxiedTestComposite(proxiedTestTenant, proxiedTestRepo, hash), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	h := NewProxiedHandlerWithResolver(store, &errResolver{err: errors.New("byob: decrypt creds: cipher: message authentication failed")}, key, "/_bundle/", "/_pack/", nil, nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/_bundle/" + proxiedTestTenant + "/" + proxiedTestRepo + "/" + hash + "?token=" + tok)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("resolver error must fail closed: status=%d, want 500 (U-5 fail-open returned a GC-storm-triggering 404); body=%q", resp.StatusCode, body)
	}
}

func TestProxiedHandler_ResolverError_PackFailsClosed_500(t *testing.T) {
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	rkeys, err := keys.NewRepo(proxiedTestTenant, proxiedTestRepo)
	if err != nil {
		t.Fatal(err)
	}
	hash := "0123456789abcdef0123456789abcdef01234567"
	if _, err := store.PutIfAbsent(context.Background(), rkeys.CanonicalPackKey(hash), strings.NewReader("pack bytes"), nil); err != nil {
		t.Fatal(err)
	}

	key := []byte("0123456789abcdef0123456789abcdef")
	tok, err := proxiedurl.Mint(key, "pack", proxiedTestComposite(proxiedTestTenant, proxiedTestRepo, hash), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	h := NewProxiedHandlerWithResolver(store, &errResolver{err: errors.New("byob: open store: i/o timeout")}, key, "/_bundle/", "/_pack/", nil, nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/_pack/" + proxiedTestTenant + "/" + proxiedTestRepo + "/" + hash + "?token=" + tok)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("resolver error must fail closed: status=%d, want 500; body=%q", resp.StatusCode, body)
	}
}

func TestProxiedHandler_NoBinding_OperatorFallbackStillAllowed(t *testing.T) {
	// Canary for the one sanctioned fallback: a resolver reporting the
	// genuinely-absent binding (ErrNoSuchBinding-shaped) must keep serving
	// from the operator store.
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	rkeys, err := keys.NewRepo(proxiedTestTenant, proxiedTestRepo)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("operator bundle")
	hash := "sha256-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	if _, err := store.PutIfAbsent(context.Background(), rkeys.BundleKey(hash), strings.NewReader(string(body)), nil); err != nil {
		t.Fatal(err)
	}

	key := []byte("0123456789abcdef0123456789abcdef")
	tok, err := proxiedurl.Mint(key, "bundle", proxiedTestComposite(proxiedTestTenant, proxiedTestRepo, hash), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	h := NewProxiedHandlerWithResolver(store, &errResolver{err: auth.ErrNoSuchBinding}, key, "/_bundle/", "/_pack/", nil, nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/_bundle/" + proxiedTestTenant + "/" + proxiedTestRepo + "/" + hash + "?token=" + tok)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("absent binding must keep operator fallback: status=%d, want 200; body=%q", resp.StatusCode, got)
	}
	if string(got) != string(body) {
		t.Errorf("body = %q, want %q", got, body)
	}
}
