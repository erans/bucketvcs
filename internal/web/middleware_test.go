package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// fakeStore implements DataStore for handler/middleware tests.
type fakeStore struct {
	verify        func(ctx context.Context, u, p string) (*auth.Actor, error)
	sessions      map[string]*auth.Session // keyed by raw id
	repos         func(actor *auth.Actor) []Repo
	findByEmail   func(email string) (*auth.Actor, error)
	findIdentity  func(issuer, subject string) (*auth.Actor, error)
	linkIdentity  func(userID, issuer, subject, email string) error
	perm          auth.Perm // returned by LookupRepoPerm
	permErr       error     // when non-nil, LookupRepoPerm returns it
	getUserByName func(ctx context.Context, name string) (*auth.User, error)
	setPassword   func(ctx context.Context, userName, plaintext string) error
	hasPassword   func(ctx context.Context, userName string) (bool, error)
}

func newFakeStore() *fakeStore { return &fakeStore{sessions: map[string]*auth.Session{}} }

func (f *fakeStore) VerifyPassword(ctx context.Context, u, p string) (*auth.Actor, error) {
	return f.verify(ctx, u, p)
}
func (f *fakeStore) CreateSession(ctx context.Context, userID, provider string, ttl time.Duration) (string, error) {
	id := "sess-" + userID
	f.sessions[id] = &auth.Session{UserID: userID, Name: userID, Provider: provider, ExpiresAt: time.Now().Add(ttl)}
	return id, nil
}
func (f *fakeStore) LookupSession(ctx context.Context, raw string) (*auth.Session, error) {
	s, ok := f.sessions[raw]
	if !ok {
		return nil, auth.ErrNoSession
	}
	return s, nil
}
func (f *fakeStore) TouchSession(ctx context.Context, raw string, ttl time.Duration) error {
	return nil
}
func (f *fakeStore) DeleteSession(ctx context.Context, raw string) error {
	delete(f.sessions, raw)
	return nil
}
func (f *fakeStore) ListAccessibleRepos(ctx context.Context, actor *auth.Actor) ([]Repo, error) {
	if f.repos == nil {
		return nil, nil
	}
	return f.repos(actor), nil
}
func (f *fakeStore) GetVisibleRepo(ctx context.Context, actor *auth.Actor, tenant, name string) (*Repo, error) {
	return nil, nil
}
func (f *fakeStore) LookupRepoPerm(ctx context.Context, actor *auth.Actor, tenant, repo string) (auth.Perm, error) {
	if f.permErr != nil {
		return auth.PermNone, f.permErr
	}
	return f.perm, nil
}
func (f *fakeStore) FindUserByEmail(ctx context.Context, email string) (*auth.Actor, error) {
	if f.findByEmail != nil {
		return f.findByEmail(email)
	}
	return nil, auth.ErrNoSuchUser
}
func (f *fakeStore) FindIdentity(ctx context.Context, issuer, subject string) (*auth.Actor, error) {
	if f.findIdentity != nil {
		return f.findIdentity(issuer, subject)
	}
	return nil, auth.ErrNoSuchUser
}
func (f *fakeStore) LinkIdentity(ctx context.Context, userID, issuer, subject, email string) error {
	if f.linkIdentity != nil {
		return f.linkIdentity(userID, issuer, subject, email)
	}
	return nil
}
func (f *fakeStore) GetUserByName(ctx context.Context, name string) (*auth.User, error) {
	if f.getUserByName != nil {
		return f.getUserByName(ctx, name)
	}
	return &auth.User{ID: name, Name: name}, nil
}
func (f *fakeStore) SetPassword(ctx context.Context, userName, plaintext string) error {
	if f.setPassword != nil {
		return f.setPassword(ctx, userName, plaintext)
	}
	return nil
}
func (f *fakeStore) HasPassword(ctx context.Context, userName string) (bool, error) {
	if f.hasPassword != nil {
		return f.hasPassword(ctx, userName)
	}
	return true, nil
}

func TestSessionMiddleware_LoadsAndAnon(t *testing.T) {
	store := newFakeStore()
	store.sessions["good"] = &auth.Session{UserID: "u1", Name: "alice", ExpiresAt: time.Now().Add(time.Hour)}

	var seen *auth.Session
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = SessionFromContext(r.Context())
		w.WriteHeader(200)
	})
	mw := sessionMiddleware(store, time.Hour)(next)

	// with valid cookie
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "good"})
	mw.ServeHTTP(httptest.NewRecorder(), req)
	if seen == nil || seen.Name != "alice" {
		t.Fatalf("expected session, got %+v", seen)
	}

	// no cookie => anon
	seen = nil
	mw.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if seen != nil {
		t.Fatalf("expected anon, got %+v", seen)
	}

	// stale cookie => anon
	seen = nil
	req2 := httptest.NewRequest("GET", "/", nil)
	req2.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "stale"})
	mw.ServeHTTP(httptest.NewRecorder(), req2)
	if seen != nil {
		t.Fatalf("stale cookie should be anon, got %+v", seen)
	}
}
