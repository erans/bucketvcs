package gateway

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/proxiedurl"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

type fakeStoreNoSign struct{ storage.ObjectStore }

func (f fakeStoreNoSign) Capabilities() storage.Capabilities {
	return storage.Capabilities{SignedURLs: false}
}
func (f fakeStoreNoSign) SignedGetURL(ctx context.Context, key string, opts storage.SignedURLOptions) (string, http.Header, error) {
	return "", nil, storage.ErrNotSupported
}

type fakeStoreWithSign struct {
	storage.ObjectStore
	minted string
	err    error
}

func (f *fakeStoreWithSign) Capabilities() storage.Capabilities {
	return storage.Capabilities{SignedURLs: true}
}
func (f *fakeStoreWithSign) SignedGetURL(ctx context.Context, key string, opts storage.SignedURLOptions) (string, http.Header, error) {
	if f.err != nil {
		return "", nil, f.err
	}
	return f.minted, nil, nil
}

func TestBuildBundleURL_Auto_DirectFirst(t *testing.T) {
	store := &fakeStoreWithSign{minted: "https://signed.example/x"}
	b := URLBuilder{
		Store: store, ProxiedKey: []byte("0123456789abcdef0123456789abcdef"),
		ProxiedBaseURL: "https://gw.example", BundleTTL: 4 * time.Hour, PackTTL: time.Hour,
		Mode: URIModeAuto,
	}
	got, via, err := b.BuildBundleURL(context.Background(), "acme", "site", "sha256-aa", "kk", "sha256:hex")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "https://signed.example/x" || via != "direct" {
		t.Errorf("got=%q via=%q", got, via)
	}
}

func TestBuildBundleURL_Auto_FallsBackToProxied(t *testing.T) {
	b := URLBuilder{
		Store: fakeStoreNoSign{}, ProxiedKey: []byte("0123456789abcdef0123456789abcdef"),
		ProxiedBaseURL: "https://gw.example", BundleTTL: 4 * time.Hour, PackTTL: time.Hour,
		Mode: URIModeAuto,
	}
	got, via, err := b.BuildBundleURL(context.Background(), "acme", "site", "sha256-aa", "kk", "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.HasPrefix(got, "https://gw.example/_bundle/acme/site/sha256-aa?token=") || via != "proxied" {
		t.Errorf("got=%q via=%q", got, via)
	}
	// verify the proxied URL's token round-trips with the same key/kind/composite hash.
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	tok := u.Query().Get("token")
	if _, verr := proxiedurl.Verify(b.ProxiedKey, tok, "bundle", "acme/site/sha256-aa", time.Now()); verr != nil {
		t.Errorf("Verify proxied token: %v", verr)
	}
}

func TestBuildBundleURL_Direct_Required_ErrorsOnNoSupport(t *testing.T) {
	b := URLBuilder{
		Store:          fakeStoreNoSign{},
		ProxiedKey:     []byte("0123456789abcdef0123456789abcdef"),
		ProxiedBaseURL: "https://gw.example",
		Mode:           URIModeDirect,
	}
	_, _, err := b.BuildBundleURL(context.Background(), "acme", "site", "sha256-aa", "kk", "")
	if !errors.Is(err, storage.ErrNotSupported) {
		t.Fatalf("err = %v, want ErrNotSupported", err)
	}
}

func TestBuildBundleURL_Off_ReturnsErr(t *testing.T) {
	b := URLBuilder{Mode: URIModeOff}
	_, _, err := b.BuildBundleURL(context.Background(), "acme", "site", "sha256-aa", "kk", "")
	if err == nil {
		t.Fatalf("expected error in Off mode")
	}
}

// TestBuildBundleURL_Proxied_DirectMode verifies that URIModeProxied
// emits a proxied URL without ever calling SignedGetURL. Catches a
// regression where the buildURL guard `Mode == Direct || Mode == Auto`
// would accidentally include Proxied (e.g., via an enum reorder).
func TestBuildBundleURL_Proxied_DirectMode(t *testing.T) {
	// fakeStoreNeverSign panics on SignedGetURL so we definitively
	// detect any leak into the direct branch.
	type fakeStoreNeverSign struct{ storage.ObjectStore }
	store := fakeStoreNeverSign{}
	b := URLBuilder{
		Store:          store,
		ProxiedKey:     []byte("0123456789abcdef0123456789abcdef"),
		ProxiedBaseURL: "https://gw.example",
		BundleTTL:      4 * time.Hour,
		PackTTL:        time.Hour,
		Mode:           URIModeProxied,
	}
	got, via, err := b.BuildBundleURL(context.Background(), "acme", "site", "sha256-aa", "kk", "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if via != "proxied" {
		t.Errorf("via = %q, want proxied", via)
	}
	if !strings.HasPrefix(got, "https://gw.example/_bundle/acme/site/sha256-aa?token=") {
		t.Errorf("got = %q", got)
	}
}

// TestBuildBundleURL_MultiTenantURLShape pins that proxied URLs embed
// (tenant, repo) as the first two path segments after /_bundle/ — the
// shape introduced in M19 that mirrors /_lfs/<t>/<r>/<oid> from M13.
func TestBuildBundleURL_MultiTenantURLShape(t *testing.T) {
	b := &URLBuilder{
		Store:          fakeStoreNoSign{},
		ProxiedKey:     bytes.Repeat([]byte{0x11}, 32),
		ProxiedBaseURL: "https://gw.example.com",
		BundleTTL:      time.Hour,
		Mode:           URIModeProxied,
	}
	hash := "sha256-" + strings.Repeat("a", 64)
	got, via, err := b.BuildBundleURL(context.Background(), "acme", "site", hash, "irrelevant-key", "")
	if err != nil {
		t.Fatalf("BuildBundleURL: %v", err)
	}
	if via != "proxied" {
		t.Errorf("via=%q, want proxied", via)
	}
	wantPrefix := "https://gw.example.com/_bundle/acme/site/" + hash + "?token="
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("URL=%q, want prefix %q", got, wantPrefix)
	}
}

// TestBuildPackURL_MultiTenantURLShape mirrors the above for packs.
func TestBuildPackURL_MultiTenantURLShape(t *testing.T) {
	b := &URLBuilder{
		Store:          fakeStoreNoSign{},
		ProxiedKey:     bytes.Repeat([]byte{0x22}, 32),
		ProxiedBaseURL: "https://gw.example.com",
		PackTTL:        time.Hour,
		Mode:           URIModeProxied,
	}
	hash := strings.Repeat("c", 40)
	got, via, err := b.BuildPackURL(context.Background(), "acme", "site", hash, "irrelevant-key", "")
	if err != nil {
		t.Fatalf("BuildPackURL: %v", err)
	}
	if via != "proxied" {
		t.Errorf("via=%q, want proxied", via)
	}
	wantPrefix := "https://gw.example.com/_pack/acme/site/" + hash + "?token="
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("URL=%q, want prefix %q", got, wantPrefix)
	}
}

// TestBuildBundleURL_TokenBindsTenantRepoHash mints a URL, extracts the token,
// and verifies it ONLY round-trips against the same composite "<t>/<r>/<h>".
// Tamper of tenant in the composite must fail verify.
func TestBuildBundleURL_TokenBindsTenantRepoHash(t *testing.T) {
	key := bytes.Repeat([]byte{0x33}, 32)
	b := &URLBuilder{
		Store:          fakeStoreNoSign{},
		ProxiedKey:     key,
		ProxiedBaseURL: "https://gw.example.com",
		BundleTTL:      time.Hour,
		Mode:           URIModeProxied,
	}
	hash := "sha256-" + strings.Repeat("e", 64)
	u, _, err := b.BuildBundleURL(context.Background(), "acme", "site", hash, "k", "")
	if err != nil {
		t.Fatalf("BuildBundleURL: %v", err)
	}
	parsed, perr := url.Parse(u)
	if perr != nil {
		t.Fatalf("url.Parse: %v", perr)
	}
	tok := parsed.Query().Get("token")
	if tok == "" {
		t.Fatalf("no token in %q", u)
	}
	composite := "acme/site/" + hash
	if _, vErr := proxiedurl.Verify(key, tok, "bundle", composite, time.Now()); vErr != nil {
		t.Errorf("verify with correct composite: %v", vErr)
	}
	if _, vErr := proxiedurl.Verify(key, tok, "bundle", "other/site/"+hash, time.Now()); vErr == nil {
		t.Errorf("verify with swapped tenant: expected error, got nil")
	}
}
