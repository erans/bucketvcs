package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/browsemodel"
)

// errNotVisible is the test sentinel a browseDataStore returns when a repo is
// not in its visible set.
var errNotVisible = errors.New("not visible")

// browseDataStore is a self-contained DataStore fake for browse routing tests.
// All methods return zero values except GetVisibleRepo, which consults `visible`.
type browseDataStore struct {
	visible map[string]bool // "tenant/name" -> visible
}

func (b *browseDataStore) VerifyPassword(ctx context.Context, u, p string) (*auth.Actor, error) {
	return nil, errors.New("nope")
}
func (b *browseDataStore) CreateSession(ctx context.Context, userID, provider string, ttl time.Duration) (string, error) {
	return "", nil
}
func (b *browseDataStore) LookupSession(ctx context.Context, raw string) (*auth.Session, error) {
	return nil, errors.New("none")
}
func (b *browseDataStore) TouchSession(ctx context.Context, raw string, ttl time.Duration) error {
	return nil
}
func (b *browseDataStore) DeleteSession(ctx context.Context, raw string) error { return nil }
func (b *browseDataStore) ListAccessibleRepos(ctx context.Context, a *auth.Actor) ([]Repo, error) {
	return nil, nil
}
func (b *browseDataStore) GetVisibleRepo(ctx context.Context, a *auth.Actor, tenant, name string) (*Repo, error) {
	if b.visible[tenant+"/"+name] {
		return &Repo{Tenant: tenant, Name: name}, nil
	}
	return nil, errNotVisible
}
func (b *browseDataStore) FindUserByEmail(ctx context.Context, email string) (*auth.Actor, error) {
	return nil, errors.New("none")
}
func (b *browseDataStore) FindIdentity(ctx context.Context, issuer, subject string) (*auth.Actor, error) {
	return nil, errors.New("none")
}
func (b *browseDataStore) LinkIdentity(ctx context.Context, userID, issuer, subject, email string) error {
	return nil
}

// fakeContent is a configurable ContentStore for browse tests.
type fakeContent struct {
	refs browsemodel.Refs
	res  browsemodel.Resolved
	warm bool
}

func (f *fakeContent) ListRefs(ctx context.Context, t, r string) (browsemodel.Refs, error) {
	if f.warm {
		return browsemodel.Refs{}, browsemodel.ErrWarming
	}
	return f.refs, nil
}
func (f *fakeContent) Resolve(ctx context.Context, t, r, rest string) (browsemodel.Resolved, error) {
	if f.warm {
		return browsemodel.Resolved{}, browsemodel.ErrWarming
	}
	return f.res, nil
}
func (f *fakeContent) ReadTree(ctx context.Context, t, r, oid, p string) ([]browsemodel.TreeEntry, error) {
	return nil, nil
}
func (f *fakeContent) ReadBlob(ctx context.Context, t, r, oid, p string) (browsemodel.Blob, error) {
	return browsemodel.Blob{}, browsemodel.ErrNotFound
}
func (f *fakeContent) Log(ctx context.Context, t, r, oid string, off, lim int) ([]browsemodel.CommitMeta, bool, error) {
	return nil, false, nil
}
func (f *fakeContent) Commit(ctx context.Context, t, r, oid string) (browsemodel.CommitDetail, error) {
	return browsemodel.CommitDetail{}, browsemodel.ErrNotFound
}

func newBrowseServer(content ContentStore, visible map[string]bool) http.Handler {
	return NewHandler(Deps{
		Store:   &browseDataStore{visible: visible},
		Content: content,
	})
}

func TestBrowse_Routing(t *testing.T) {
	content := &fakeContent{res: browsemodel.Resolved{Ref: "main", OID: "abc", Path: ""}}
	h := newBrowseServer(content, map[string]bool{"acme/demo": true})
	cases := []struct {
		path string
		want int
	}{
		{"/acme/demo", 200},
		{"/acme/demo/tree/main/sub", 200},
		{"/acme/demo/commits/main", 200},
		{"/acme/demo/bogus/main", http.StatusNotFound}, // unknown verb
		{"/acme", http.StatusNotFound},                  // single segment
		{"/acme/secret", http.StatusNotFound},           // not visible → 404
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", c.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: got %d want %d", c.path, rec.Code, c.want)
		}
	}
}

func TestBrowse_NotVisibleIs404(t *testing.T) {
	h := newBrowseServer(&fakeContent{}, map[string]bool{}) // nothing visible
	req := httptest.NewRequest("GET", "/acme/demo", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
}

func TestBrowse_WarmingIs503(t *testing.T) {
	h := newBrowseServer(&fakeContent{warm: true}, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", rec.Code)
	}
}

func TestBrowse_DisabledWhenContentNil(t *testing.T) {
	h := NewHandler(Deps{Store: &browseDataStore{visible: map[string]bool{"acme/demo": true}}})
	req := httptest.NewRequest("GET", "/acme/demo", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("content nil should 404, got %d", rec.Code)
	}
}
