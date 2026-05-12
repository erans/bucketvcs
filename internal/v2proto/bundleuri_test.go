package v2proto

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

func TestHandleBundleURI_Current_Advertises(t *testing.T) {
	const validHex = "deadbeef0123456789abcdef0123456789abcdef0123456789abcdef01234567"
	body := manifest.Body{
		Refs: map[string]string{"refs/heads/main": "tip"},
		Bundles: []manifest.BundleEntry{{
			ID: "b1", Kind: "full_default", Ref: "refs/heads/main",
			TipOID: "tip", CoversManifestVersion: 1,
			GeneratedAt: time.Now().Add(-time.Minute).Format(time.RFC3339),
			BundleHash: "sha256-" + validHex, BundleKey: "bk",
		}},
	}
	var sawExpected string
	var out bytes.Buffer
	err := HandleBundleURI(context.Background(), &out, BundleURIDeps{
		Body:        body,
		Now:         time.Now(),
		WarmCommits: 100, WarmAge: 24 * time.Hour,
		IsAncestor: func(a, d string, max int) bool { return true },
		WalkBack:   func(from, target string, max int) (int, error) { return 0, nil },
		BuildURL: func(_ context.Context, hash, key, expected string) (string, error) {
			sawExpected = expected
			return "https://example/u", nil
		},
	})
	if err != nil {
		t.Fatalf("HandleBundleURI: %v", err)
	}
	if !strings.Contains(out.String(), "bundle.b1.uri=https://example/u") {
		t.Fatalf("response missing bundle.b1.uri:\n%s", out.String())
	}
	// HandleBundleURI must thread the well-formed BundleHash through to
	// BuildURL as "sha256:<hex>" — regression-guard for the hash-extraction
	// path (bundleHashHex itself is unit-tested independently).
	if want := "sha256:" + validHex; sawExpected != want {
		t.Fatalf("BuildURL got expectedHash=%q, want %q", sawExpected, want)
	}
}

func TestHandleBundleURI_Stale_Omits(t *testing.T) {
	body := manifest.Body{
		Refs: map[string]string{"refs/heads/main": "new-tip"},
		Bundles: []manifest.BundleEntry{{
			ID: "b1", Kind: "full_default", Ref: "refs/heads/main",
			TipOID: "old-tip", GeneratedAt: time.Now().Add(-25 * time.Hour).Format(time.RFC3339),
		}},
	}
	var out bytes.Buffer
	err := HandleBundleURI(context.Background(), &out, BundleURIDeps{
		Body:        body,
		Now:         time.Now(),
		WarmCommits: 100, WarmAge: 24 * time.Hour,
		IsAncestor: func(a, d string, max int) bool { return true },
		WalkBack:   func(from, target string, max int) (int, error) { return 5, nil },
		BuildURL:   func(_ context.Context, hash, key, expected string) (string, error) { return "https://x", nil },
	})
	if err != nil {
		t.Fatalf("HandleBundleURI: %v", err)
	}
	if strings.Contains(out.String(), "bundle.b1.uri=") {
		t.Fatalf("stale bundle should not be advertised:\n%s", out.String())
	}
}

func TestHandleBundleURI_RefDeleted_Omits(t *testing.T) {
	body := manifest.Body{
		// refs/heads/main is absent from the manifest — the bundle's
		// covered ref has been deleted since generation.
		Refs: map[string]string{},
		Bundles: []manifest.BundleEntry{{
			ID: "b1", Kind: "full_default", Ref: "refs/heads/main",
			TipOID: "tip", GeneratedAt: time.Now().Format(time.RFC3339),
		}},
	}
	var out bytes.Buffer
	err := HandleBundleURI(context.Background(), &out, BundleURIDeps{
		Body:        body,
		Now:         time.Now(),
		WarmCommits: 100, WarmAge: 24 * time.Hour,
		IsAncestor: func(a, d string, max int) bool { return true },
		WalkBack:   func(from, target string, max int) (int, error) { return 0, nil },
		BuildURL: func(_ context.Context, hash, key, expected string) (string, error) {
			t.Fatalf("BuildURL must not be invoked when ref is deleted")
			return "", nil
		},
	})
	if err != nil {
		t.Fatalf("HandleBundleURI: %v", err)
	}
	if strings.Contains(out.String(), "bundle.b1.uri=") {
		t.Fatalf("deleted-ref bundle must not be advertised:\n%s", out.String())
	}
}

func TestHandleBundleURI_BuildURLError_Omits(t *testing.T) {
	body := manifest.Body{
		Refs: map[string]string{"refs/heads/main": "tip"},
		Bundles: []manifest.BundleEntry{{
			ID: "b1", Kind: "full_default", Ref: "refs/heads/main",
			TipOID: "tip", GeneratedAt: time.Now().Format(time.RFC3339),
		}},
	}
	var out bytes.Buffer
	err := HandleBundleURI(context.Background(), &out, BundleURIDeps{
		Body:        body,
		Now:         time.Now(),
		WarmCommits: 100, WarmAge: 24 * time.Hour,
		IsAncestor: func(a, d string, max int) bool { return true },
		WalkBack:   func(from, target string, max int) (int, error) { return 0, nil },
		BuildURL: func(_ context.Context, hash, key, expected string) (string, error) {
			return "", errors.New("signed URL backend unavailable")
		},
	})
	if err != nil {
		t.Fatalf("HandleBundleURI: %v", err)
	}
	if strings.Contains(out.String(), "bundle.b1.uri=") {
		t.Fatalf("BuildURL error must not be advertised:\n%s", out.String())
	}
}

func TestHandleBundleURI_BuildURLEmptyString_Omits(t *testing.T) {
	body := manifest.Body{
		Refs: map[string]string{"refs/heads/main": "tip"},
		Bundles: []manifest.BundleEntry{{
			ID: "b1", Kind: "full_default", Ref: "refs/heads/main",
			TipOID: "tip", GeneratedAt: time.Now().Format(time.RFC3339),
		}},
	}
	var out bytes.Buffer
	err := HandleBundleURI(context.Background(), &out, BundleURIDeps{
		Body:        body,
		Now:         time.Now(),
		WarmCommits: 100, WarmAge: 24 * time.Hour,
		IsAncestor: func(a, d string, max int) bool { return true },
		WalkBack:   func(from, target string, max int) (int, error) { return 0, nil },
		BuildURL: func(_ context.Context, hash, key, expected string) (string, error) {
			return "", nil // misconfigured backend returns empty URL with no error
		},
	})
	if err != nil {
		t.Fatalf("HandleBundleURI: %v", err)
	}
	if strings.Contains(out.String(), "bundle.b1.uri=") {
		t.Fatalf("empty URL must not be advertised (would emit malformed bundle.b1.uri=):\n%s", out.String())
	}
}
