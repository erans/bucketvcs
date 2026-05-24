package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/ratelimit"
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

// SSH key stubs — not exercised by auth middleware tests.
func (f *fakeStore) AddSSHKey(ctx context.Context, k auth.SSHKey) error { return nil }
func (f *fakeStore) ListSSHKeysForUser(ctx context.Context, userID string) ([]auth.SSHKey, error) {
	return nil, nil
}
func (f *fakeStore) ListSSHKeysForRepo(ctx context.Context, tenant, repo string) ([]auth.SSHKey, error) {
	return nil, nil
}
func (f *fakeStore) RevokeSSHKey(ctx context.Context, keyIDOrPrefix string) error { return nil }
func (f *fakeStore) TouchSSHKeyUsage(ctx context.Context, keyID string) error     { return nil }
func (f *fakeStore) GetUserByName(ctx context.Context, name string) (*auth.User, error) {
	return nil, auth.ErrNoSuchUser
}

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
	actor, ok := RunAuth(w, r, st, rr, nil, false, nil)
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
	if _, ok := RunAuth(w, r, st, rr, nil, false, nil); ok {
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
	if _, ok := RunAuth(w, r, st, rr, nil, false, nil); ok {
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
	if _, ok := RunAuth(w, r, st, rr, nil, false, nil); ok {
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
	if _, ok := RunAuth(w, r, st, rr, nil, false, nil); ok {
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
	if _, ok := RunAuth(w, r, st, rr, nil, false, nil); ok {
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
	got, ok := RunAuth(w, r, st, rr, nil, false, nil)
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
	if _, ok := RunAuth(w, r, st, rr, nil, false, nil); ok {
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
	if _, ok := RunAuth(w, r, st, rr, nil, false, nil); ok {
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
	if _, ok := RunAuth(w, r, st, rr, nil, false, nil); ok {
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
	got, ok := RunAuth(w, r, st, rr, nil, false, nil)
	if !ok {
		t.Fatalf("expected allow on scope match, status=%d body=%q", w.Code, w.Body.String())
	}
	if got != actor {
		t.Fatalf("actor = %+v, want %+v", got, actor)
	}
}

// --- M18 Task 3: rate-limit integration tests --------------------------------

// TestRunAuth_RateLimitsRepeatedBadCreds verifies that after Burst credential
// failures from the same IP, the next attempt returns 429 + Retry-After
// before the auth store is even consulted.
func TestRunAuth_RateLimitsRepeatedBadCreds(t *testing.T) {
	st := &fakeStore{
		flags:   auth.RepoFlags{PublicRead: false},
		credErr: auth.ErrInvalidCredential,
	}
	limiter := ratelimit.NewLimiter(ratelimit.Config{
		Burst:           3,
		RefillPerMinute: 0,
		SweepInterval:   24 * time.Hour,
	})
	defer limiter.Close()

	rr := &RoutedRequest{Tenant: "acme", Repo: "site", Op: OpUploadPack, RequiredAction: auth.ActionRead}

	// 3 bad-cred attempts: each returns 401, each MarkFailure increments
	// the IP bucket. After the 3rd, failures == Burst.
	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/acme/site/info/refs", nil)
		r.SetBasicAuth("alice", "wrongpass")
		r.RemoteAddr = "1.2.3.4:54321"
		_, ok := RunAuth(w, r, st, rr, limiter, false, nil)
		if ok {
			t.Fatalf("attempt %d: ok=true; expected 401", i)
		}
		if w.Code != http.StatusUnauthorized {
			t.Errorf("attempt %d: code=%d, want 401", i, w.Code)
		}
	}

	// 4th attempt: 429 BEFORE credential check (Check sees failures >= Burst).
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/acme/site/info/refs", nil)
	r.SetBasicAuth("alice", "wrongpass")
	r.RemoteAddr = "1.2.3.4:54321"
	_, ok := RunAuth(w, r, st, rr, limiter, false, nil)
	if ok {
		t.Errorf("rate-limited attempt: ok=true; want false")
	}
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("code=%d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Errorf("Retry-After header missing")
	}
}

// TestRunAuth_SuccessfulAuthResetsRateLimit verifies that a successful
// credential check (MarkSuccess) clears prior failures so the client is not
// rate-limited on subsequent attempts that go bad again.
func TestRunAuth_SuccessfulAuthResetsRateLimit(t *testing.T) {
	actor := &auth.Actor{UserID: "u1", Name: "alice"}
	st := &fakeStore{
		flags:     auth.RepoFlags{PublicRead: false},
		credErr:   auth.ErrInvalidCredential,
		credActor: actor,
		perm:      auth.PermRead,
	}
	limiter := ratelimit.NewLimiter(ratelimit.Config{
		Burst:           3,
		RefillPerMinute: 0,
		SweepInterval:   24 * time.Hour,
	})
	defer limiter.Close()
	rr := &RoutedRequest{Tenant: "acme", Repo: "site", Op: OpUploadPack, RequiredAction: auth.ActionRead}

	// Two failures (still under Burst=3, no 429 yet).
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/acme/site/info/refs", nil)
		r.SetBasicAuth("alice", "wrong")
		r.RemoteAddr = "5.6.7.8:1111"
		RunAuth(w, r, st, rr, limiter, false, nil)
	}
	// Successful attempt -> MarkSuccess resets the bucket.
	st.credErr = nil
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/acme/site/info/refs", nil)
	r.SetBasicAuth("alice", "right")
	r.RemoteAddr = "5.6.7.8:1111"
	if _, ok := RunAuth(w, r, st, rr, limiter, false, nil); !ok {
		t.Fatalf("successful auth: ok=false; status=%d body=%q", w.Code, w.Body.String())
	}

	// 3 more failures should yield 3x 401, never 429 (bucket was reset).
	st.credErr = auth.ErrInvalidCredential
	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/acme/site/info/refs", nil)
		r.SetBasicAuth("alice", "wrong")
		r.RemoteAddr = "5.6.7.8:1111"
		_, ok := RunAuth(w, r, st, rr, limiter, false, nil)
		if ok {
			t.Fatalf("attempt %d: ok=true; expected 401", i)
		}
		if w.Code == http.StatusTooManyRequests {
			t.Errorf("attempt %d: 429 (should have been reset by MarkSuccess)", i)
		}
	}
}

// TestRunAuth_NilLimiterIsNoop verifies that passing nil for the Limiter
// disables rate limiting entirely — even 100 consecutive bad-cred attempts
// keep returning 401, never 429.
func TestRunAuth_NilLimiterIsNoop(t *testing.T) {
	st := &fakeStore{flags: auth.RepoFlags{PublicRead: false}, credErr: auth.ErrInvalidCredential}
	rr := &RoutedRequest{Tenant: "acme", Repo: "site", Op: OpUploadPack, RequiredAction: auth.ActionRead}
	for i := 0; i < 100; i++ {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/acme/site/info/refs", nil)
		r.SetBasicAuth("alice", "wrong")
		r.RemoteAddr = "9.9.9.9:1111"
		_, _ = RunAuth(w, r, st, rr, nil, false, nil)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d: 429 with nil Limiter; want 401 every time", i)
		}
	}
}

// TestRunAuth_ScopeMismatchNotCountedAsFailure pins that a credential
// that verifies but has the wrong scope returns 403 and does NOT count
// toward the rate-limit bucket. Scope denials are policy failures, not
// brute-force signals — counting them would let a misconfigured CI bot
// (right token, wrong tenant) trip itself off-net repeatedly.
func TestRunAuth_ScopeMismatchNotCountedAsFailure(t *testing.T) {
	actor := &auth.Actor{UserID: "u1", Name: "alice"}
	st := &fakeStore{
		flags:     auth.RepoFlags{PublicRead: false},
		credActor: actor,
		credToken: "tok-1",
		credScope: &auth.Scope{Tenant: "OTHER", Repo: "other", Perm: auth.PermRead},
	}
	limiter := ratelimit.NewLimiter(ratelimit.Config{
		Burst:           3,
		RefillPerMinute: 0,
		SweepInterval:   24 * time.Hour,
	})
	defer limiter.Close()
	rr := &RoutedRequest{Tenant: "acme", Repo: "site", Op: OpUploadPack, RequiredAction: auth.ActionRead}

	// 20 scope-mismatch (403) attempts must never produce 429.
	for i := 0; i < 20; i++ {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/acme/site/info/refs", nil)
		r.SetBasicAuth("alice", "right-but-wrong-scope")
		r.RemoteAddr = "5.5.5.5:6789"
		_, ok := RunAuth(w, r, st, rr, limiter, false, nil)
		if ok {
			t.Fatalf("attempt %d: ok=true; expected scope-mismatch 403", i)
		}
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d: 429 from scope mismatch; scope denials must not count", i)
		}
		if w.Code != http.StatusForbidden {
			t.Errorf("attempt %d: code=%d, want 403", i, w.Code)
		}
	}
}

// TestRunAuth_AnonymousReadsDontTripLimiter pins that anonymous reads
// (no Authorization header) on a public-read repo from a clean IP are
// served normally and never produce 429. The IP bucket is checked but
// is empty (anonymous requests carry no credential to fail), so Check
// always returns allowed.
func TestRunAuth_AnonymousReadsDontTripLimiter(t *testing.T) {
	st := &fakeStore{flags: auth.RepoFlags{PublicRead: true}}
	limiter := ratelimit.NewLimiter(ratelimit.Config{
		Burst:           3,
		RefillPerMinute: 0,
		SweepInterval:   24 * time.Hour,
	})
	defer limiter.Close()
	rr := &RoutedRequest{Tenant: "a", Repo: "b", Op: OpUploadPack, RequiredAction: auth.ActionRead}

	for i := 0; i < 50; i++ {
		w := httptest.NewRecorder()
		r := req(t, "POST", "/a/b.git/git-upload-pack", "", "", "")
		r.RemoteAddr = "7.7.7.7:9999"
		_, ok := RunAuth(w, r, st, rr, limiter, false, nil)
		if !ok {
			t.Fatalf("attempt %d: anon read denied (code=%d)", i, w.Code)
		}
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d: 429 on anon read; must never throttle credential-less requests", i)
		}
	}
}

