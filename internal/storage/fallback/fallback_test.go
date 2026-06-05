package fallback_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/fallback"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func twoStores(t *testing.T) (regional, canonical storage.ObjectStore) {
	t.Helper()
	r, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return r, c
}

func put(t *testing.T, s storage.ObjectStore, key, body string) {
	t.Helper()
	if _, err := s.PutIfAbsent(context.Background(), key, strings.NewReader(body), nil); err != nil {
		t.Fatalf("seed %s: %v", key, err)
	}
}

func read(t *testing.T, s storage.ObjectStore, key string) string {
	t.Helper()
	obj, err := s.Get(context.Background(), key, nil)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	defer obj.Body.Close()
	b, err := io.ReadAll(obj.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const rootKey = "tenants/acme/repos/web/manifest/root.json"
const packKey = "tenants/acme/repos/web/packs/canonical/aa/deadbeef.pack"

func TestImmutableKeysPreferRegionalFallBackToCanonical(t *testing.T) {
	regional, canonical := twoStores(t)
	put(t, canonical, packKey, "canonical-bytes")
	fs := fallback.New(regional, canonical, fallback.RootFromRegional, nil)

	if got := read(t, fs, packKey); got != "canonical-bytes" {
		t.Fatalf("fallback read = %q", got)
	}
	put(t, regional, packKey, "regional-bytes")
	if got := read(t, fs, packKey); got != "regional-bytes" {
		t.Fatalf("regional-preferred read = %q", got)
	}
	if _, err := fs.Get(context.Background(), "tenants/acme/repos/web/packs/canonical/bb/nope.pack", nil); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestRootKeyRoutingStrongCurrent(t *testing.T) {
	regional, canonical := twoStores(t)
	put(t, regional, rootKey, "stale-regional-root")
	put(t, canonical, rootKey, "canonical-root")
	fs := fallback.New(regional, canonical, fallback.RootFromCanonical, nil)
	if got := read(t, fs, rootKey); got != "canonical-root" {
		t.Fatalf("strong-current root = %q, want canonical-root", got)
	}
}

func TestRootKeyRoutingBoundedStale(t *testing.T) {
	regional, canonical := twoStores(t)
	put(t, canonical, rootKey, "canonical-root")
	fs := fallback.New(regional, canonical, fallback.RootFromRegional, nil)
	if got := read(t, fs, rootKey); got != "canonical-root" {
		t.Fatalf("first-window root = %q", got)
	}
	put(t, regional, rootKey, "regional-root")
	if got := read(t, fs, rootKey); got != "regional-root" {
		t.Fatalf("bounded-stale root = %q, want regional-root", got)
	}
}

func TestWritesRefused(t *testing.T) {
	regional, canonical := twoStores(t)
	fs := fallback.New(regional, canonical, fallback.RootFromRegional, nil)
	ctx := context.Background()

	if _, err := fs.PutIfAbsent(ctx, packKey, strings.NewReader("x"), nil); !errors.Is(err, storage.ErrReadOnlyReplica) {
		t.Fatalf("PutIfAbsent: want ErrReadOnlyReplica, got %v", err)
	}
	if _, err := fs.PutIfVersionMatches(ctx, packKey, storage.ObjectVersion{}, strings.NewReader("x"), nil); !errors.Is(err, storage.ErrReadOnlyReplica) {
		t.Fatalf("PutIfVersionMatches: want ErrReadOnlyReplica, got %v", err)
	}
	if err := fs.DeleteIfVersionMatches(ctx, packKey, storage.ObjectVersion{}); !errors.Is(err, storage.ErrReadOnlyReplica) {
		t.Fatalf("Delete: want ErrReadOnlyReplica, got %v", err)
	}
	if _, err := fs.CreateMultipart(ctx, packKey, nil); !errors.Is(err, storage.ErrReadOnlyReplica) {
		t.Fatalf("CreateMultipart: want ErrReadOnlyReplica, got %v", err)
	}
	if _, err := fs.CompleteMultipartIfAbsent(ctx, nil, nil); !errors.Is(err, storage.ErrReadOnlyReplica) {
		t.Fatalf("CompleteMultipart: want ErrReadOnlyReplica, got %v", err)
	}
}

func TestListIsRegionalOnly(t *testing.T) {
	regional, canonical := twoStores(t)
	put(t, canonical, packKey, "only-canonical")
	fs := fallback.New(regional, canonical, fallback.RootFromRegional, nil)
	page, err := fs.List(context.Background(), "tenants/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Objects) != 0 {
		t.Fatalf("List leaked canonical objects: %d", len(page.Objects))
	}
}

func TestHeadAndGetRangeFallBack(t *testing.T) {
	regional, canonical := twoStores(t)
	put(t, canonical, packKey, "0123456789")
	fs := fallback.New(regional, canonical, fallback.RootFromRegional, nil)

	if _, err := fs.Head(context.Background(), packKey); err != nil {
		t.Fatalf("Head fallback: %v", err)
	}
	rc, err := fs.GetRange(context.Background(), packKey, 2, 4)
	if err != nil {
		t.Fatalf("GetRange fallback: %v", err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if string(b) != "234" {
		t.Fatalf("GetRange = %q, want 234", b)
	}
}

func TestNameAndCapabilities(t *testing.T) {
	regional, canonical := twoStores(t)
	fs := fallback.New(regional, canonical, fallback.RootFromRegional, nil)
	if want := "replica(localfs->localfs)"; fs.Name() != want {
		t.Fatalf("Name = %q, want %q", fs.Name(), want)
	}
	caps := fs.Capabilities()
	// localfs reports SignedURLs=false; composed store must also be false.
	if caps.SignedURLs {
		t.Fatal("SignedURLs must be false when either backing store lacks support")
	}
}

// spyStore wraps an ObjectStore and records calls to Head and SignedGetURL.
type spyStore struct {
	storage.ObjectStore
	headCalled         int
	signedGetURLCalled int
}

func (s *spyStore) Head(ctx context.Context, key string) (*storage.ObjectMetadata, error) {
	s.headCalled++
	return s.ObjectStore.Head(ctx, key)
}

func (s *spyStore) SignedGetURL(ctx context.Context, key string, opts storage.SignedURLOptions) (string, http.Header, error) {
	s.signedGetURLCalled++
	return s.ObjectStore.SignedGetURL(ctx, key, opts)
}

// TestSignedGetURLRootRouting verifies that in RootFromCanonical mode a root
// key is presigned against the canonical store directly — regional.Head is
// never called and canonical.SignedGetURL is called exactly once.
func TestSignedGetURLRootRouting(t *testing.T) {
	r, c := twoStores(t)
	// Seed root key in both stores so neither path would fail on a missing key.
	put(t, r, rootKey, "regional-root")
	put(t, c, rootKey, "canonical-root")

	spyRegional := &spyStore{ObjectStore: r}
	spyCanonical := &spyStore{ObjectStore: c}

	fs := fallback.New(spyRegional, spyCanonical, fallback.RootFromCanonical, nil)
	ctx := context.Background()
	opts := storage.SignedURLOptions{Method: "GET"}

	// localfs returns ErrNotSupported for signed URLs; that's fine — we only
	// care which store's methods were invoked, not the URL value.
	_, _, _ = fs.SignedGetURL(ctx, rootKey, opts)

	if spyRegional.headCalled != 0 {
		t.Fatalf("regional.Head called %d times; want 0 (root must bypass regional)", spyRegional.headCalled)
	}
	if spyCanonical.signedGetURLCalled != 1 {
		t.Fatalf("canonical.SignedGetURL called %d times; want 1", spyCanonical.signedGetURLCalled)
	}
}

// composed store returns ErrNotSupported (propagated from localfs, which does
// not support signed URLs) regardless of whether the object lives in the
// regional bucket or the canonical bucket.
func TestSignedGetURLPropagatesErrNotSupported(t *testing.T) {
	regional, canonical := twoStores(t)
	put(t, regional, packKey, "regional-bytes")
	put(t, canonical, rootKey, "canonical-root")
	fs := fallback.New(regional, canonical, fallback.RootFromRegional, nil)

	ctx := context.Background()
	opts := storage.SignedURLOptions{Method: "GET"}

	// Object exists regionally: Head succeeds, SignedGetURL hits regional,
	// localfs returns ErrNotSupported.
	_, _, err := fs.SignedGetURL(ctx, packKey, opts)
	if !errors.Is(err, storage.ErrNotSupported) {
		t.Fatalf("SignedGetURL (regional hit): want ErrNotSupported, got %v", err)
	}

	// Object not in regional, falls back to canonical: canonical is also
	// localfs, so also returns ErrNotSupported.
	_, _, err = fs.SignedGetURL(ctx, rootKey, opts)
	if !errors.Is(err, storage.ErrNotSupported) {
		t.Fatalf("SignedGetURL (canonical fallback): want ErrNotSupported, got %v", err)
	}

	// Object missing from both: Head returns ErrNotFound from regional,
	// so SignedGetURL delegates to canonical. Canonical is also localfs
	// and returns ErrNotSupported unconditionally (it never reads the
	// filesystem in SignedGetURL). The composed store surfaces whatever
	// the canonical store returns.
	_, _, err = fs.SignedGetURL(ctx, "tenants/acme/repos/web/packs/canonical/zz/missing.pack", opts)
	if !errors.Is(err, storage.ErrNotSupported) {
		t.Fatalf("SignedGetURL (missing key, canonical localfs): want ErrNotSupported, got %v", err)
	}
}
