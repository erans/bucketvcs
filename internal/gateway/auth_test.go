package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// fakeStore is an in-memory minimal auth.Store for middleware tests.
type fakeStore struct {
	credActor *auth.Actor
	credToken string
	credScope *auth.Scope
	credErr   error
	perm      auth.Perm
	flags     auth.RepoFlags
	flagsErr  error
	flagsFn   func(tenant, repo string) (auth.RepoFlags, error)
}

func (f *fakeStore) VerifyCredential(ctx context.Context, c auth.Credential) (*auth.Actor, string, *auth.Scope, error) {
	return f.credActor, f.credToken, f.credScope, f.credErr
}
func (f *fakeStore) LookupRepoPerm(ctx context.Context, a *auth.Actor, t, r string) (auth.Perm, error) {
	if a == nil {
		return auth.PermNone, nil
	}
	return f.perm, nil
}
func (f *fakeStore) GetRepoFlags(ctx context.Context, t, r string) (auth.RepoFlags, error) {
	if f.flagsFn != nil {
		return f.flagsFn(t, r)
	}
	return f.flags, f.flagsErr
}
func (f *fakeStore) TouchTokenUsage(ctx context.Context, id string) error { return nil }
func (f *fakeStore) Close() error                                         { return nil }

func req(t *testing.T, method, path, query, basicUser, basicPass string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, "http://x"+path+"?"+query, nil)
	if basicUser != "" || basicPass != "" {
		r.SetBasicAuth(basicUser, basicPass)
	}
	return r
}

func TestRunAuth_AnonymousReadPublic(t *testing.T) {
	st := &fakeStore{flags: auth.RepoFlags{PublicRead: true}}
	rr := &RoutedRequest{Tenant: "a", Repo: "b", Op: OpUploadPack, RequiredAction: auth.ActionRead}
	w := httptest.NewRecorder()
	r := req(t, "POST", "/a/b.git/git-upload-pack", "", "", "")
	actor, ok := RunAuth(w, r, st, rr)
	if !ok {
		t.Fatalf("expected allow, got status %d", w.Code)
	}
	if actor != nil {
		t.Fatalf("expected anonymous, got %+v", actor)
	}
}

func TestRunAuth_AnonymousWritePublic_Challenge(t *testing.T) {
	st := &fakeStore{flags: auth.RepoFlags{PublicRead: true}}
	rr := &RoutedRequest{Tenant: "a", Repo: "b", Op: OpReceivePack, RequiredAction: auth.ActionWrite}
	w := httptest.NewRecorder()
	r := req(t, "POST", "/a/b.git/git-receive-pack", "", "", "")
	if _, ok := RunAuth(w, r, st, rr); ok {
		t.Fatal("expected deny")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if !strings.HasPrefix(w.Header().Get("WWW-Authenticate"), "Basic ") {
		t.Fatalf("missing WWW-Authenticate: %q", w.Header().Get("WWW-Authenticate"))
	}
}

func TestRunAuth_AnonymousReadPrivate_Challenge(t *testing.T) {
	st := &fakeStore{flags: auth.RepoFlags{PublicRead: false}}
	rr := &RoutedRequest{Tenant: "a", Repo: "b", Op: OpUploadPack, RequiredAction: auth.ActionRead}
	w := httptest.NewRecorder()
	r := req(t, "POST", "/a/b.git/git-upload-pack", "", "", "")
	if _, ok := RunAuth(w, r, st, rr); ok {
		t.Fatal("expected deny")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestRunAuth_NoSuchRepo404(t *testing.T) {
	st := &fakeStore{flagsErr: auth.ErrNoSuchRepo}
	rr := &RoutedRequest{Tenant: "a", Repo: "b", Op: OpUploadPack, RequiredAction: auth.ActionRead}
	w := httptest.NewRecorder()
	r := req(t, "POST", "/a/b.git/git-upload-pack", "", "", "")
	if _, ok := RunAuth(w, r, st, rr); ok {
		t.Fatal("expected deny")
	}
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestRunAuth_BadCredentials_401(t *testing.T) {
	st := &fakeStore{credErr: auth.ErrInvalidCredential, flags: auth.RepoFlags{}}
	rr := &RoutedRequest{Tenant: "a", Repo: "b", Op: OpUploadPack, RequiredAction: auth.ActionRead}
	w := httptest.NewRecorder()
	r := req(t, "POST", "/a/b.git/git-upload-pack", "", "alice", "bvts_BAD")
	if _, ok := RunAuth(w, r, st, rr); ok {
		t.Fatal("expected deny")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestRunAuth_AuthenticatedUnauthorized_403(t *testing.T) {
	actor := &auth.Actor{UserID: "u1", Name: "alice"}
	st := &fakeStore{credActor: actor, credToken: "tokid", flags: auth.RepoFlags{}, perm: auth.PermRead}
	rr := &RoutedRequest{Tenant: "a", Repo: "b", Op: OpReceivePack, RequiredAction: auth.ActionWrite}
	w := httptest.NewRecorder()
	r := req(t, "POST", "/a/b.git/git-receive-pack", "", "alice", "bvts_OK")
	if _, ok := RunAuth(w, r, st, rr); ok {
		t.Fatal("expected deny")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

func TestRunAuth_AuthenticatedAuthorized_AttachesActor(t *testing.T) {
	actor := &auth.Actor{UserID: "u1", Name: "alice"}
	st := &fakeStore{credActor: actor, credToken: "tokid", flags: auth.RepoFlags{}, perm: auth.PermWrite}
	rr := &RoutedRequest{Tenant: "a", Repo: "b", Op: OpReceivePack, RequiredAction: auth.ActionWrite}
	w := httptest.NewRecorder()
	r := req(t, "POST", "/a/b.git/git-receive-pack", "", "alice", "bvts_OK")
	got, ok := RunAuth(w, r, st, rr)
	if !ok {
		t.Fatalf("expected allow, status=%d", w.Code)
	}
	if got != actor {
		t.Fatalf("got actor %+v want %+v", got, actor)
	}
}

func TestRunAuth_PassesContextErrors(t *testing.T) {
	st := &fakeStore{flagsErr: errors.New("internal-disk")}
	rr := &RoutedRequest{Tenant: "a", Repo: "b", Op: OpUploadPack, RequiredAction: auth.ActionRead}
	w := httptest.NewRecorder()
	r := req(t, "POST", "/a/b.git/git-upload-pack", "", "", "")
	if _, ok := RunAuth(w, r, st, rr); ok {
		t.Fatal("expected deny on internal error")
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

// TestRunAuth_VerifyCredentialBackendError_500 ensures non-credential
// errors from VerifyCredential surface as 500, not 401. A DB outage
// should never be reported to the client as "bad credentials."
func TestRunAuth_VerifyCredentialBackendError_500(t *testing.T) {
	st := &fakeStore{credErr: errors.New("db unreachable"), flags: auth.RepoFlags{}}
	rr := &RoutedRequest{Tenant: "a", Repo: "b", Op: OpUploadPack, RequiredAction: auth.ActionRead}
	w := httptest.NewRecorder()
	r := req(t, "POST", "/a/b.git/git-upload-pack", "", "alice", "bvts_OK")
	if _, ok := RunAuth(w, r, st, rr); ok {
		t.Fatal("expected deny")
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (got challenge instead?)", w.Code)
	}
}

// TestRunAuth_ScopeMismatch verifies that a scoped credential (e.g. a deploy key
// bound to acme/web) is rejected with 403 "scope mismatch" when the request
// targets a different repo (acme/other).
func TestRunAuth_ScopeMismatch(t *testing.T) {
	actor := &auth.Actor{UserID: "deploy:bvsk_x", Name: "deploy-key:ci"}
	st := &fakeStore{
		credActor: actor,
		credToken: "bvsk_x",
		credScope: &auth.Scope{Tenant: "acme", Repo: "web", Perm: auth.PermWrite},
		flagsFn: func(tenant, repo string) (auth.RepoFlags, error) {
			if tenant == "acme" && repo == "other" {
				return auth.RepoFlags{}, nil
			}
			return auth.RepoFlags{}, auth.ErrNoSuchRepo
		},
	}
	rr := &RoutedRequest{Tenant: "acme", Repo: "other", Op: OpReceivePack, RequiredAction: auth.ActionWrite}
	w := httptest.NewRecorder()
	r := req(t, "POST", "/acme/other.git/git-receive-pack", "", "alice", "anytoken")
	if _, ok := RunAuth(w, r, st, rr); ok {
		t.Fatal("expected deny on scope mismatch")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "scope mismatch") {
		t.Fatalf("body = %q, want to contain \"scope mismatch\"", w.Body.String())
	}
}

// TestRunAuth_ScopeMatch verifies that a scoped credential bound to acme/web
// allows write access to acme/web without going through LookupRepoPerm.
func TestRunAuth_ScopeMatch(t *testing.T) {
	actor := &auth.Actor{UserID: "deploy:bvsk_x", Name: "deploy-key:ci"}
	st := &fakeStore{
		credActor: actor,
		credToken: "bvsk_x",
		credScope: &auth.Scope{Tenant: "acme", Repo: "web", Perm: auth.PermWrite},
		flags:     auth.RepoFlags{},
		// perm intentionally left at zero (PermNone) to confirm LookupRepoPerm is bypassed.
	}
	rr := &RoutedRequest{Tenant: "acme", Repo: "web", Op: OpReceivePack, RequiredAction: auth.ActionWrite}
	w := httptest.NewRecorder()
	r := req(t, "POST", "/acme/web.git/git-receive-pack", "", "deploy-key:ci", "bvsk_x")
	got, ok := RunAuth(w, r, st, rr)
	if !ok {
		t.Fatalf("expected allow on scope match, status=%d body=%q", w.Code, w.Body.String())
	}
	if got != actor {
		t.Fatalf("actor = %+v, want %+v", got, actor)
	}
}
