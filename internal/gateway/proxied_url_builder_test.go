package gateway

import (
	"context"
	"errors"
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
func (f fakeStoreNoSign) SignedGetURL(ctx context.Context, key string, opts storage.SignedURLOptions) (string, error) {
	return "", storage.ErrNotSupported
}

type fakeStoreWithSign struct {
	storage.ObjectStore
	minted string
	err    error
}

func (f *fakeStoreWithSign) Capabilities() storage.Capabilities {
	return storage.Capabilities{SignedURLs: true}
}
func (f *fakeStoreWithSign) SignedGetURL(ctx context.Context, key string, opts storage.SignedURLOptions) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.minted, nil
}

func TestBuildBundleURL_Auto_DirectFirst(t *testing.T) {
	store := &fakeStoreWithSign{minted: "https://signed.example/x"}
	b := URLBuilder{
		Store: store, ProxiedKey: []byte("0123456789abcdef0123456789abcdef"),
		ProxiedBaseURL: "https://gw.example", BundleTTL: 4 * time.Hour, PackTTL: time.Hour,
		Mode: URIModeAuto,
	}
	got, via, err := b.BuildBundleURL(context.Background(), "sha256-aa", "kk", "sha256:hex")
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
	got, via, err := b.BuildBundleURL(context.Background(), "sha256-aa", "kk", "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.HasPrefix(got, "https://gw.example/_bundle/sha256-aa?token=") || via != "proxied" {
		t.Errorf("got=%q via=%q", got, via)
	}
	// verify the proxied URL's token round-trips with the same key/kind/hash.
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	tok := u.Query().Get("token")
	if _, verr := proxiedurl.Verify(b.ProxiedKey, tok, "bundle", "sha256-aa", time.Now()); verr != nil {
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
	_, _, err := b.BuildBundleURL(context.Background(), "sha256-aa", "kk", "")
	if !errors.Is(err, storage.ErrNotSupported) {
		t.Fatalf("err = %v, want ErrNotSupported", err)
	}
}

func TestBuildBundleURL_Off_ReturnsErr(t *testing.T) {
	b := URLBuilder{Mode: URIModeOff}
	_, _, err := b.BuildBundleURL(context.Background(), "sha256-aa", "kk", "")
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
	got, via, err := b.BuildBundleURL(context.Background(), "sha256-aa", "kk", "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if via != "proxied" {
		t.Errorf("via = %q, want proxied", via)
	}
	if !strings.HasPrefix(got, "https://gw.example/_bundle/sha256-aa?token=") {
		t.Errorf("got = %q", got)
	}
}
