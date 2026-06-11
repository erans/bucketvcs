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
	verify            func(ctx context.Context, u, p string) (*auth.Actor, error)
	sessions          map[string]*auth.Session // keyed by raw id
	deleteSessionsFor func(ctx context.Context, userID, exceptRawID string) (int64, error)

	// session list/revoke (self-service + admin)
	sessionsForUser     []auth.SessionInfo
	allSessions         []auth.AdminSessionInfo
	revokeCount         int64  // returned by DeleteSessionByHash* (0 => default 1; -1 => 0 "already gone")
	lastRevokeUserID    string // recorded by DeleteSessionByHashForUser
	lastRevokeHash      string // recorded by both revoke-by-hash methods
	lastRevokeAllUserID string // recorded by DeleteSessionsForUser
	repos               func(actor *auth.Actor) []Repo
	findByEmail         func(email string) (*auth.Actor, error)
	findIdentity        func(issuer, subject string) (*auth.Actor, error)
	linkIdentity        func(userID, issuer, subject, email string) error
	perm                auth.Perm // returned by LookupRepoPerm
	permErr             error     // when non-nil, LookupRepoPerm returns it
	getVisibleRepo      func(ctx context.Context, actor *auth.Actor, tenant, name string) (*Repo, error)
	getRepoFlags        func(ctx context.Context, tenant, repo string) (auth.RepoFlags, error)
	setRepoPublic       func(ctx context.Context, tenant, repo string, public bool) error
	renameRepo          func(ctx context.Context, tenant, oldName, newName string) error
	deleteRepo          func(ctx context.Context, tenant, repo string) error
	registerRepoIfNew   func(ctx context.Context, tenant, name string) (bool, error)
	getUserByName       func(ctx context.Context, name string) (*auth.User, error)
	setPassword         func(ctx context.Context, userName, plaintext string) error
	hasPassword         func(ctx context.Context, userName string) (bool, error)

	// token methods
	listTokensForUser func(ctx context.Context, name string) ([]TokenInfo, error)
	getTokenOwner     func(ctx context.Context, id string) (string, error)
	createToken       func(ctx context.Context, id, userID, secretHash, label string, expiresAt *int64, scopes auth.TokenScope) error
	revokeToken       func(ctx context.Context, id string) error
	rotateToken       func(ctx context.Context, id, newSecretHash string) error

	// SSH key methods
	listSSHKeysForUser func(ctx context.Context, userID string) ([]auth.SSHKey, error)
	addSSHKey          func(ctx context.Context, k auth.SSHKey) error
	revokeSSHKey       func(ctx context.Context, keyIDOrPrefix string) error

	// repo access methods (admin tab)
	listRepoGrants       func(ctx context.Context, tenant, repo string) ([]RepoGrant, error)
	grant                func(ctx context.Context, userName, tenant, repo, perm string) error
	revokeRepoPermission func(ctx context.Context, userName, tenant, repo string) error
	listSSHKeysForRepo   func(ctx context.Context, tenant, repo string) ([]auth.SSHKey, error)

	// admin area: global user management
	listUsers       func(ctx context.Context) ([]UserInfo, error)
	createUser      func(ctx context.Context, name string, isAdmin bool) (string, error)
	setUserDisabled func(ctx context.Context, name string, disabled bool) error
	deleteUser      func(ctx context.Context, name string) error
	setEmail        func(ctx context.Context, userName, email string) error

	// optional alias resolver (implements auth.RepoAliasResolver when non-nil)
	resolveAlias func(ctx context.Context, tenant, name string) (string, bool, error)
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
func (f *fakeStore) DeleteSessionsForUser(ctx context.Context, userID, exceptRawID string) (int64, error) {
	f.lastRevokeAllUserID = userID
	if f.deleteSessionsFor != nil {
		return f.deleteSessionsFor(ctx, userID, exceptRawID)
	}
	var n int64
	for raw, s := range f.sessions {
		if s.UserID == userID && raw != exceptRawID {
			delete(f.sessions, raw)
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) ListSessionsForUser(ctx context.Context, userID, currentRawID string) ([]auth.SessionInfo, error) {
	return f.sessionsForUser, nil
}
func (f *fakeStore) DeleteSessionByHashForUser(ctx context.Context, userID, idHash string) (int64, error) {
	f.lastRevokeUserID = userID
	f.lastRevokeHash = idHash
	if f.revokeCount < 0 {
		return 0, nil
	}
	if f.revokeCount != 0 {
		return f.revokeCount, nil
	}
	return 1, nil
}
func (f *fakeStore) ListAllSessions(ctx context.Context, limit int) ([]auth.AdminSessionInfo, int, error) {
	total := len(f.allSessions)
	list := f.allSessions
	if limit > 0 && len(list) > limit {
		list = list[:limit]
	}
	return list, total, nil
}
func (f *fakeStore) SessionOwnerByHash(ctx context.Context, idHash string) (string, string, error) {
	for _, s := range f.allSessions {
		if s.IDHash == idHash {
			return s.UserID, s.UserName, nil
		}
	}
	return "", "", auth.ErrNoSuchUser
}
func (f *fakeStore) DeleteSessionByHash(ctx context.Context, idHash string) (int64, error) {
	f.lastRevokeHash = idHash
	if f.revokeCount < 0 {
		return 0, nil
	}
	if f.revokeCount != 0 {
		return f.revokeCount, nil
	}
	return 1, nil
}
func (f *fakeStore) ListAccessibleRepos(ctx context.Context, actor *auth.Actor) ([]Repo, error) {
	if f.repos == nil {
		return nil, nil
	}
	return f.repos(actor), nil
}
func (f *fakeStore) GetVisibleRepo(ctx context.Context, actor *auth.Actor, tenant, name string) (*Repo, error) {
	if f.getVisibleRepo != nil {
		return f.getVisibleRepo(ctx, actor, tenant, name)
	}
	return nil, nil
}
func (f *fakeStore) LookupRepoPerm(ctx context.Context, actor *auth.Actor, tenant, repo string) (auth.Perm, error) {
	if f.permErr != nil {
		return auth.PermNone, f.permErr
	}
	return f.perm, nil
}
func (f *fakeStore) GetRepoFlags(ctx context.Context, tenant, repo string) (auth.RepoFlags, error) {
	if f.getRepoFlags != nil {
		return f.getRepoFlags(ctx, tenant, repo)
	}
	return auth.RepoFlags{}, nil
}
func (f *fakeStore) SetRepoPublic(ctx context.Context, tenant, repo string, public bool) error {
	if f.setRepoPublic != nil {
		return f.setRepoPublic(ctx, tenant, repo, public)
	}
	return nil
}
func (f *fakeStore) RenameRepo(ctx context.Context, tenant, oldName, newName string) error {
	if f.renameRepo != nil {
		return f.renameRepo(ctx, tenant, oldName, newName)
	}
	return nil
}
func (f *fakeStore) DeleteRepoCascade(ctx context.Context, tenant, repo string) error {
	if f.deleteRepo != nil {
		return f.deleteRepo(ctx, tenant, repo)
	}
	return nil
}
func (f *fakeStore) RegisterRepoIfNew(ctx context.Context, tenant, name string) (bool, error) {
	if f.registerRepoIfNew != nil {
		return f.registerRepoIfNew(ctx, tenant, name)
	}
	return true, nil
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
func (f *fakeStore) ListTokensForUser(ctx context.Context, name string) ([]TokenInfo, error) {
	if f.listTokensForUser != nil {
		return f.listTokensForUser(ctx, name)
	}
	return nil, nil
}
func (f *fakeStore) GetTokenOwner(ctx context.Context, id string) (string, error) {
	if f.getTokenOwner != nil {
		return f.getTokenOwner(ctx, id)
	}
	return "", auth.ErrNoSuchToken
}
func (f *fakeStore) CreateToken(ctx context.Context, id, userID, secretHash, label string, expiresAt *int64, scopes auth.TokenScope) error {
	if f.createToken != nil {
		return f.createToken(ctx, id, userID, secretHash, label, expiresAt, scopes)
	}
	return nil
}
func (f *fakeStore) RevokeToken(ctx context.Context, id string) error {
	if f.revokeToken != nil {
		return f.revokeToken(ctx, id)
	}
	return nil
}
func (f *fakeStore) RotateToken(ctx context.Context, id, newSecretHash string) error {
	if f.rotateToken != nil {
		return f.rotateToken(ctx, id, newSecretHash)
	}
	return nil
}
func (f *fakeStore) ListSSHKeysForUser(ctx context.Context, userID string) ([]auth.SSHKey, error) {
	if f.listSSHKeysForUser != nil {
		return f.listSSHKeysForUser(ctx, userID)
	}
	return nil, nil
}
func (f *fakeStore) AddSSHKey(ctx context.Context, k auth.SSHKey) error {
	if f.addSSHKey != nil {
		return f.addSSHKey(ctx, k)
	}
	return nil
}
func (f *fakeStore) RevokeSSHKey(ctx context.Context, keyIDOrPrefix string) error {
	if f.revokeSSHKey != nil {
		return f.revokeSSHKey(ctx, keyIDOrPrefix)
	}
	return nil
}
func (f *fakeStore) ListRepoGrants(ctx context.Context, tenant, repo string) ([]RepoGrant, error) {
	if f.listRepoGrants != nil {
		return f.listRepoGrants(ctx, tenant, repo)
	}
	return nil, nil
}
func (f *fakeStore) Grant(ctx context.Context, userName, tenant, repo, perm string) error {
	if f.grant != nil {
		return f.grant(ctx, userName, tenant, repo, perm)
	}
	return nil
}
func (f *fakeStore) RevokeRepoPermission(ctx context.Context, userName, tenant, repo string) error {
	if f.revokeRepoPermission != nil {
		return f.revokeRepoPermission(ctx, userName, tenant, repo)
	}
	return nil
}
func (f *fakeStore) ListSSHKeysForRepo(ctx context.Context, tenant, repo string) ([]auth.SSHKey, error) {
	if f.listSSHKeysForRepo != nil {
		return f.listSSHKeysForRepo(ctx, tenant, repo)
	}
	return nil, nil
}
func (f *fakeStore) ListUsers(ctx context.Context) ([]UserInfo, error) {
	if f.listUsers != nil {
		return f.listUsers(ctx)
	}
	return nil, nil
}
func (f *fakeStore) CreateUser(ctx context.Context, name string, isAdmin bool) (string, error) {
	if f.createUser != nil {
		return f.createUser(ctx, name, isAdmin)
	}
	return "fakeid-" + name, nil
}
func (f *fakeStore) SetUserDisabled(ctx context.Context, name string, disabled bool) error {
	if f.setUserDisabled != nil {
		return f.setUserDisabled(ctx, name, disabled)
	}
	return nil
}
func (f *fakeStore) DeleteUser(ctx context.Context, name string) error {
	if f.deleteUser != nil {
		return f.deleteUser(ctx, name)
	}
	return nil
}
func (f *fakeStore) SetEmail(ctx context.Context, userName, email string) error {
	if f.setEmail != nil {
		return f.setEmail(ctx, userName, email)
	}
	return nil
}

// ResolveAlias implements auth.RepoAliasResolver when f.resolveAlias is set.
// The type-assertion in aliasRedirect will succeed only when this method is
// present, giving fine-grained control per test.
func (f *fakeStore) ResolveAlias(ctx context.Context, tenant, name string) (string, bool, error) {
	if f.resolveAlias != nil {
		return f.resolveAlias(ctx, tenant, name)
	}
	return "", false, nil
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
