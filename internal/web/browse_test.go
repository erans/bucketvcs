package web

import (
	"bytes"
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
	perm    auth.Perm       // returned by LookupRepoPerm (default PermNone)
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
func (b *browseDataStore) LookupRepoPerm(ctx context.Context, a *auth.Actor, tenant, repo string) (auth.Perm, error) {
	return b.perm, nil
}
func (b *browseDataStore) GetRepoFlags(ctx context.Context, tenant, repo string) (auth.RepoFlags, error) {
	return auth.RepoFlags{}, nil
}
func (b *browseDataStore) SetRepoPublic(ctx context.Context, tenant, repo string, public bool) error {
	return nil
}
func (b *browseDataStore) RenameRepo(ctx context.Context, tenant, oldName, newName string) error {
	panic("browseDataStore.RenameRepo not implemented")
}
func (b *browseDataStore) DeleteRepoCascade(ctx context.Context, tenant, repo string) error {
	panic("browseDataStore.DeleteRepoCascade not implemented")
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
func (b *browseDataStore) GetUserByName(ctx context.Context, name string) (*auth.User, error) {
	panic("browseDataStore.GetUserByName not implemented")
}
func (b *browseDataStore) SetPassword(ctx context.Context, userName, plaintext string) error {
	panic("browseDataStore.SetPassword not implemented")
}
func (b *browseDataStore) HasPassword(ctx context.Context, userName string) (bool, error) {
	panic("browseDataStore.HasPassword not implemented")
}
func (b *browseDataStore) ListTokensForUser(ctx context.Context, name string) ([]TokenInfo, error) {
	panic("browseDataStore.ListTokensForUser not implemented")
}
func (b *browseDataStore) GetTokenOwner(ctx context.Context, id string) (string, error) {
	panic("browseDataStore.GetTokenOwner not implemented")
}
func (b *browseDataStore) CreateToken(ctx context.Context, id, userID, secretHash, label string, expiresAt *int64, scopes auth.TokenScope) error {
	panic("browseDataStore.CreateToken not implemented")
}
func (b *browseDataStore) RevokeToken(ctx context.Context, id string) error {
	panic("browseDataStore.RevokeToken not implemented")
}
func (b *browseDataStore) RotateToken(ctx context.Context, id, newSecretHash string) error {
	panic("browseDataStore.RotateToken not implemented")
}
func (b *browseDataStore) ListSSHKeysForUser(ctx context.Context, userID string) ([]auth.SSHKey, error) {
	panic("browseDataStore.ListSSHKeysForUser not implemented")
}
func (b *browseDataStore) AddSSHKey(ctx context.Context, k auth.SSHKey) error {
	panic("browseDataStore.AddSSHKey not implemented")
}
func (b *browseDataStore) RevokeSSHKey(ctx context.Context, keyIDOrPrefix string) error {
	panic("browseDataStore.RevokeSSHKey not implemented")
}
func (b *browseDataStore) ListRepoGrants(ctx context.Context, tenant, repo string) ([]RepoGrant, error) {
	panic("browseDataStore.ListRepoGrants not implemented")
}
func (b *browseDataStore) Grant(ctx context.Context, userName, tenant, repo, perm string) error {
	panic("browseDataStore.Grant not implemented")
}
func (b *browseDataStore) RevokeRepoPermission(ctx context.Context, userName, tenant, repo string) error {
	panic("browseDataStore.RevokeRepoPermission not implemented")
}
func (b *browseDataStore) ListSSHKeysForRepo(ctx context.Context, tenant, repo string) ([]auth.SSHKey, error) {
	panic("browseDataStore.ListSSHKeysForRepo not implemented")
}
func (b *browseDataStore) ListUsers(ctx context.Context) ([]UserInfo, error) {
	panic("browseDataStore.ListUsers not implemented")
}
func (b *browseDataStore) CreateUser(ctx context.Context, name string, isAdmin bool) (string, error) {
	panic("browseDataStore.CreateUser not implemented")
}
func (b *browseDataStore) SetUserDisabled(ctx context.Context, name string, disabled bool) error {
	panic("browseDataStore.SetUserDisabled not implemented")
}
func (b *browseDataStore) DeleteUser(ctx context.Context, name string) error {
	panic("browseDataStore.DeleteUser not implemented")
}
func (b *browseDataStore) SetEmail(ctx context.Context, userName, email string) error {
	panic("browseDataStore.SetEmail not implemented")
}

// fakeContent is a configurable ContentStore for browse tests.
type fakeContent struct {
	refs     browsemodel.Refs
	warm     bool
	tree     []browsemodel.TreeEntry
	blob     browsemodel.Blob
	log      []browsemodel.CommitMeta
	more     bool
	commit   browsemodel.CommitDetail
	activity map[string]browsemodel.CommitMeta
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
func (f *fakeContent) TreeActivity(ctx context.Context, t, r, oid, p string) (map[string]browsemodel.CommitMeta, error) {
	return f.activity, nil
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

func TestChromaCSSRoute(t *testing.T) {
	h := newBrowseServer(&fakeContent{}, map[string]bool{})
	req := httptest.NewRequest("GET", "/_ui/static/chroma.css", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), ".chroma") {
		t.Fatalf("chroma.css route: code=%d body=%.120s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Fatalf("content-type = %q", ct)
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

func TestCommit_DiffLineClasses(t *testing.T) {
	content := &fakeContent{
		commit: browsemodel.CommitDetail{
			Meta:    browsemodel.CommitMeta{OID: "c2", ShortOID: "c2", Summary: "update a", AuthorName: "Ann", AuthorTime: 1700000000},
			Message: "update a\n",
			Files: []browsemodel.FileDiff{{
				NewPath: "a.txt", Status: "M", Additions: 1, Deletions: 1,
				Hunks: []browsemodel.Hunk{{Header: "@@ -1 +1 @@", Lines: []browsemodel.DiffLine{
					{Kind: ' ', Text: "ctx line"},
					{Kind: '-', Text: "old"},
					{Kind: '+', Text: "new"},
				}}},
			}},
		},
	}
	h := newBrowseServer(content, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo/commit/c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{`class="dl ctx"`, `class="dl del"`, `class="dl add"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s in commit view: %s", want, body)
		}
	}
	if strings.Contains(body, `class="dl k`) {
		t.Errorf("old k-class scheme still present")
	}
}

func TestTree_QueryRefSelectsRef(t *testing.T) {
	content := &fakeContent{
		refs: browsemodel.Refs{Default: "main", Branches: []browsemodel.RefInfo{
			{Name: "main", OID: "abcdefabcdefabcdefabcdefabcdefabcdefabcd"},
			{Name: "dev", OID: "1234567890123456789012345678901234567890"},
		}},
		tree: []browsemodel.TreeEntry{{Name: "a.txt", Path: "a.txt", Type: "blob", OID: "x"}},
	}
	h := newBrowseServer(content, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo/tree/?ref=dev", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("?ref= tree: code %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "tree/dev/") {
		t.Fatalf("expected links on ref dev: %s", rec.Body.String())
	}
}

func TestTree_HXRequestReturnsFragment(t *testing.T) {
	content := &fakeContent{
		refs: browsemodel.Refs{Default: "main", Branches: []browsemodel.RefInfo{{Name: "main", OID: "abcdefabcdefabcdefabcdefabcdefabcdefabcd"}}},
		tree: []browsemodel.TreeEntry{{Name: "a.txt", Path: "a.txt", Type: "blob", OID: "x"}},
	}
	h := newBrowseServer(content, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo/tree/main", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if strings.Contains(body, "<html") {
		t.Fatalf("HX-Request should get a bare fragment: %s", body)
	}
	if !strings.Contains(body, `id="tree"`) || !strings.Contains(body, "a.txt") {
		t.Fatalf("fragment missing tree content: %s", body)
	}
}

func TestRenderPartial_TreeRows(t *testing.T) {
	r, err := newRenderer("")
	if err != nil {
		t.Fatal(err)
	}
	data := treeData{
		browseHeader: browseHeader{Tenant: "acme", Repo: "demo", Ref: "main"},
		Entries:      []browsemodel.TreeEntry{{Name: "a.txt", Path: "a.txt", Type: "blob", Size: 6, OID: "x"}},
	}
	var buf bytes.Buffer
	if err := r.renderPartial(&buf, "treeRows", data); err != nil {
		t.Fatalf("renderPartial: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `id="tree"`) || !strings.Contains(out, "a.txt") {
		t.Fatalf("partial missing container/entry: %s", out)
	}
	if strings.Contains(out, "<html") {
		t.Fatalf("partial must not include the base layout: %s", out)
	}
}

func TestCommits_AgeColumn(t *testing.T) {
	content := &fakeContent{
		refs: browsemodel.Refs{Default: "main", Branches: []browsemodel.RefInfo{{Name: "main", OID: "abcdefabcdefabcdefabcdefabcdefabcdefabcd"}}},
		log:  []browsemodel.CommitMeta{{OID: "c2", ShortOID: "c2", Summary: "update a", AuthorName: "Ann", AuthorTime: time.Now().Add(-2 * time.Hour).Unix()}},
	}
	h := newBrowseServer(content, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo/commits/main", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "2h ago") {
		t.Fatalf("commit log missing relative age: %s", rec.Body.String())
	}
}

func TestBlob_HumanizedSize(t *testing.T) {
	content := &fakeContent{
		refs: browsemodel.Refs{Default: "main", Branches: []browsemodel.RefInfo{{Name: "main", OID: "abcdefabcdefabcdefabcdefabcdefabcdefabcd"}}},
		blob: browsemodel.Blob{Path: "bin.dat", Size: 4 << 20, Binary: true, Bytes: []byte{0}},
	}
	h := newBrowseServer(content, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo/blob/main/bin.dat", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "4.0 MiB") {
		t.Fatalf("binary notice missing humanized size: %s", rec.Body.String())
	}
}

func TestTree_ActivityColumnRendered(t *testing.T) {
	content := &fakeContent{
		refs: browsemodel.Refs{Default: "main", Branches: []browsemodel.RefInfo{{Name: "main", OID: "abcdefabcdefabcdefabcdefabcdefabcdefabcd"}}},
		tree: []browsemodel.TreeEntry{
			{Name: "a.txt", Path: "a.txt", Type: "blob", Size: 6, OID: "x"},
			{Name: "old.txt", Path: "old.txt", Type: "blob", Size: 1, OID: "y"},
		},
		activity: map[string]browsemodel.CommitMeta{
			"a.txt": {OID: "abc", Summary: "update a", AuthorTime: 1700000000},
		},
	}
	h := newBrowseServer(content, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo/tree/main", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "update a") {
		t.Fatalf("attributed entry missing summary: %s", body)
	}
	if !strings.Contains(body, "—") {
		t.Fatalf("unattributed entry should render —: %s", body)
	}
}

func TestUIWideCSP(t *testing.T) {
	content := &fakeContent{
		refs: browsemodel.Refs{Default: "main", Branches: []browsemodel.RefInfo{{Name: "main", OID: "abcdefabcdefabcdefabcdefabcdefabcdefabcd"}}},
		tree: []browsemodel.TreeEntry{{Name: "a.txt", Path: "a.txt", Type: "blob", OID: "x"}},
		blob: browsemodel.Blob{Path: "a.txt", Size: 2, Bytes: []byte("x\n")},
	}
	h := newBrowseServer(content, map[string]bool{"acme/demo": true})
	const want = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"
	for _, path := range []string{"/", "/login", "/acme/demo", "/acme/demo/tree/main", "/acme/demo/blob/main/a.txt", "/nope"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := rec.Header().Get("Content-Security-Policy"); got != want {
			t.Errorf("%s: CSP = %q", path, got)
		}
	}
	req := httptest.NewRequest("GET", "/acme/demo/raw/main/a.txt", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Content-Security-Policy"); got != "default-src 'none'; sandbox" {
		t.Errorf("raw CSP = %q", got)
	}
}

func TestBlob_MarkdownRenderedToggle(t *testing.T) {
	content := &fakeContent{
		refs: browsemodel.Refs{Default: "main", Branches: []browsemodel.RefInfo{{Name: "main", OID: "abcdefabcdefabcdefabcdefabcdefabcdefabcd"}}},
		blob: browsemodel.Blob{Path: "docs/guide.md", Size: 20, Bytes: []byte("# Title\n\n**bold** <script>x()</script>\n")},
	}
	h := newBrowseServer(content, map[string]bool{"acme/demo": true})

	// Source view: offers [rendered].
	req := httptest.NewRequest("GET", "/acme/demo/blob/main/docs/guide.md", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "?view=rendered") || !strings.Contains(body, "[rendered]") {
		t.Fatalf("source view missing [rendered] toggle: %s", body)
	}

	// Rendered view: sanitized HTML + [source] link back.
	req = httptest.NewRequest("GET", "/acme/demo/blob/main/docs/guide.md?view=rendered", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body = rec.Body.String()
	if !strings.Contains(body, "<strong>") && !strings.Contains(body, "<h1") {
		t.Fatalf("rendered view missing rendered markdown: %s", body)
	}
	if strings.Contains(body, "<script>") {
		t.Fatalf("rendered view not sanitized: %s", body)
	}
	if !strings.Contains(body, "[source]") {
		t.Fatalf("rendered view missing [source] toggle: %s", body)
	}
}

func TestBlob_NonMarkdownNoRenderToggle(t *testing.T) {
	content := &fakeContent{
		refs: browsemodel.Refs{Default: "main", Branches: []browsemodel.RefInfo{{Name: "main", OID: "abcdefabcdefabcdefabcdefabcdefabcdefabcd"}}},
		blob: browsemodel.Blob{Path: "main.go", Size: 10, Bytes: []byte("package x\n")},
	}
	h := newBrowseServer(content, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo/blob/main/main.go?view=rendered", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if strings.Contains(body, "[rendered]") || strings.Contains(body, "[source]") {
		t.Fatalf("non-markdown blob should have no render toggle: %s", body)
	}
	if !strings.Contains(body, "main.go") {
		t.Fatalf("source view broken: %s", body)
	}
}

func TestLineNumsJSServed(t *testing.T) {
	h := newBrowseServer(&fakeContent{}, map[string]bool{})
	req := httptest.NewRequest("GET", "/_ui/static/linenums.js", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "hashchange") {
		t.Fatalf("linenums.js not served: code=%d", rec.Code)
	}
}

func TestTreeRoot_RendersReadme(t *testing.T) {
	content := &fakeContent{
		refs: browsemodel.Refs{Default: "main", Branches: []browsemodel.RefInfo{{Name: "main", OID: "abcdefabcdefabcdefabcdefabcdefabcdefabcd"}}},
		tree: []browsemodel.TreeEntry{{Name: "README.md", Path: "README.md", Type: "blob", Size: 10, OID: "x"}},
		blob: browsemodel.Blob{Path: "README.md", Size: 10, Bytes: []byte("# Hello Readme\n")},
	}
	h := newBrowseServer(content, map[string]bool{"acme/demo": true})

	// Full tree page at ref root renders the README.
	req := httptest.NewRequest("GET", "/acme/demo/tree/?ref=main", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "Hello Readme") {
		t.Fatalf("tree root missing rendered README: %s", rec.Body.String())
	}

	// htmx fragment at ref root includes the README (inside #tree).
	req = httptest.NewRequest("GET", "/acme/demo/tree/main", nil)
	req.Header.Set("HX-Request", "true")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "Hello Readme") {
		t.Fatalf("htmx fragment missing README: %s", rec.Body.String())
	}
}

func TestTreeSubdir_NoReadme(t *testing.T) {
	content := &fakeContent{
		refs: browsemodel.Refs{Default: "main", Branches: []browsemodel.RefInfo{{Name: "main", OID: "abcdefabcdefabcdefabcdefabcdefabcdefabcd"}}},
		tree: []browsemodel.TreeEntry{{Name: "README.md", Path: "sub/README.md", Type: "blob", Size: 10, OID: "x"}},
		blob: browsemodel.Blob{Path: "sub/README.md", Size: 10, Bytes: []byte("# Sub Readme\n")},
	}
	h := newBrowseServer(content, map[string]bool{"acme/demo": true})
	req := httptest.NewRequest("GET", "/acme/demo/tree/main/sub", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "Sub Readme") {
		t.Fatalf("subdirectory tree should not render README: %s", rec.Body.String())
	}
}
