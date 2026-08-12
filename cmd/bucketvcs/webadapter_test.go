package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/web"
)

func TestWebAdapter_IdentityMethods(t *testing.T) {
	dir := t.TempDir()
	s, err := sqlitestore.Open(filepath.Join(dir, "auth.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "alice", false)
	if err := s.SetEmail(ctx, "alice", "alice@corp.com"); err != nil {
		t.Fatalf("SetEmail: %v", err)
	}

	var ds web.DataStore = newWebAdapter(s)

	a, err := ds.FindUserByEmail(ctx, "alice@corp.com")
	if err != nil || a.UserID != uid {
		t.Fatalf("FindUserByEmail: %+v err %v", a, err)
	}
	if err := ds.LinkIdentity(ctx, uid, "https://i", "sub1", "alice@corp.com"); err != nil {
		t.Fatalf("LinkIdentity: %v", err)
	}
	a2, err := ds.FindIdentity(ctx, "https://i", "sub1")
	if err != nil || a2.UserID != uid {
		t.Fatalf("FindIdentity: %+v err %v", a2, err)
	}
}

// TestWebAdapter_TokensPage_ListsByUserID is the U-3 regression: GET
// /settings/tokens must list the session user's tokens. The handler passes
// the session's UserID (a random id, never the login name) into
// ListTokensForUser; pre-fix the store resolved that value as a user NAME
// (GetUserByName) and returned ErrNoSuchUser, so the page rendered a 500
// for every user. This test drives the real sqlite store through the real
// webAdapter (the production composition) with a live session cookie.
func TestWebAdapter_TokensPage_ListsByUserID(t *testing.T) {
	dir := t.TempDir()
	s, err := sqlitestore.Open(filepath.Join(dir, "auth.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	// A user whose id != name (ids are random) + one self-service token row.
	uid, err := s.CreateUser(ctx, "alice", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if uid == "alice" {
		t.Fatalf("user id unexpectedly equals name; fixture must keep id != name")
	}
	if err := s.CreateToken(ctx, "tok1AAAAAAAAAAAAAAAAAAAA", uid, "h1",
		"ci-token", nil, auth.ScopeRepoRead, "", "", ""); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	// A live web session for alice; the cookie goes through the real session
	// middleware (LookupSession by raw id).
	rawSess, err := s.CreateSession(ctx, uid, "password", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	h := web.NewHandler(web.Deps{Store: newWebAdapter(s)})
	req := httptest.NewRequest(http.MethodGet, "/settings/tokens", nil)
	req.AddCookie(&http.Cookie{Name: "bvcs_session", Value: rawSess})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings/tokens: status %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ci-token") {
		t.Fatalf("token page: label 'ci-token' missing:\n%s", rec.Body.String())
	}

	// The adapter-level call with the handler's exact argument shape (userID)
	// must also succeed against the real store.
	rows, err := newWebAdapter(s).ListTokensForUser(ctx, uid)
	if err != nil {
		t.Fatalf("ListTokensForUser(userID): %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "tok1AAAAAAAAAAAAAAAAAAAA" {
		t.Fatalf("rows = %+v, want the ci-token row", rows)
	}
}

func TestWebAdapter_Satisfies(t *testing.T) {
	dir := t.TempDir()
	s, err := sqlitestore.Open(filepath.Join(dir, "auth.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "alice", false); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.RegisterRepo(ctx, "acme", "demo"); err != nil {
		t.Fatalf("RegisterRepo: %v", err)
	}
	if err := s.SetRepoPublic(ctx, "acme", "demo", true); err != nil {
		t.Fatalf("SetRepoPublic: %v", err)
	}

	var ds web.DataStore = newWebAdapter(s) // must satisfy the interface
	repos, err := ds.ListAccessibleRepos(ctx, (*auth.Actor)(nil))
	if err != nil {
		t.Fatalf("ListAccessibleRepos: %v", err)
	}
	if len(repos) != 1 || repos[0].Name != "demo" || !repos[0].PublicRead {
		t.Fatalf("repos = %+v", repos)
	}
}
