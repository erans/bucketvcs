package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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
	refs   browsemodel.Refs
	warm   bool
	tree   []browsemodel.TreeEntry
	blob   browsemodel.Blob
	log    []browsemodel.CommitMeta
	more   bool
	commit browsemodel.CommitDetail
}

func (f *fakeContent) ListRefs(ctx context.Context, t, r string) (browsemodel.Refs, error) {
	if f.warm {
		return browsemodel.Refs{}, browsemodel.ErrWarming
	}
	return f.refs, nil
}
func (f *fakeContent) ReadTree(ctx context.Context, t, r, oid, p string) ([]browsemodel.TreeEntry, error) {
	if f.warm {
		return nil, browsemodel.ErrWarming
	}
	return f.tree, nil
}
func (f *fakeContent) ReadBlob(ctx context.Context, t, r, oid, p string) (browsemodel.Blob, error) {
	if f.warm {
		return browsemodel.Blob{}, browsemodel.ErrWarming
	}
	return f.blob, nil
}
func (f *fakeContent) Log(ctx context.Context, t, r, oid string, off, lim int) ([]browsemodel.CommitMeta, bool, error) {
	if f.warm {
		return nil, false, browsemodel.ErrWarming
	}
	return f.log, f.more, nil
}
func (f *fakeContent) Commit(ctx context.Context, t, r, oid string) (browsemodel.CommitDetail, error) {
	if f.warm {
		return browsemodel.CommitDetail{}, browsemodel.ErrWarming
	}
	return f.commit, nil
}

func newBrowseServer(content ContentStore, visible map[string]bool) http.Handler {
	return NewHandler(Deps{
		Store:   &browseDataStore{visible: visible},
		Content: content,
	})
}

// mainRefs is a convenience refs fixture with branch "main" pointing to a
// well-formed 40-hex OID, used across tests that hit ref-based URLs.
func mainRefs() browsemodel.Refs {
	return browsemodel.Refs{
		Default:  "main",
		Branches: []browsemodel.RefInfo{{Name: "main", OID: "abcdefabcdefabcdefabcdefabcdefabcdefabcd"}},
	}
}

func TestBrowse_Routing(t *testing.T) {
	content := &fakeContent{refs: mainRefs()}
	h := newBrowseServer(content, map[string]bool{"acme/demo": true})
	cases := []struct {
		path string
		want int
	}{
		{"/acme/demo", 200},
		{"/acme/demo/", 200},                            // trailing slash on repo home
		{"/acme/demo//x", http.StatusTemporaryRedirect}, // double-slash: mux path-cleans to 307
		{"/acme/demo/tree/main/sub", 200},
		{"/acme/demo/commits/main", 200},
		{"/acme/demo/bogus/main", http.StatusNotFound}, // unknown verb
		{"/acme", http.StatusNotFound},                 // single segment
		{"/acme/secret", http.StatusNotFound},          // not visible → 404
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

func TestRepoHome_RendersTree(t *testing.T) {
	content := &fakeContent{
		refs: browsemodel.Refs{Default: "main", Branches: []browsemodel.RefInfo{{Name: "main", OID: "abcdefabcdefabcdefabcdefabcdefabcdefabcd"}}},
		tree: []browsemodel.TreeEntry{{Name: "a.txt", Path: "a.txt", Type: "blob", Size: 6, OID: "x"}},
	}
	h := newBrowseServer(content, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "a.txt") {
		t.Fatalf("home missing tree entry: %s", rec.Body.String())
	}
}

func TestTree_RendersPathEntries(t *testing.T) {
	content := &fakeContent{
		refs: browsemodel.Refs{Default: "main", Branches: []browsemodel.RefInfo{{Name: "main", OID: "abcdefabcdefabcdefabcdefabcdefabcdefabcd"}}},
		tree: []browsemodel.TreeEntry{{Name: "b.txt", Path: "sub/b.txt", Type: "blob", OID: "y"}},
	}
	h := newBrowseServer(content, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo/tree/main/sub", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "b.txt") {
		t.Fatalf("tree missing entry: %s", rec.Body.String())
	}
}

func TestRaw_ForcesSafeContentType(t *testing.T) {
	content := &fakeContent{
		refs: browsemodel.Refs{Default: "main", Branches: []browsemodel.RefInfo{{Name: "main", OID: "abcdefabcdefabcdefabcdefabcdefabcdefabcd"}}},
		blob: browsemodel.Blob{Path: "evil.html", Size: 20, Bytes: []byte("<script>x()</script>")},
	}
	h := newBrowseServer(content, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo/raw/main/evil.html", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("content-type = %q, want text/plain", ct)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing nosniff")
	}
	if csp := rec.Header().Get("Content-Security-Policy"); csp != "default-src 'none'; sandbox" {
		t.Fatalf("CSP = %q, want default-src 'none'; sandbox", csp)
	}
	if rec.Body.String() != "<script>x()</script>" {
		t.Fatalf("raw body altered: %q", rec.Body.String())
	}
}

func TestRaw_BinaryIsOctetStreamAttachment(t *testing.T) {
	content := &fakeContent{
		refs: browsemodel.Refs{Default: "main", Branches: []browsemodel.RefInfo{{Name: "main", OID: "abcdefabcdefabcdefabcdefabcdefabcdefabcd"}}},
		blob: browsemodel.Blob{Path: "bin.dat", Size: 4, Binary: true, Bytes: []byte{0, 1, 2, 0}},
	}
	h := newBrowseServer(content, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo/raw/main/bin.dat", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Fatalf("disposition = %q", cd)
	}
}

func TestBlob_HighlightedAndEscaped(t *testing.T) {
	content := &fakeContent{
		refs: browsemodel.Refs{Default: "main", Branches: []browsemodel.RefInfo{{Name: "main", OID: "abcdefabcdefabcdefabcdefabcdefabcdefabcd"}}},
		blob: browsemodel.Blob{Path: "main.go", Size: 30, Bytes: []byte("package main // <x>\n")},
	}
	h := newBrowseServer(content, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo/blob/main/main.go", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if strings.Contains(body, "<x>") {
		t.Fatalf("blob content must be HTML-escaped, found raw <x>: %s", body)
	}
	if !strings.Contains(body, "main.go") {
		t.Fatalf("blob view missing filename")
	}
}

func TestCommits_ListAndPaging(t *testing.T) {
	content := &fakeContent{
		refs: browsemodel.Refs{Default: "main", Branches: []browsemodel.RefInfo{{Name: "main", OID: "abcdefabcdefabcdefabcdefabcdefabcdefabcd"}}},
		log:  []browsemodel.CommitMeta{{OID: "c2", ShortOID: "c2", Summary: "update a", AuthorName: "Ann"}},
		more: true,
	}
	h := newBrowseServer(content, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo/commits/main", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "update a") {
		t.Fatalf("commit log missing summary: %s", body)
	}
	if !strings.Contains(body, "page=1") {
		t.Fatalf("expected next-page link when more=true: %s", body)
	}
}

func TestCommit_RendersDiff(t *testing.T) {
	content := &fakeContent{
		commit: browsemodel.CommitDetail{
			Meta:    browsemodel.CommitMeta{OID: "c2", ShortOID: "c2", Summary: "update a", AuthorName: "Ann"},
			Message: "update a\n",
			Parents: []string{"c1"},
			Files: []browsemodel.FileDiff{{
				NewPath: "a.txt", Status: "M", Additions: 1, Deletions: 1,
				Hunks: []browsemodel.Hunk{{Header: "@@ -1 +1 @@", Lines: []browsemodel.DiffLine{
					{Kind: '-', Text: "hello"}, {Kind: '+', Text: "hello again"},
				}}},
			}},
		},
	}
	h := newBrowseServer(content, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo/commit/c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "a.txt") || !strings.Contains(body, "hello again") {
		t.Fatalf("commit view missing diff: %s", body)
	}
}

// TestBrowse_OIDLinksUseOID checks that navigation links use the resolved OID
// (not an empty string) when a page is reached via a raw 40-hex OID URL.
func TestBrowse_OIDLinksUseOID(t *testing.T) {
	const testOID = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	content := &fakeContent{
		// No refs needed: testOID is a raw 40-hex OID, so ResolveRest takes the
		// IsHex40 fast path without consulting refs.
		tree: []browsemodel.TreeEntry{{Name: "a.txt", Path: "a.txt", Type: "blob", Size: 6, OID: "x"}},
	}
	h := newBrowseServer(content, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo/tree/"+testOID+"/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// The entry is a blob so the link uses /blob/ — the key invariant is that the
	// OID appears in the href (not an empty segment producing "//").
	if !strings.Contains(body, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef/a.txt") {
		t.Errorf("expected OID in navigation link, got: %s", body)
	}
	if strings.Contains(body, "blob//") || strings.Contains(body, "tree//") {
		t.Errorf("double-slash in navigation link: %s", body)
	}
}

// TestRaw_TooLargeReturns413 checks that requesting a raw download for an
// oversized blob returns 413 instead of a 0-byte 200.
func TestRaw_TooLargeReturns413(t *testing.T) {
	content := &fakeContent{
		refs: browsemodel.Refs{Default: "main", Branches: []browsemodel.RefInfo{{Name: "main", OID: "abcdefabcdefabcdefabcdefabcdefabcdefabcd"}}},
		blob: browsemodel.Blob{Path: "big.bin", Size: 11 << 20, TooLarge: true},
	}
	h := newBrowseServer(content, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo/raw/main/big.bin", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d, want 413", rec.Code)
	}
}

// TestTree_HashInFilenameLinkEscaped verifies that filenames containing '#'
// are percent-encoded in generated href attributes.
func TestTree_HashInFilenameLinkEscaped(t *testing.T) {
	content := &fakeContent{
		refs: browsemodel.Refs{Default: "main", Branches: []browsemodel.RefInfo{{Name: "main", OID: "abcdefabcdefabcdefabcdefabcdefabcdefabcd"}}},
		tree: []browsemodel.TreeEntry{{Name: "a#b.txt", Path: "a#b.txt", Type: "blob", Size: 3, OID: "x"}},
	}
	h := newBrowseServer(content, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo/tree/main/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "a%23b.txt") {
		t.Fatalf("expected %%23-escaped link for #-filename, body: %s", body)
	}
}

// TestCommit_NonOIDIs404 verifies that /commit/<ref-name> returns 404
// (commit links always use full OIDs, never ref names).
func TestCommit_NonOIDIs404(t *testing.T) {
	h := newBrowseServer(&fakeContent{}, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo/commit/main", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404 for non-OID commit URL", rec.Code)
	}
}

func TestRfc5987Encode(t *testing.T) {
	cases := map[string]string{
		"plain.txt":   "plain.txt",
		"foo'bar.png": "foo%27bar.png",
		"a(b)*c.bin":  "a%28b%29%2Ac.bin",
		"caf\xc3\xa9": "caf%C3%A9",
		"sp ace.dat":  "sp%20ace.dat",
	}
	for in, want := range cases {
		if got := rfc5987Encode(in); got != want {
			t.Errorf("rfc5987Encode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCommits_PathFilteredIs404(t *testing.T) {
	content := &fakeContent{
		refs: browsemodel.Refs{Default: "main", Branches: []browsemodel.RefInfo{{Name: "main", OID: "abcdefabcdefabcdefabcdefabcdefabcdefabcd"}}},
	}
	h := newBrowseServer(content, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo/commits/main/sub", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("path-filtered commits should 404 (deferred feature), got %d", rec.Code)
	}
}

func TestQueryPage_Clamped(t *testing.T) {
	req := httptest.NewRequest("GET", "/x?page=99999999", nil)
	if got := queryPage(req); got != maxLogPage {
		t.Fatalf("queryPage huge = %d, want clamp %d", got, maxLogPage)
	}
	req = httptest.NewRequest("GET", "/x?page=3", nil)
	if got := queryPage(req); got != 3 {
		t.Fatalf("queryPage 3 = %d", got)
	}
}
