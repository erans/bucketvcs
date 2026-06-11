# Observability surface: sessions + audit viewer — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Surface the server's observability data in the browser — session management (self-service + admin) and a bucket-read audit-log viewer (global + per-repo).

**Architecture:** Sessions use the real queryable `sessions` table via the existing web `DataStore` (extended). The audit viewer is a new read-only `internal/auditlog` package (the read-side sibling of `internal/shiplog`) that lists/fetches/decodes/filters the shipped `sys/logs/activity/…ndjson.gz` objects; `internal/web` consumes it through a narrow `AuditReader` interface wired from the `ObjectStore` in `serve.go`. All new web pages reuse the existing admin/settings/repo-settings chassis.

**Tech Stack:** Go, `database/sql` (sqlite/postgres) for sessions, `compress/gzip`+`encoding/json` for the audit decode, `internal/storage.ObjectStore` for reads, `html/template`, the `internal/web` chassis.

**Spec:** `docs/superpowers/specs/2026-06-10-observability-surface-design.md`

**Reference reading before starting:**
- `internal/auth/sqlitestore/sessions.go` (`hashSessionID`, `CreateSession`, `DeleteSessionsForUser`, the `sessions` columns: `id_hash,user_id,provider,created_at,expires_at,last_seen`) and the `users` table (`id`,`name`).
- `internal/auth` types (`Session`, `User`) — where `SessionInfo` will live.
- `internal/web/datastore.go` (the `DataStore` interface — already has `CreateSession`/`DeleteSessionsForUser`) and `internal/web/*_test.go` (the `fakeStore`/`newFakeStore` test fake that satisfies `DataStore` — it must gain the new methods).
- `internal/web/settings.go` (`handleSettings`, `handlePasswordChange` — `sess.UserID`, `r.Cookie(sessionCookieName)`, `postGuard`, `redirectFlash`, the session-revoke pattern), `internal/web/admin.go` (`handleAdminUsers` — `requireAdmin`, list→render), `internal/web/handler.go` (mux `HandleFunc` registrations ~line 130-152; `Deps`/`server` structs ~line 30-72), `internal/web/reposettings.go` (`handleRepoSettings` tab switch), `internal/web/services.go` (consumer-interface pattern).
- `internal/web/templates/_partials.html` (`settingsnav`, `adminnav`, `reposettingsnav`), `admin_users.html` (table + inline POST-form action pattern), `settings.html`.
- `internal/storage/objectstore.go` + `options.go` (`ObjectStore.List`/`Get`, `ListOptions{MaxKeys,ContinuationToken}`, `ListPage{Objects []ObjectMetadata,NextToken}`, `ObjectMetadata{Key,Size,…}`, `Object{Body io.ReadCloser,Metadata}`, `GetOptions`).
- `internal/shiplog/engine.go` (`DefaultPrefix="sys/logs"`, `StreamActivity="activity"`) and `tap.go` (`marshal` — the `{ts,level,event,…attrs}` JSON shape, ts is RFC3339Nano).
- `cmd/bucketvcs/serve.go` (~line 909 where `web.Deps{…}` is built; `store` is the in-scope `ObjectStore`; the shiplog prefix/config).

---

## Task 1: Session store methods + DataStore extension

**Files:**
- Create: `internal/auth/sessioninfo.go`
- Modify: `internal/auth/sqlitestore/sessions.go`
- Modify: `internal/web/datastore.go` (interface) + the web test fake
- Test: `internal/auth/sqlitestore/sessions_test.go`

- [ ] **Step 1: Add the view types** in `internal/auth/sessioninfo.go`

```go
package auth

// SessionInfo is a non-sensitive view of one session row for the UI. It never
// carries the raw session id (that exists only in the client cookie); IDHash is
// the stored SHA-256 hex and is safe to render and accept back on a revoke POST.
type SessionInfo struct {
	IDHash    string
	Provider  string
	CreatedAt int64 // unix seconds
	ExpiresAt int64
	LastSeen  int64
	IsCurrent bool // true for the requesting session (set by ListSessionsForUser)
}

// AdminSessionInfo augments SessionInfo with owner identity for the admin list.
type AdminSessionInfo struct {
	SessionInfo
	UserID   string
	UserName string
}
```

- [ ] **Step 2: Write the failing test** in `sessions_test.go`

Read the file first for the existing store-test harness (how a `*Store` + a user are created). Mirror it. Add:

```go
func TestListSessionsForUser_MarksCurrentAndOrders(t *testing.T) {
	st := newTestStore(t) // use the real helper name
	ctx := context.Background()
	uid := createTestUser(t, st, "alice") // real helper; returns user id
	raw1, _ := st.CreateSession(ctx, uid, "password", time.Hour)
	raw2, _ := st.CreateSession(ctx, uid, "oidc", time.Hour)

	list, err := st.ListSessionsForUser(ctx, uid, raw2)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(list))
	}
	var sawCurrent bool
	for _, si := range list {
		if si.IDHash == "" || si.Provider == "" {
			t.Errorf("missing fields: %+v", si)
		}
		if si.IsCurrent {
			sawCurrent = true
		}
	}
	if !sawCurrent {
		t.Errorf("no session marked current for raw2")
	}
	_ = raw1
}

func TestDeleteSessionByHashForUser_IsUserScoped(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	a := createTestUser(t, st, "alice")
	b := createTestUser(t, st, "bob")
	raw, _ := st.CreateSession(ctx, a, "password", time.Hour)
	bobList, _ := st.ListSessionsForUser(ctx, a, raw)
	hash := bobList[0].IDHash
	// Bob tries to delete Alice's session by hash → 0 rows (user-scoped).
	n, err := st.DeleteSessionByHashForUser(ctx, b, hash)
	if err != nil || n != 0 {
		t.Fatalf("cross-user delete should affect 0 rows, got n=%d err=%v", n, err)
	}
	// Alice deletes her own → 1 row.
	n, err = st.DeleteSessionByHashForUser(ctx, a, hash)
	if err != nil || n != 1 {
		t.Fatalf("self delete want 1 row, got n=%d err=%v", n, err)
	}
}

func TestListAllSessionsAndDeleteByHash(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	a := createTestUser(t, st, "alice")
	raw, _ := st.CreateSession(ctx, a, "password", time.Hour)
	all, err := st.ListAllSessions(ctx)
	if err != nil || len(all) != 1 || all[0].UserName != "alice" {
		t.Fatalf("ListAllSessions = %+v err=%v", all, err)
	}
	n, err := st.DeleteSessionByHash(ctx, all[0].IDHash)
	if err != nil || n != 1 {
		t.Fatalf("DeleteSessionByHash want 1, got %d err=%v", n, err)
	}
	_ = raw
}
```

Adapt `newTestStore`/`createTestUser` to the real helpers in the file. Run `go test ./internal/auth/sqlitestore/ -run 'TestListSessions|TestDeleteSessionByHash|TestListAllSessions' -v` → FAIL (undefined).

- [ ] **Step 3: Implement the four methods** in `sessions.go` (after `DeleteSessionsForUser`)

```go
// ListSessionsForUser returns the user's sessions newest-first, marking the one
// matching currentRawID (the request cookie's raw id) as IsCurrent. The raw id
// is hashed here so it never leaves the store; the returned IDHash is safe to
// render. currentRawID may be "" (nothing marked current).
func (s *Store) ListSessionsForUser(ctx context.Context, userID, currentRawID string) ([]auth.SessionInfo, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id_hash, provider, created_at, expires_at, last_seen
		   FROM sessions WHERE user_id = ? ORDER BY last_seen DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	curHash := ""
	if currentRawID != "" {
		curHash = hashSessionID(currentRawID)
	}
	var out []auth.SessionInfo
	for rows.Next() {
		var si auth.SessionInfo
		if err := rows.Scan(&si.IDHash, &si.Provider, &si.CreatedAt, &si.ExpiresAt, &si.LastSeen); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		si.IsCurrent = si.IDHash == curHash
		out = append(out, si)
	}
	return out, rows.Err()
}

// DeleteSessionByHashForUser deletes one session by its stored hash, scoped to
// userID so a user cannot revoke another user's session. Returns rows affected.
func (s *Store) DeleteSessionByHashForUser(ctx context.Context, userID, idHash string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE user_id = ? AND id_hash = ?`, userID, idHash)
	if err != nil {
		return 0, fmt.Errorf("delete session by hash for user: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ListAllSessions returns every session with owner identity (admin view),
// newest-first.
func (s *Store) ListAllSessions(ctx context.Context) ([]auth.AdminSessionInfo, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.id_hash, s.provider, s.created_at, s.expires_at, s.last_seen, s.user_id, u.name
		   FROM sessions s JOIN users u ON u.id = s.user_id
		  ORDER BY s.last_seen DESC`)
	if err != nil {
		return nil, fmt.Errorf("list all sessions: %w", err)
	}
	defer rows.Close()
	var out []auth.AdminSessionInfo
	for rows.Next() {
		var a auth.AdminSessionInfo
		if err := rows.Scan(&a.IDHash, &a.Provider, &a.CreatedAt, &a.ExpiresAt, &a.LastSeen, &a.UserID, &a.UserName); err != nil {
			return nil, fmt.Errorf("scan admin session: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteSessionByHash deletes one session by hash regardless of owner (admin).
func (s *Store) DeleteSessionByHash(ctx context.Context, idHash string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, idHash)
	if err != nil {
		return 0, fmt.Errorf("delete session by hash: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
```

- [ ] **Step 4: Extend the `DataStore` interface** (`internal/web/datastore.go`), after `DeleteSessionsForUser`:

```go
	ListSessionsForUser(ctx context.Context, userID, currentRawID string) ([]auth.SessionInfo, error)
	DeleteSessionByHashForUser(ctx context.Context, userID, idHash string) (int64, error)
	ListAllSessions(ctx context.Context) ([]auth.AdminSessionInfo, error)
	DeleteSessionByHash(ctx context.Context, idHash string) (int64, error)
```
(`auth` is already imported in datastore.go.) Then add these four methods to the web **test fake** that satisfies `DataStore` (find it in the web test files — it's the struct with the other `…Session…` methods). Implement them to return canned data driven by struct fields (e.g. `sessionsForUser []auth.SessionInfo`, `allSessions []auth.AdminSessionInfo`, and record delete calls), so Tasks 2/3 can drive behavior.

- [ ] **Step 5: Run tests + build**

Run: `go test ./internal/auth/sqlitestore/ -run 'TestListSessions|TestDeleteSessionByHash|TestListAllSessions' -v` → PASS. `go build ./...` (web fake must satisfy the grown interface — fix it if not). `go vet ./internal/auth/... ./internal/web/`.

- [ ] **Step 6: Commit**

```bash
git add internal/auth/sessioninfo.go internal/auth/sqlitestore/sessions.go internal/auth/sqlitestore/sessions_test.go internal/web/datastore.go internal/web/*_test.go
git commit -m "feat(auth): session list/revoke methods (user-scoped + admin) + DataStore extension"
```

---

## Task 2: Self-service `/settings/sessions`

**Files:**
- Create: `internal/web/settings_sessions.go`
- Create: `internal/web/templates/settings_sessions.html`
- Modify: `internal/web/handler.go` (mux), `internal/web/templates/_partials.html` (settingsnav)
- Test: `internal/web/settings_sessions_test.go`

- [ ] **Step 1: Write failing tests** (read an existing settings test — e.g. tokens — for the authed-GET + `csrfPost` harness)

```go
func TestSessionsPage_ListsAndMarksCurrent(t *testing.T) {
	fake := /* fakeStore with sessionsForUser = []auth.SessionInfo{{IDHash:"h1",Provider:"password",IsCurrent:true},{IDHash:"h2",Provider:"oidc"}} */
	srv := /* server with that store + a logged-in session */
	body, code := /* authed GET /settings/sessions */
	if code != 200 { t.Fatalf("status %d", code) }
	for _, want := range []string{"password", "oidc", "current", "h2"} {
		if !strings.Contains(body, want) { t.Fatalf("missing %q: %s", want, body) }
	}
}

func TestSessionsRevoke_UserScoped(t *testing.T) {
	fake := /* fakeStore recording DeleteSessionByHashForUser calls */
	srv := /* ... logged-in as user U ... */
	rec := /* authed csrfPost /settings/sessions/revoke with id_hash=h2 */
	if rec.Code != http.StatusSeeOther { t.Fatalf("want 303, got %d", rec.Code) }
	if fake.lastRevokeUserID != "<U's id>" || fake.lastRevokeHash != "h2" {
		t.Fatalf("revoke not user-scoped: %+v", fake)
	}
}

func TestSessionsRevokeAll_Others(t *testing.T) {
	// authed csrfPost /settings/sessions/revoke-all → calls DeleteSessionsForUser(userID, currentRawID); 303 flash
}
```

Match the real harness (server constructor, session cookie, `csrfPost`). Run `go test ./internal/web/ -run TestSessions -v` → FAIL (404, no handler).

- [ ] **Step 2: Implement handlers** in `internal/web/settings_sessions.go`

```go
package web

import (
	"log/slog"
	"net/http"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

type sessionsData struct {
	base
	Sessions []auth.SessionInfo
}

func (s *server) handleSessionsPage(w http.ResponseWriter, r *http.Request) {
	if !s.requireUser(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		s.renderError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	sess := SessionFromContext(r.Context())
	curRaw := ""
	if c, err := r.Cookie(sessionCookieName); err == nil {
		curRaw = c.Value
	}
	list, err := s.store.ListSessionsForUser(r.Context(), sess.UserID, curRaw)
	if err != nil {
		s.logger.Error("sessions: list", "user", sess.Name, "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	d := sessionsData{
		base:     base{Session: sess, CSRF: issueCSRF(w, requestIsTLS(r, s.trustProxy)), Flash: takeFlash(w, r)},
		Sessions: list,
	}
	if err := s.renderBuffered(w, "settings_sessions.html", d); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "render error")
		return
	}
	EmitRequestMetric(r.Context(), s.logger, "settings_sessions", http.StatusOK)
}

func (s *server) handleSessionRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.requireUser(w, r) {
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	sess := SessionFromContext(r.Context())
	idHash := r.PostFormValue("id_hash")
	if idHash == "" {
		s.redirectFlash(w, r, "/settings/sessions", "no session specified")
		return
	}
	n, err := s.store.DeleteSessionByHashForUser(r.Context(), sess.UserID, idHash)
	if err != nil {
		s.logger.Error("sessions: revoke", "user", sess.Name, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "session", "revoke", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "auth.session.revoked",
		slog.String("user", sess.Name), slog.Int64("count", n))
	EmitAdminActionMetric(r.Context(), s.logger, "session", "revoke", "ok")
	if n == 0 {
		s.redirectFlash(w, r, "/settings/sessions", "session already gone")
		return
	}
	s.redirectFlash(w, r, "/settings/sessions", "session revoked")
}

func (s *server) handleSessionRevokeAll(w http.ResponseWriter, r *http.Request) {
	if !s.requireUser(w, r) {
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	sess := SessionFromContext(r.Context())
	curRaw := ""
	if c, err := r.Cookie(sessionCookieName); err == nil {
		curRaw = c.Value
	}
	n, err := s.store.DeleteSessionsForUser(r.Context(), sess.UserID, curRaw)
	if err != nil {
		s.logger.Error("sessions: revoke all", "user", sess.Name, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "session", "revoke_all", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "auth.session.revoked_all",
		slog.String("user", sess.Name), slog.Int64("count", n))
	EmitAdminActionMetric(r.Context(), s.logger, "session", "revoke_all", "ok")
	s.redirectFlash(w, r, "/settings/sessions", fmt.Sprintf("%d other session(s) signed out", n))
}
```
(add `fmt` import.) Confirm `requireUser`, `sessionCookieName`, `postGuard`, `redirectFlash`, `issueCSRF`, `requestIsTLS`, `takeFlash`, `EmitAdminActionMetric`, `emitAdmin` names against settings.go.

- [ ] **Step 3: Register routes** in `handler.go` (next to the other `/settings/*`):

```go
	s.mux.HandleFunc("/settings/sessions", s.handleSessionsPage)
	s.mux.HandleFunc("/settings/sessions/revoke", s.handleSessionRevoke)
	s.mux.HandleFunc("/settings/sessions/revoke-all", s.handleSessionRevokeAll)
```

- [ ] **Step 4: settingsnav link** in `_partials.html` `settingsnav`:
```html
  <a href="/settings">profile</a> · <a href="/settings/tokens">tokens</a> · <a href="/settings/keys">ssh keys</a> · <a href="/settings/sessions">sessions</a>
```

- [ ] **Step 5: Template** `internal/web/templates/settings_sessions.html`

```html
{{define "title"}}sessions · settings · bucketvcs{{end}}
{{define "content"}}
<div class="settings">
  {{template "settingsnav" .}}
  <h2>active sessions</h2>
  {{if .Sessions}}
  <table>
    <thead><tr><th>provider</th><th>created</th><th>last seen</th><th>expires</th><th>actions</th></tr></thead>
    <tbody>
    {{range .Sessions}}
    <tr>
      <td>{{.Provider}}{{if .IsCurrent}} <span class="badge">current</span>{{end}}</td>
      <td title="{{abstime .CreatedAt}}">{{reltime .CreatedAt}}</td>
      <td title="{{abstime .LastSeen}}">{{reltime .LastSeen}}</td>
      <td title="{{abstime .ExpiresAt}}">{{reltime .ExpiresAt}}</td>
      <td class="mono">
        {{if .IsCurrent}}<span class="empty">— log out to end —</span>
        {{else}}
        <form method="post" action="/settings/sessions/revoke" class="inline">
          <input type="hidden" name="csrf_token" value="{{$.CSRF}}">
          <input type="hidden" name="id_hash" value="{{.IDHash}}">
          <button type="submit">revoke</button>
        </form>
        {{end}}
      </td>
    </tr>
    {{end}}
    </tbody>
  </table>
  <form method="post" action="/settings/sessions/revoke-all" class="inline">
    <input type="hidden" name="csrf_token" value="{{.CSRF}}">
    <button type="submit">revoke all other sessions</button>
  </form>
  {{else}}
  <p class="empty">no active sessions.</p>
  {{end}}
</div>
{{end}}
{{template "base" .}}
```
Register `settings_sessions.html` in `render.go`'s page list (or `renderBuffered` 500s — confirm the list location, same as prior phases). Confirm `abstime`/`reltime` funcs accept int64 unix seconds (the user table uses them the same way in admin_users.html).

- [ ] **Step 6: Run + commit**

`go test ./internal/web/ -run TestSessions -v` → PASS. `go test ./internal/web/ 2>&1 | tail -3`, `go build ./...`, `go vet`, gofmt.
```bash
git add internal/web/settings_sessions.go internal/web/templates/settings_sessions.html internal/web/handler.go internal/web/templates/_partials.html internal/web/render.go internal/web/settings_sessions_test.go
git commit -m "feat(web): self-service /settings/sessions (list + revoke + revoke-all)"
```

---

## Task 3: Admin `/admin/sessions`

**Files:**
- Create: `internal/web/admin_sessions.go`, `internal/web/templates/admin_sessions.html`
- Modify: `internal/web/handler.go` (mux), `_partials.html` (adminnav), `render.go`
- Test: `internal/web/admin_sessions_test.go`

- [ ] **Step 1: Failing tests**

```go
func TestAdminSessions_ListsAllAndRevoke(t *testing.T) {
	// fakeStore.allSessions = []auth.AdminSessionInfo{{SessionInfo:{IDHash:"h1",Provider:"password"},UserName:"alice"}}
	// admin session; GET /admin/sessions → 200 contains "alice","password","h1"
	// non-admin GET /admin/sessions → 404 (requireAdmin)
	// admin csrfPost /admin/sessions/revoke id_hash=h1 → 303; fake records DeleteSessionByHash("h1")
}
```
Mirror `admin_users_test.go` for the admin-session harness + non-admin 404 assertion. Run → FAIL.

- [ ] **Step 2: Implement** `admin_sessions.go` (mirror `handleAdminUsers`)

```go
package web

import (
	"log/slog"
	"net/http"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

type adminSessionsData struct {
	base
	Sessions []auth.AdminSessionInfo
}

func (s *server) handleAdminSessions(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		s.renderError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	list, err := s.store.ListAllSessions(r.Context())
	if err != nil {
		s.logger.Error("admin sessions: list", "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	d := adminSessionsData{
		base:     base{Session: SessionFromContext(r.Context()), CSRF: issueCSRF(w, requestIsTLS(r, s.trustProxy)), Flash: takeFlash(w, r)},
		Sessions: list,
	}
	if err := s.renderBuffered(w, "admin_sessions.html", d); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "render error")
		return
	}
	EmitRequestMetric(r.Context(), s.logger, "admin_sessions", http.StatusOK)
}

func (s *server) handleAdminSessionRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	idHash := r.PostFormValue("id_hash")
	if idHash == "" {
		s.redirectFlash(w, r, "/admin/sessions", "no session specified")
		return
	}
	n, err := s.store.DeleteSessionByHash(r.Context(), idHash)
	if err != nil {
		s.logger.Error("admin sessions: revoke", "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "session", "admin_revoke", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "auth.session.admin_revoked", slog.String("id_hash", idHash), slog.Int64("count", n))
	EmitAdminActionMetric(r.Context(), s.logger, "session", "admin_revoke", "ok")
	if n == 0 {
		s.redirectFlash(w, r, "/admin/sessions", "session already gone")
		return
	}
	s.redirectFlash(w, r, "/admin/sessions", "session revoked")
}
```

- [ ] **Step 3: mux + adminnav + template + render registration**

`handler.go`:
```go
	s.mux.HandleFunc("/admin/sessions", s.handleAdminSessions)
	s.mux.HandleFunc("/admin/sessions/revoke", s.handleAdminSessionRevoke)
```
`_partials.html` `adminnav`: append ` · <a href="/admin/sessions">sessions</a>`.
`admin_sessions.html`:
```html
{{define "title"}}sessions · admin · bucketvcs{{end}}
{{define "content"}}
<div class="settings">
  {{template "adminnav" .}}
  <h2>sessions</h2>
  {{if .Sessions}}
  <table>
    <thead><tr><th>user</th><th>provider</th><th>created</th><th>last seen</th><th>expires</th><th>actions</th></tr></thead>
    <tbody>
    {{range .Sessions}}
    <tr>
      <td class="mono">{{.UserName}}</td>
      <td>{{.Provider}}</td>
      <td title="{{abstime .CreatedAt}}">{{reltime .CreatedAt}}</td>
      <td title="{{abstime .LastSeen}}">{{reltime .LastSeen}}</td>
      <td title="{{abstime .ExpiresAt}}">{{reltime .ExpiresAt}}</td>
      <td>
        <form method="post" action="/admin/sessions/revoke" class="inline">
          <input type="hidden" name="csrf_token" value="{{$.CSRF}}">
          <input type="hidden" name="id_hash" value="{{.IDHash}}">
          <button type="submit">revoke</button>
        </form>
      </td>
    </tr>
    {{end}}
    </tbody>
  </table>
  {{else}}
  <p class="empty">no active sessions.</p>
  {{end}}
</div>
{{end}}
{{template "base" .}}
```
Register `admin_sessions.html` in `render.go`.

- [ ] **Step 4: Run + commit**

`go test ./internal/web/ -run 'TestAdminSessions' -v` → PASS; `go test ./internal/web/ 2>&1 | tail -3`; build/vet/gofmt.
```bash
git add internal/web/admin_sessions.go internal/web/templates/admin_sessions.html internal/web/handler.go internal/web/templates/_partials.html internal/web/render.go internal/web/admin_sessions_test.go
git commit -m "feat(web): admin /admin/sessions (list all + revoke)"
```

---

## Task 4: `internal/auditlog` — Event + DecodeGz + Filter

**Files:**
- Create: `internal/auditlog/event.go`, `internal/auditlog/decode.go`, `internal/auditlog/filter.go`
- Test: `internal/auditlog/decode_test.go`, `internal/auditlog/filter_test.go`

- [ ] **Step 1: Failing tests**

`decode_test.go`:
```go
package auditlog

import (
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
)

func gzLines(lines ...string) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte(strings.Join(lines, "\n")))
	zw.Close()
	return buf.Bytes()
}

func TestDecodeGz_TypedFieldsAndAttrs(t *testing.T) {
	raw := gzLines(
		`{"ts":"2026-06-10T12:00:00Z","level":"INFO","event":"policy.ref.rejected","tenant":"acme","repo":"app","actor":"alice","reason":"blocked"}`,
		`{"ts":"2026-06-10T12:01:00Z","level":"INFO","event":"auth.session.created","user":"bob"}`,
		`not json`,
	)
	evs, skipped, err := DecodeGz(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || skipped != 1 {
		t.Fatalf("got %d events skipped=%d", len(evs), skipped)
	}
	if evs[0].Event != "policy.ref.rejected" || evs[0].Tenant != "acme" || evs[0].Repo != "app" || evs[0].Actor != "alice" {
		t.Fatalf("ev0 fields: %+v", evs[0])
	}
	if evs[0].Attrs["reason"] != "blocked" {
		t.Fatalf("attrs missing reason: %+v", evs[0].Attrs)
	}
	if evs[1].Actor != "bob" { // "user" falls back to Actor
		t.Fatalf("ev1 actor from user: %+v", evs[1])
	}
	if evs[0].Ts.IsZero() {
		t.Fatalf("ts not parsed")
	}
}

func TestDecodeGz_NotGzip(t *testing.T) {
	if _, _, err := DecodeGz(strings.NewReader("plain")); err == nil {
		t.Fatal("want gzip error")
	}
}
```

`filter_test.go`:
```go
package auditlog

import (
	"testing"
	"time"
)

func TestFilter_Match(t *testing.T) {
	e := Event{Event: "policy.ref.rejected", Tenant: "acme", Repo: "app", Actor: "alice", Ts: time.Unix(1000, 0)}
	cases := []struct {
		f    Filter
		want bool
	}{
		{Filter{}, true},
		{Filter{EventPrefix: "policy."}, true},
		{Filter{EventPrefix: "auth."}, false},
		{Filter{Tenant: "acme", Repo: "app"}, true},
		{Filter{Tenant: "other"}, false},
		{Filter{Actor: "bob"}, false},
		{Filter{Since: time.Unix(2000, 0)}, false},
		{Filter{Until: time.Unix(500, 0)}, false},
		{Filter{Since: time.Unix(500, 0), Until: time.Unix(2000, 0)}, true},
	}
	for i, c := range cases {
		if got := c.f.Match(e); got != c.want {
			t.Errorf("case %d: got %v want %v (f=%+v)", i, got, c.want, c.f)
		}
	}
}
```
Run `go test ./internal/auditlog/ -v` → FAIL (package/functions undefined).

- [ ] **Step 2: Implement** `event.go`

```go
// Package auditlog reads the activity (audit) stream that internal/shiplog
// writes to object storage as gzipped NDJSON, decoding it into Events for the
// web audit viewer. It is the read-side counterpart of internal/shiplog.
package auditlog

import "time"

// Event is one decoded activity record (the JSON shiplog's TapHandler writes).
type Event struct {
	Ts     time.Time
	Level  string
	Event  string
	Tenant string
	Repo   string
	Actor  string
	Attrs  map[string]any // all fields except the lifted ts/level/event/tenant/repo
}
```

`decode.go`:
```go
package auditlog

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// maxObjectDecompressed caps per-object decompressed reads (compression-bomb /
// OOM guard). Activity objects are far smaller in practice.
const maxObjectDecompressed = 64 << 20

// DecodeGz gunzips r and decodes each NDJSON line into an Event. Malformed or
// empty lines are skipped and counted (returned int). A gzip-level error
// returns the error; a single bad line never fails the batch.
func DecodeGz(r io.Reader) ([]Event, int, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, 0, fmt.Errorf("auditlog: gunzip: %w", err)
	}
	defer gz.Close()
	sc := bufio.NewScanner(io.LimitReader(gz, maxObjectDecompressed))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // tolerate long lines
	var out []Event
	skipped := 0
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if json.Unmarshal(line, &m) != nil {
			skipped++
			continue
		}
		out = append(out, eventFromMap(m))
	}
	if err := sc.Err(); err != nil {
		return out, skipped, fmt.Errorf("auditlog: scan: %w", err)
	}
	return out, skipped, nil
}

func eventFromMap(m map[string]any) Event {
	e := Event{
		Level:  asString(m["level"]),
		Event:  asString(m["event"]),
		Tenant: asString(m["tenant"]),
		Repo:   asString(m["repo"]),
		Ts:     parseTime(asString(m["ts"])),
		Attrs:  map[string]any{},
	}
	if e.Actor = asString(m["actor"]); e.Actor == "" {
		e.Actor = asString(m["user"])
	}
	for k, v := range m {
		switch k {
		case "ts", "level", "event", "tenant", "repo":
			// lifted into typed fields
		default:
			e.Attrs[k] = v // actor/user kept here too for the details view
		}
	}
	return e
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	return time.Time{}
}
```

`filter.go`:
```go
package auditlog

import (
	"strings"
	"time"
)

// Filter narrows Events in-memory. Zero-valued fields match anything.
type Filter struct {
	EventPrefix  string
	Tenant       string
	Repo         string
	Actor        string
	Since, Until time.Time
}

// Match reports whether e satisfies f.
func (f Filter) Match(e Event) bool {
	if f.EventPrefix != "" && !strings.HasPrefix(e.Event, f.EventPrefix) {
		return false
	}
	if f.Tenant != "" && e.Tenant != f.Tenant {
		return false
	}
	if f.Repo != "" && e.Repo != f.Repo {
		return false
	}
	if f.Actor != "" && e.Actor != f.Actor {
		return false
	}
	if !f.Since.IsZero() && e.Ts.Before(f.Since) {
		return false
	}
	if !f.Until.IsZero() && e.Ts.After(f.Until) {
		return false
	}
	return true
}
```

- [ ] **Step 3: Run + commit**

`go test ./internal/auditlog/ -v` → PASS. `go vet ./internal/auditlog/`, gofmt.
```bash
git add internal/auditlog/event.go internal/auditlog/decode.go internal/auditlog/filter.go internal/auditlog/decode_test.go internal/auditlog/filter_test.go
git commit -m "feat(auditlog): Event + DecodeGz (NDJSON.gz) + Filter"
```

---

## Task 5: `internal/auditlog.Reader` — paginated read

**Files:**
- Create: `internal/auditlog/reader.go`
- Test: `internal/auditlog/reader_test.go`

- [ ] **Step 1: Failing test** with a fake object store

```go
package auditlog

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// fakeStore implements the auditlog ObjectStore slice from an in-memory map.
type fakeStore struct{ objs map[string][]byte }

func (f *fakeStore) List(ctx context.Context, prefix string, opts *storage.ListOptions) (*storage.ListPage, error) {
	var keys []string
	for k := range f.objs {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			keys = append(keys, k)
		}
	}
	sortStrings(keys) // ascending; implement with sort.Strings
	var md []storage.ObjectMetadata
	for _, k := range keys {
		md = append(md, storage.ObjectMetadata{Key: k, Size: int64(len(f.objs[k]))})
	}
	return &storage.ListPage{Objects: md}, nil
}
func (f *fakeStore) Get(ctx context.Context, key string, opts *storage.GetOptions) (*storage.Object, error) {
	b, ok := f.objs[key]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return &storage.Object{Body: io.NopCloser(bytes.NewReader(b)), Metadata: storage.ObjectMetadata{Key: key, Size: int64(len(b))}}, nil
}

func TestReaderPage_NewestFirstAndCursor(t *testing.T) {
	fs := &fakeStore{objs: map[string][]byte{
		"sys/logs/activity/2026/06/10/120000-i-000001.ndjson.gz": gzLines(`{"ts":"2026-06-10T12:00:00Z","event":"a","tenant":"acme","repo":"app"}`),
		"sys/logs/activity/2026/06/10/130000-i-000002.ndjson.gz": gzLines(`{"ts":"2026-06-10T13:00:00Z","event":"b","tenant":"acme","repo":"app"}`),
		"sys/logs/activity/2026/06/10/140000-i-000003.ndjson.gz": gzLines(`{"ts":"2026-06-10T14:00:00Z","event":"c","tenant":"other","repo":"x"}`),
	}}
	r := NewReader(fs, "sys/logs")
	r.ObjectsPerPage = 2
	evs, next, err := r.Page(context.Background(), Filter{}, "")
	if err != nil {
		t.Fatal(err)
	}
	// newest two objects (140000 then 130000), events sorted newest-first
	if len(evs) != 2 || evs[0].Event != "c" || evs[1].Event != "b" {
		t.Fatalf("page1 = %+v", eventNames(evs))
	}
	if next == "" {
		t.Fatalf("expected a cursor (older objects remain)")
	}
	evs2, next2, _ := r.Page(context.Background(), Filter{}, next)
	if len(evs2) != 1 || evs2[0].Event != "a" || next2 != "" {
		t.Fatalf("page2 = %+v next2=%q", eventNames(evs2), next2)
	}
}

func TestReaderPage_CrossTenantFilterExcludes(t *testing.T) {
	fs := &fakeStore{objs: map[string][]byte{
		"sys/logs/activity/2026/06/10/120000-i-1.ndjson.gz": gzLines(
			`{"ts":"2026-06-10T12:00:00Z","event":"a","tenant":"acme","repo":"app"}`,
			`{"ts":"2026-06-10T12:00:01Z","event":"b","tenant":"other","repo":"app"}`,
		),
	}}
	r := NewReader(fs, "sys/logs")
	evs, _, err := r.Page(context.Background(), Filter{Tenant: "acme", Repo: "app"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Tenant != "acme" {
		t.Fatalf("cross-tenant leak: %+v", eventNames(evs))
	}
}

func TestReaderPage_EmptyStore(t *testing.T) {
	r := NewReader(&fakeStore{objs: map[string][]byte{}}, "sys/logs")
	evs, next, err := r.Page(context.Background(), Filter{}, "")
	if err != nil || len(evs) != 0 || next != "" {
		t.Fatalf("empty store: evs=%d next=%q err=%v", len(evs), next, err)
	}
}
```
Add helpers `sortStrings` (sort.Strings) and `eventNames`. Run `go test ./internal/auditlog/ -run TestReaderPage -v` → FAIL.

- [ ] **Step 2: Implement** `reader.go`

```go
package auditlog

import (
	"context"
	"sort"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// ObjectStore is the minimal storage slice the Reader needs (the real
// *storage.ObjectStore satisfies it).
type ObjectStore interface {
	List(ctx context.Context, prefix string, opts *storage.ListOptions) (*storage.ListPage, error)
	Get(ctx context.Context, key string, opts *storage.GetOptions) (*storage.Object, error)
}

// Reader reads the activity stream newest-first with object-cursor pagination.
type Reader struct {
	store                ObjectStore
	prefix               string // "<logPrefix>/activity/"
	ObjectsPerPage       int    // default 20
	MaxDecompressedBytes int64  // page-level soft cap; default 32 MiB
}

// NewReader builds a Reader over store, reading <logPrefix>/activity/ (logPrefix
// defaults to the shiplog DefaultPrefix "sys/logs").
func NewReader(store ObjectStore, logPrefix string) *Reader {
	p := logPrefix
	if p == "" {
		p = "sys/logs"
	}
	for len(p) > 0 && p[len(p)-1] == '/' {
		p = p[:len(p)-1]
	}
	return &Reader{
		store:                store,
		prefix:               p + "/activity/",
		ObjectsPerPage:       20,
		MaxDecompressedBytes: 32 << 20,
	}
}

// listKeys returns all activity object keys ascending (oldest→newest). Activity
// keys embed date+time so lexical order is chronological.
func (r *Reader) listKeys(ctx context.Context) ([]string, error) {
	var keys []string
	token := ""
	for {
		page, err := r.store.List(ctx, r.prefix, &storage.ListOptions{ContinuationToken: token})
		if err != nil {
			return nil, err
		}
		for _, o := range page.Objects {
			keys = append(keys, o.Key)
		}
		if page.NextToken == "" {
			break
		}
		token = page.NextToken
	}
	sort.Strings(keys)
	return keys, nil
}

// Page returns one page of events newest-first, applying f. cursor is the oldest
// object key consumed by the previous page ("" = start at newest). next is the
// oldest object key consumed by this page ("" when no older objects remain).
func (r *Reader) Page(ctx context.Context, f Filter, cursor string) ([]Event, string, error) {
	keys, err := r.listKeys(ctx)
	if err != nil {
		return nil, "", err
	}
	// end = exclusive upper index (newest side) for this page.
	end := len(keys)
	if cursor != "" {
		if idx := sort.SearchStrings(keys, cursor); idx < len(keys) && keys[idx] == cursor {
			end = idx // consume strictly-older keys: indices < idx
		} else {
			end = 0 // cursor not found → nothing older
		}
	}
	perPage := r.ObjectsPerPage
	if perPage <= 0 {
		perPage = 20
	}
	var events []Event
	var bytesUsed int64
	consumed := 0
	oldestIdx := end
	for i := end - 1; i >= 0 && consumed < perPage; i-- {
		obj, gerr := r.store.Get(ctx, keys[i], nil)
		if gerr != nil {
			// best-effort: skip an unreadable object, still advance the cursor
			oldestIdx = i
			consumed++
			continue
		}
		evs, _, derr := DecodeGz(obj.Body)
		obj.Body.Close()
		if derr != nil {
			oldestIdx = i
			consumed++
			continue
		}
		for _, e := range evs {
			if f.Match(e) {
				events = append(events, e)
			}
		}
		bytesUsed += obj.Metadata.Size
		oldestIdx = i
		consumed++
		if r.MaxDecompressedBytes > 0 && bytesUsed >= r.MaxDecompressedBytes {
			break // page-level soft cap; next page continues from oldestIdx
		}
	}
	next := ""
	if oldestIdx > 0 && consumed > 0 {
		next = keys[oldestIdx]
	}
	sort.Slice(events, func(a, b int) bool { return events[a].Ts.After(events[b].Ts) })
	return events, next, nil
}
```
**Note on the byte cap:** `bytesUsed` accumulates the *compressed* `Metadata.Size` as a cheap soft guard against an oversized page; the hard per-object decompressed cap is enforced inside `DecodeGz` (`maxObjectDecompressed`). This is a simplification of the spec's "MaxDecompressedBytes" (which it bounds in aggregate via per-object cap × object cap) — protective intent preserved, exact byte accounting deferred.

- [ ] **Step 3: Run + commit**

`go test ./internal/auditlog/ -v` → all PASS. `go build ./... && go vet ./internal/auditlog/`, gofmt.
```bash
git add internal/auditlog/reader.go internal/auditlog/reader_test.go
git commit -m "feat(auditlog): Reader.Page (newest-first object-cursor pagination + filtering)"
```

---

## Task 6: Web wiring — `AuditReader` interface + serve

**Files:**
- Modify: `internal/web/services.go` (interface), `internal/web/handler.go` (Deps/server), `cmd/bucketvcs/serve.go`
- Test: `internal/web/services_test.go` (assertion)

- [ ] **Step 1: Add the interface** to `internal/web/services.go`

```go
// AuditReader is the read surface the audit viewer needs (satisfied by
// *auditlog.Reader). nil disables the audit pages.
type AuditReader interface {
	Page(ctx context.Context, f auditlog.Filter, cursor string) ([]auditlog.Event, string, error)
}
```
(import `github.com/bucketvcs/bucketvcs/internal/auditlog`.)

- [ ] **Step 2: Deps/server fields** (`handler.go`)

In `Deps` (after the other admin services): `Audit AuditReader`. In `server`: `audit AuditReader`. In `NewHandler`'s `&server{…}`: `audit: d.Audit,`.

- [ ] **Step 3: Conformance assertion test** (`services_test.go`)

```go
func TestAuditReaderSatisfiedByReader(t *testing.T) {
	var _ AuditReader = (*auditlog.Reader)(nil)
}
```
(import auditlog.) Run `go test ./internal/web/ -run TestAuditReaderSatisfiedByReader -v` → must compile + PASS (the signature is the guarantee the interface matches `*auditlog.Reader`; reconcile if it doesn't).

- [ ] **Step 4: Wire serve.go**

In the `web.Deps{…}` literal (~line 909, where `store` is the in-scope `ObjectStore`), add:
```go
				Audit: func() web.AuditReader {
					if store == nil {
						return nil
					}
					return auditlog.NewReader(store, shiplog.DefaultPrefix)
				}(),
```
Import `internal/auditlog` and `internal/shiplog` in serve.go (shiplog likely already imported). If the operator configures a custom shiplog prefix, pass that instead of `shiplog.DefaultPrefix` — read how the shiplog engine is configured in serve.go and reuse the same prefix value (fall back to `DefaultPrefix`). Only add `Audit` to the `web.Deps` literal (not gateway/sshd Options).

- [ ] **Step 5: Build + commit**

`go build ./... && go test ./internal/web/ -run TestAuditReaderSatisfiedByReader -v` → PASS. vet/gofmt.
```bash
git add internal/web/services.go internal/web/handler.go internal/web/services_test.go cmd/bucketvcs/serve.go
git commit -m "feat(web): wire auditlog.Reader as AuditReader into web.Deps"
```

---

## Task 7: Global `/admin/audit`

**Files:**
- Create: `internal/web/admin_audit.go`, `internal/web/templates/admin_audit.html`
- Modify: `handler.go` (mux), `_partials.html` (adminnav), `render.go`
- Test: `internal/web/admin_audit_test.go`

- [ ] **Step 1: Failing tests**

Extend the web test fake set with a fake `AuditReader` (records the `Filter` + `cursor` it was called with; returns canned events + a next cursor). Tests:
```go
func TestAdminAudit_RendersAndFilters(t *testing.T) {
	// fakeAudit returns 1 event {Event:"policy.ref.rejected",Tenant:"acme",Repo:"app",Actor:"alice"}
	// admin GET /admin/audit?event=policy.&tenant=acme → 200, body contains "policy.ref.rejected","acme","alice"
	// assert fakeAudit received Filter{EventPrefix:"policy.",Tenant:"acme"}
}
func TestAdminAudit_NonAdmin404(t *testing.T) { /* non-admin GET /admin/audit → 404 */ }
func TestAdminAudit_NilReaderNotice(t *testing.T) { /* Audit dep nil → 200 with "not available" notice */ }
```
Run → FAIL.

- [ ] **Step 2: Implement** `admin_audit.go`

```go
package web

import (
	"net/http"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auditlog"
)

type auditRow struct {
	auditlog.Event
	TimeStr string
}

type adminAuditData struct {
	base
	Enabled    bool
	Rows       []auditRow
	NextCursor string
	// echo the active filter back into the form
	FEvent, FTenant, FRepo, FActor, FSince, FUntil string
}

func (s *server) handleAdminAudit(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		s.renderError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	d := adminAuditData{
		base:    base{Session: SessionFromContext(r.Context()), CSRF: issueCSRF(w, requestIsTLS(r, s.trustProxy)), Flash: takeFlash(w, r)},
		Enabled: s.audit != nil,
	}
	if s.audit != nil {
		f, since, until := parseAuditFilter(r) // sets d.F* echoes below
		d.FEvent, d.FTenant, d.FRepo, d.FActor, d.FSince, d.FUntil = r.URL.Query().Get("event"), r.URL.Query().Get("tenant"), r.URL.Query().Get("repo"), r.URL.Query().Get("actor"), since, until
		evs, next, err := s.audit.Page(r.Context(), f, r.URL.Query().Get("cursor"))
		if err != nil {
			s.logger.Error("admin audit: page", "err", err)
			s.renderError(w, r, http.StatusInternalServerError, "internal error")
			return
		}
		d.Rows = toAuditRows(evs)
		d.NextCursor = next
	}
	if err := s.renderBuffered(w, "admin_audit.html", d); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "render error")
		return
	}
	EmitRequestMetric(r.Context(), s.logger, "admin_audit", http.StatusOK)
}

// parseAuditFilter builds a Filter from the query string. Bad since/until are
// ignored (the echo strings are returned so the form re-shows what was typed).
func parseAuditFilter(r *http.Request) (auditlog.Filter, string, string) {
	q := r.URL.Query()
	f := auditlog.Filter{
		EventPrefix: q.Get("event"),
		Tenant:      q.Get("tenant"),
		Repo:        q.Get("repo"),
		Actor:       q.Get("actor"),
	}
	sinceStr, untilStr := q.Get("since"), q.Get("until")
	if t, err := time.Parse("2006-01-02", sinceStr); err == nil {
		f.Since = t
	}
	if t, err := time.Parse("2006-01-02", untilStr); err == nil {
		f.Until = t.Add(24 * time.Hour) // inclusive of the named day
	}
	return f, sinceStr, untilStr
}

func toAuditRows(evs []auditlog.Event) []auditRow {
	rows := make([]auditRow, 0, len(evs))
	for _, e := range evs {
		rows = append(rows, auditRow{Event: e, TimeStr: e.Ts.UTC().Format("2006-01-02 15:04:05Z")})
	}
	return rows
}
```
**Note:** `parseAuditFilter`/`toAuditRows`/`auditRow` are shared with Task 8 (per-repo) — define them here; Task 8 reuses them.

- [ ] **Step 3: mux + adminnav + template + render registration**

`handler.go`: `s.mux.HandleFunc("/admin/audit", s.handleAdminAudit)`.
`_partials.html` `adminnav`: append ` · <a href="/admin/audit">audit</a>`.
`admin_audit.html`:
```html
{{define "title"}}audit · admin · bucketvcs{{end}}
{{define "content"}}
<div class="settings">
  {{template "adminnav" .}}
  <h2>audit log</h2>
  {{if not .Enabled}}
  <p class="hint">audit log is not available (no storage configured or log shipping is off).</p>
  {{else}}
  <form method="get" action="/admin/audit" class="refswitch">
    <input type="text" name="event" value="{{.FEvent}}" placeholder="event prefix (e.g. policy.)">
    <input type="text" name="tenant" value="{{.FTenant}}" placeholder="tenant">
    <input type="text" name="repo" value="{{.FRepo}}" placeholder="repo">
    <input type="text" name="actor" value="{{.FActor}}" placeholder="actor">
    <input type="text" name="since" value="{{.FSince}}" placeholder="since YYYY-MM-DD">
    <input type="text" name="until" value="{{.FUntil}}" placeholder="until YYYY-MM-DD">
    <button type="submit">filter</button>
  </form>
  <p class="hint">shows events shipped to storage; in-flight events appear after the next ship.</p>
  {{if .Rows}}
  <table class="commits">
    <thead><tr><th>time</th><th>event</th><th>tenant/repo</th><th>actor</th></tr></thead>
    <tbody>
    {{range .Rows}}
    <tr>
      <td class="mono">{{.TimeStr}}</td>
      <td>{{.Event}}</td>
      <td class="mono">{{.Tenant}}{{if .Repo}}/{{.Repo}}{{end}}</td>
      <td>{{.Actor}}</td>
    </tr>
    {{end}}
    </tbody>
  </table>
  <div class="pager">
    {{if .NextCursor}}<a href="/admin/audit?event={{.FEvent}}&tenant={{.FTenant}}&repo={{.FRepo}}&actor={{.FActor}}&since={{.FSince}}&until={{.FUntil}}&cursor={{urlquery .NextCursor}}">older</a>{{end}}
  </div>
  {{else}}
  <p class="empty">no audit events{{if or .FEvent .FTenant .FRepo .FActor}} match the filter{{end}}.</p>
  {{end}}
  {{end}}
</div>
{{end}}
{{template "base" .}}
```
Register `admin_audit.html` in `render.go`. Confirm `urlquery` is a built-in template func (it is — `html/template` provides it); if the project wraps funcs, use the existing `urlpath`/escaping helper instead — verify and adjust.

- [ ] **Step 4: Run + commit**

`go test ./internal/web/ -run TestAdminAudit -v` → PASS; full web suite; build/vet/gofmt.
```bash
git add internal/web/admin_audit.go internal/web/templates/admin_audit.html internal/web/handler.go internal/web/templates/_partials.html internal/web/render.go internal/web/admin_audit_test.go
git commit -m "feat(web): global /admin/audit viewer (filters + object-cursor pager)"
```

---

## Task 8: Per-repo `/{t}/{r}/settings/audit`

**Files:**
- Create: `internal/web/reposettings_audit.go`
- Create: `internal/web/templates/reposettings_audit.html`
- Modify: `internal/web/reposettings.go` (tab switch), `_partials.html` (reposettingsnav), `render.go`
- Test: `internal/web/reposettings_audit_test.go`

- [ ] **Step 1: Failing tests**

```go
func TestRepoAudit_ForcesTenantRepoAndIgnoresOverride(t *testing.T) {
	// repo-admin GET /acme/app/settings/audit?tenant=other&repo=x
	// assert the AuditReader received Filter{Tenant:"acme",Repo:"app"} (NOT "other"/"x")
	// 200, renders the repo's events
}
func TestRepoAudit_NonAdmin404(t *testing.T) { /* non-repo-admin → 404 (chassis) */ }
func TestRepoAudit_NilReaderNotice(t *testing.T) { /* Audit nil → "not available" notice */ }
```
Run → FAIL (no `audit` tab).

- [ ] **Step 2: Tab dispatch** — in `reposettings.go` `handleRepoSettings` switch, add:
```go
	case "audit":
		s.repoSettingsAudit(w, r, sr)
```

- [ ] **Step 3: Implement** `reposettings_audit.go`

```go
package web

import "net/http"

type repoAuditData struct {
	base
	Tenant, Repo string
	IsAdmin      bool
	Enabled      bool
	Rows         []auditRow
	NextCursor   string
	FEvent, FActor, FSince, FUntil string
}

func (s *server) repoSettingsAudit(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if r.Method != http.MethodGet {
		s.renderError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	d := repoAuditData{
		base:    base{Session: SessionFromContext(r.Context()), CSRF: issueCSRF(w, requestIsTLS(r, s.trustProxy)), Flash: takeFlash(w, r)},
		Tenant:  sr.tenant,
		Repo:    sr.repo,
		IsAdmin: isGlobalAdmin(r),
		Enabled: s.audit != nil,
	}
	if s.audit != nil {
		f, since, until := parseAuditFilter(r) // reuse Task 7 helper
		// HARD cross-tenant boundary: force this repo, ignore any client tenant/repo.
		f.Tenant = sr.tenant
		f.Repo = sr.repo
		d.FEvent, d.FActor, d.FSince, d.FUntil = r.URL.Query().Get("event"), r.URL.Query().Get("actor"), since, until
		evs, next, err := s.audit.Page(r.Context(), f, r.URL.Query().Get("cursor"))
		if err != nil {
			s.logger.Error("repo audit: page", "tenant", sr.tenant, "repo", sr.repo, "err", err)
			s.renderError(w, r, http.StatusInternalServerError, "internal error")
			return
		}
		d.Rows = toAuditRows(evs)
		d.NextCursor = next
	}
	if err := s.renderBuffered(w, "reposettings_audit.html", d); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "render error")
		return
	}
	EmitRequestMetric(r.Context(), s.logger, "reposettings_audit", http.StatusOK)
}
```
The chassis (`handleRepoSettings`) already enforced `canAdminRepo` + repo existence before dispatch, so no extra authz here.

- [ ] **Step 4: reposettingsnav tab + template + render**

`_partials.html` `reposettingsnav`: add ` · <a href="/{{.Tenant}}/{{.Repo}}/settings/audit">audit</a>` (place after policy/triggers; before the `{{if .IsAdmin}}` hooks block, so repo-admins see it).
`reposettings_audit.html`:
```html
{{define "title"}}{{.Tenant}}/{{.Repo}} audit · bucketvcs{{end}}
{{define "content"}}
<div class="settings">
  <div class="repohdr"><span class="path"><a href="/{{.Tenant}}/{{.Repo}}">{{.Tenant}}/{{.Repo}}</a></span></div>
  {{template "reposettingsnav" .}}
  <h2>audit log</h2>
  {{if not .Enabled}}
  <p class="hint">audit log is not available (no storage configured or log shipping is off).</p>
  {{else}}
  <form method="get" action="/{{.Tenant}}/{{.Repo}}/settings/audit" class="refswitch">
    <input type="text" name="event" value="{{.FEvent}}" placeholder="event prefix">
    <input type="text" name="actor" value="{{.FActor}}" placeholder="actor">
    <input type="text" name="since" value="{{.FSince}}" placeholder="since YYYY-MM-DD">
    <input type="text" name="until" value="{{.FUntil}}" placeholder="until YYYY-MM-DD">
    <button type="submit">filter</button>
  </form>
  <p class="hint">repo-scoped events shipped to storage; user/auth events are not repo-scoped and won't appear here.</p>
  {{if .Rows}}
  <table class="commits">
    <thead><tr><th>time</th><th>event</th><th>actor</th></tr></thead>
    <tbody>
    {{range .Rows}}
    <tr><td class="mono">{{.TimeStr}}</td><td>{{.Event}}</td><td>{{.Actor}}</td></tr>
    {{end}}
    </tbody>
  </table>
  <div class="pager">
    {{if .NextCursor}}<a href="/{{.Tenant}}/{{.Repo}}/settings/audit?event={{.FEvent}}&actor={{.FActor}}&since={{.FSince}}&until={{.FUntil}}&cursor={{urlquery .NextCursor}}">older</a>{{end}}
  </div>
  {{else}}
  <p class="empty">no audit events for this repo{{if or .FEvent .FActor}} match the filter{{end}}.</p>
  {{end}}
  {{end}}
</div>
{{end}}
{{template "base" .}}
```
Register `reposettings_audit.html` in `render.go`.

- [ ] **Step 5: Run + commit**

`go test ./internal/web/ -run 'TestRepoAudit' -v` → PASS (esp. the forces-tenant/repo + ignores-override test); full web suite; build/vet/gofmt.
```bash
git add internal/web/reposettings_audit.go internal/web/templates/reposettings_audit.html internal/web/reposettings.go internal/web/templates/_partials.html internal/web/render.go internal/web/reposettings_audit_test.go
git commit -m "feat(web): per-repo audit tab (server-forced tenant+repo filter)"
```

---

## Task 9: Smoke + operator guide

**Files:**
- Create: `scripts/smoke-observability.sh`
- Modify: a web-ui operator guide under `docs/operator-guides/`

- [ ] **Step 1: Find the closest smoke + read it** (`ls scripts/ | grep -iE 'smoke|web'`; the triggers/phase-3 web smoke shows serve-bootstrap + login + CSRF helpers).

- [ ] **Step 2: Write `scripts/smoke-observability.sh`**

Against a localfs serve with `--ui` + log shipping enabled (read the serve flag for shipping; e.g. `--log-shipping` or equivalent — confirm from serve.go):
1. bootstrap admin + a repo; web login (cookie + CSRF).
2. **Sessions**: `GET /settings/sessions` → assert the current session shows `current`; create a second session (second login) → `GET /settings/sessions` shows 2; `POST /settings/sessions/revoke` the other → back to 1; `GET /admin/sessions` shows sessions across users.
3. **Audit**: trigger an audit event (e.g. a `repo public` toggle, or a push, or a policy reject) → force a shiplog flush (the serve shutdown flushes; or wait the ship interval — the smoke may stop+restart serve to force the flush, or use a short `--log-ship-interval` if available). Then `GET /admin/audit` → assert the event appears; `GET /{t}/{r}/settings/audit` → assert it appears and that an event for a different repo does NOT.
If forcing a deterministic flush is impractical in the smoke window, assert the **page renders** (200 + the "shipped to storage" banner / empty-state) and clearly `echo "SKIP: deterministic flush"` for the appears-in-list assertion — but keep sessions fully asserted.
Make it `chmod +x`, `set -euo pipefail`, trap-cleanup.

- [ ] **Step 3: Run it** — `./scripts/smoke-observability.sh`; report green/skip per step.

- [ ] **Step 4: Operator guide** — add an "Observability" section to the web-ui guide: sessions management (self-service + admin revoke); the audit viewer (global + per-repo, the shipping-lag semantics, that it requires log shipping enabled, and the per-repo tab being strictly scoped). Match the guide's product voice.

- [ ] **Step 5: Commit**

```bash
git add scripts/smoke-observability.sh docs/operator-guides/
git commit -m "docs(observability): smoke script + operator guide section"
```

---

## Final verification

- [ ] `go test ./... 2>&1 | tail -20` — all green.
- [ ] `go build ./... && go vet ./...`.
- [ ] `gofmt -l internal/auth internal/auditlog internal/web cmd/bucketvcs` — no branch-touched files flagged.
- [ ] Manual: serve with `--store` + shipping; visit `/settings/sessions`, `/admin/sessions`, `/admin/audit`, `/{t}/{r}/settings/audit`; confirm the per-repo tab ignores a hand-added `?tenant=` and works with JS disabled.

---

## Notes / conventions

- **Cross-tenant boundary** is the one security-critical seam: the per-repo handler overwrites `Filter.Tenant/Repo` *after* parsing the query, and `Reader.Page` enforces them via `Filter.Match`. The test `TestRepoAudit_ForcesTenantRepoAndIgnoresOverride` + `TestReaderPage_CrossTenantFilterExcludes` are the guards.
- **No secret exposure**: sessions render only `id_hash`; the audit viewer renders shipped attrs as-is (the secret-free-at-emission invariant from the spec; no new sink).
- **Authz**: `/admin/*` → `requireAdmin`; `/settings/*` → `requireUser` (self); `/{t}/{r}/settings/audit` → `canAdminRepo` via the chassis.
- **Nil-reader / disabled**: every audit surface degrades to a "not available" notice, never a 500.
- **Known v1 cost**: `Reader.listKeys` lists *all* activity objects per page request (O(total objects)); a date-prefix-walk (List with `Delimiter` to descend year/month/day from newest) is the scale optimization, deferred.
- **DRY**: `parseAuditFilter`/`toAuditRows`/`auditRow` are defined once (Task 7) and reused by the per-repo handler (Task 8).
