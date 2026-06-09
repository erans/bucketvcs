package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/browsemodel"
)

// TestWeb_AliasRedirect verifies the 302 alias-redirect behaviour end-to-end:
// an old repo name that has been renamed away redirects to the new name,
// preserving sub-path and query, while truly-missing repos still 404.
//
// Test harness: fakeStore with resolveAlias func + ResolveAlias method (added
// in middleware_test.go). The type-assertion s.store.(auth.RepoAliasResolver)
// succeeds because *fakeStore now implements that interface.
func TestWeb_AliasRedirect(t *testing.T) {
	// Fixture: tenant=acme, old name=a (aliased → b), live repo=b.
	// GetVisibleRepo: "acme/a" is not visible (it was renamed away).
	//                 "acme/b" is visible (the live repo).
	// GetRepoFlags: "acme/b" exists; "acme/a" does not.
	// resolveAlias: "acme/a" → "b", ok=true; anything else → not found.

	makeStore := func() *fakeStore {
		store := newFakeStore()
		// "acme/a" is not visible (renamed away); "acme/b" is visible (live repo).
		store.getVisibleRepo = func(ctx context.Context, actor *auth.Actor, tenant, name string) (*Repo, error) {
			if tenant == "acme" && name == "b" {
				return &Repo{Tenant: tenant, Name: name}, nil
			}
			return nil, errNotVisible
		}
		store.getRepoFlags = func(ctx context.Context, tenant, repo string) (auth.RepoFlags, error) {
			if tenant == "acme" && repo == "b" {
				return auth.RepoFlags{}, nil
			}
			return auth.RepoFlags{}, auth.ErrNoSuchRepo
		}
		store.resolveAlias = func(ctx context.Context, tenant, name string) (string, bool, error) {
			if tenant == "acme" && name == "a" {
				return "b", true, nil
			}
			return "", false, nil
		}
		return store
	}

	// Content store: enough to make browse work for "acme/b" (the live repo).
	content := &fakeContent{refs: browsemodel.Refs{
		Default:  "main",
		Branches: []browsemodel.RefInfo{{Name: "main", OID: "abcdefabcdefabcdefabcdefabcdefabcdefabcd"}},
	}}

	t.Run("GET /acme/a (browse) → 302 to /acme/b", func(t *testing.T) {
		h := NewHandler(Deps{Store: makeStore(), Content: content})
		req := httptest.NewRequest(http.MethodGet, "/acme/a", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusFound {
			t.Fatalf("status=%d want 302; body=%s", rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "/acme/b" {
			t.Fatalf("Location=%q want /acme/b", loc)
		}
	})

	t.Run("GET /acme/a/settings → 302 to /acme/b/settings", func(t *testing.T) {
		// Settings redirect: only global admins reach the alias-redirect site
		// (canAdminRepo short-circuits for non-admins with 404).
		// Use adminSession so canAdminRepo passes.
		store := makeStore()
		h := NewHandler(Deps{Store: store, Content: content})
		req := httptest.NewRequest(http.MethodGet, "/acme/a/settings", nil)
		addSessionCookie(t, req, store, adminSession())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusFound {
			t.Fatalf("status=%d want 302; body=%s", rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "/acme/b/settings" {
			t.Fatalf("Location=%q want /acme/b/settings", loc)
		}
	})

	t.Run("GET /acme/a/tree/main?x=1 → 302 to /acme/b/tree/main?x=1", func(t *testing.T) {
		h := NewHandler(Deps{Store: makeStore(), Content: content})
		req := httptest.NewRequest(http.MethodGet, "/acme/a/tree/main?x=1", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusFound {
			t.Fatalf("status=%d want 302; body=%s", rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "/acme/b/tree/main?x=1" {
			t.Fatalf("Location=%q want /acme/b/tree/main?x=1", loc)
		}
	})

	t.Run("GET /acme/nope → 404 (no alias)", func(t *testing.T) {
		h := NewHandler(Deps{Store: makeStore(), Content: content})
		req := httptest.NewRequest(http.MethodGet, "/acme/nope", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status=%d want 404", rec.Code)
		}
	})
}
