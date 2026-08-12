package gateway

import (
	"context"
	"encoding/base64"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/lfs"
)

// Wave-4 U-15 end-to-end regression coverage: a git-lfs-authenticate token must
// authenticate exactly the (tenant, repo) it was minted for, at the op's
// permission (download→read, upload→write) — NOT the unscoped legacy shape
// that read/wrote any repo the backing user could touch. Drives the real
// seam: lfs.IssueSSHToken → sqlitestore.CreateToken → VerifyCredential →
// gateway.RunAuth scope enforcement.

func u15Store(t *testing.T) (*sqlitestore.Store, string) {
	t.Helper()
	s, err := sqlitestore.Open(t.TempDir() + "/auth.db")
	if err != nil {
		t.Fatalf("open auth.db: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	uid, err := s.CreateUser(ctx, "alice", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	// Both repos registered; alice holds write grants on BOTH so that any
	// cross-repo allowance would be visible if the token stayed unscoped.
	for _, repo := range []string{"app", "other"} {
		if err := s.RegisterRepo(ctx, "acme", repo); err != nil {
			t.Fatalf("RegisterRepo: %v", err)
		}
		if err := s.Grant(ctx, "alice", "acme", repo, "write"); err != nil {
			t.Fatalf("Grant: %v", err)
		}
	}
	return s, uid
}

// u15Issue mints a token through the SSH seam and returns the wire value.
func u15Issue(t *testing.T, s *sqlitestore.Store, uid, tenant, repo, op string) string {
	t.Helper()
	resp, err := lfs.IssueSSHToken(context.Background(), s, uid, "alice", tenant, repo, op,
		"https://gw.example", 15*time.Minute)
	if err != nil {
		t.Fatalf("IssueSSHToken: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(resp.Header["Authorization"], "Basic "))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	u, tok, ok := strings.Cut(string(raw), ":")
	if !ok || u != "alice" {
		t.Fatalf("basic credential split failed: %q", string(raw))
	}
	return tok
}

func TestU15_LFSSSHToken_ScopeBound(t *testing.T) {
	s, uid := u15Store(t)
	rrRead := &RoutedRequest{Tenant: "acme", Repo: "app", Op: OpLFSBatch, RequiredAction: auth.ActionRead}
	rrWrite := &RoutedRequest{Tenant: "acme", Repo: "app", Op: OpReceivePack, RequiredAction: auth.ActionWrite}
	rrOther := &RoutedRequest{Tenant: "acme", Repo: "other", Op: OpLFSBatch, RequiredAction: auth.ActionRead}
	rrOtherWrite := &RoutedRequest{Tenant: "acme", Repo: "other", Op: OpReceivePack, RequiredAction: auth.ActionWrite}

	run := func(rr *RoutedRequest, tok string) (int, bool) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/acme/app.git/info/lfs/objects/batch", nil)
		r.SetBasicAuth("alice", tok)
		_, ok := RunAuth(w, r, s, rr, nil, false, nil)
		return w.Code, ok
	}

	tok := u15Issue(t, s, uid, "acme", "app", "download")
	// download token: reads the minted repo...
	if code, ok := run(rrRead, tok); !ok {
		t.Fatalf("download token rejected on its repo read (code=%d)", code)
	}
	// ...but does NOT write it...
	if code, ok := run(rrWrite, tok); ok || code != 403 {
		t.Fatalf("download token write: ok=%v code=%d, want 403", ok, code)
	}
	// ...and does NOT read/write a sibling repo alice owns outright.
	if code, ok := run(rrOther, tok); ok || code != 403 {
		t.Fatalf("download token cross-repo read: ok=%v code=%d, want 403", ok, code)
	}
	if code, ok := run(rrOtherWrite, tok); ok || code != 403 {
		t.Fatalf("download token cross-repo write: ok=%v code=%d, want 403", ok, code)
	}

	tok = u15Issue(t, s, uid, "acme", "app", "upload")
	// upload token: writes (and reads, effective write covers read) its repo...
	if code, ok := run(rrWrite, tok); !ok {
		t.Fatalf("upload token rejected on its repo write (code=%d)", code)
	}
	if code, ok := run(rrRead, tok); !ok {
		t.Fatalf("upload token rejected on its repo read (code=%d)", code)
	}
	// ...but is still pinned to (acme, app).
	if code, ok := run(rrOther, tok); ok || code != 403 {
		t.Fatalf("upload token cross-repo read: ok=%v code=%d, want 403", ok, code)
	}
	if code, ok := run(rrOtherWrite, tok); ok || code != 403 {
		t.Fatalf("upload token cross-repo write: ok=%v code=%d, want 403", ok, code)
	}

	// And the scope must actually surface on the request context for the
	// LFS handler's upload re-check (U-1 wiring).
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/acme/app.git/info/lfs/objects/batch", nil)
	r.SetBasicAuth("alice", tok)
	_, ok := RunAuth(w, r, s, rrRead, nil, false, nil)
	if !ok {
		t.Fatal("upload token rejected")
	}
	if sc := ScopeFromContext(r.Context()); sc == nil || sc.Tenant != "acme" || sc.Repo != "app" || sc.Perm != auth.PermWrite {
		t.Fatalf("ScopeFromContext = %+v, want {acme app write}", sc)
	}
}
