# M24 — Web UI, Phase 1: Chassis + Identity

**Status:** Design approved (2026-06-01)
**Scope:** Phase 1 of a 3-phase web UI workstream. This phase builds the web-serving
chassis and the browser-identity (session/auth) layer. It deliberately ships **no**
git-content browsing and **no** settings/admin forms — those are Phases 2 and 3.

---

## 1. Background & motivation

bucketvcs today has no human-facing UI. All interaction is via the `git` client
(HTTPS/SSH), the `bucketvcs` CLI, and HTTP Basic tokens / SSH keys. We want a
GitHub/GitLab-style web UI for tenants and their repos.

The full UI is too large for one spec, so it is decomposed into three independently
shippable phases:

- **Phase 1 (this spec):** web chassis + identity — embedded server-rendered UI,
  session/cookie auth with pluggable credential providers (local password now, OIDC
  fast-follow), login/logout, a landing page listing visible repos.
- **Phase 2:** browse (read) — repo home, file tree, commit log, single commit + diff,
  blob/raw views, using a hybrid git-content reader.
- **Phase 3:** manage (admin) — CSRF-protected settings forms wrapping the existing
  stores (public toggle, grants, tokens, SSH keys, webhooks, protected refs/paths,
  hooks, quotas, user/tenant admin).

Each phase gets its own spec → plan → implementation cycle. Phase 1 is sequenced first
because the session/provider layer is the riskiest new infrastructure and gates 2–3.

### Locked design decisions (from brainstorming)

| Decision | Choice |
|---|---|
| Rendering | Server-rendered HTML + **htmx** (no SPA; handlers return HTML) |
| Packaging | `go:embed` into the binary, `--ui-dir` disk override for dev |
| Browser auth | New cookie-session layer with **pluggable providers** |
| Providers | **Password now**; **OIDC relying-party flow as Phase 1.5 fast-follow** |
| OIDC linking | **Require a pre-provisioned local user** (match by `issuer+subject`) |
| UI listener | Shares git HTTP listener by default; **optional `--ui-addr`** own port |
| Git reads | (Phase 2) hybrid: `refstore` + cached mirror/git — *not in Phase 1* |
| Aesthetic | **Super minimalist, retro ASCII / terminal** look |

---

## 2. Architecture

### 2.1 New package: `internal/web`

A self-contained package exposing one `http.Handler`. Kept decoupled from
`internal/gateway` (no import cycle; the web layer depends on `internal/auth` and the
auth `Store`, not on the gateway).

Proposed internal structure (each file one clear purpose):

```
internal/web/
  handler.go        // NewHandler(deps) http.Handler; route table
  session/
    store.go        // SessionStore: Create/Lookup/Delete/Touch over authdb
    cookie.go       // cookie encode/decode, security flags
    middleware.go   // session + CSRF middleware; actor-in-context
  provider/
    provider.go     // Provider interface + registry
    password.go     // password provider (Phase 1)
    // oidc.go       // (Phase 1.5) authorization-code + PKCE RP flow
  page/
    login.go        // GET/POST /login, POST /logout
    landing.go      // GET /  (visible-repos list)
    errors.go       // 403 / 404 / login-required renderers
  templates/        // go:embed html/template files (base, login, landing, errors)
  static/           // go:embed minimal CSS + vendored htmx
  templates.go      // embed.FS + loader (embedded default, --ui-dir override)
```

### 2.2 Mounting & the git-vs-UI dispatcher

Git/LFS request paths **always** contain a `.git/` segment; internal endpoints start
with `/_` or are `/healthz`. The UI owns the remaining "human" paths at the root. A
small dispatcher (constructed in `cmd/bucketvcs/serve.go`) routes deterministically:

```
isGitOrInternal(path) :=
    strings.HasPrefix(path, "/_")  ||
    path == "/healthz"             ||
    strings.Contains(path, ".git/")||
    strings.HasSuffix(path, ".git")

isGitOrInternal → gateway.Server (existing)
else            → web.Handler
```

Listener wiring in `serve.go`:

- `--ui-addr` **set**: start a second `http.Server` bound to `--ui-addr` serving
  `web.Handler` directly; the main `--addr` listener serves the gateway only. (Lets the
  UI bind an internal/admin interface separate from the public git endpoint.)
- `--ui-addr` **unset** and `--ui` enabled (**default on** when HTTP is served): the
  main `--addr` listener serves the dispatcher (UI + git on one port).
- `--ui=false`: no UI; main listener serves the gateway exactly as today.

The dispatcher is a tiny `http.Handler` in `serve.go` (or a `internal/web.Dispatcher`
helper) — it does not live inside the gateway package, preserving decoupling.

### 2.3 Request flow

```
HTTP request
  → serve.go dispatcher
      ├─ git/internal → gateway.Server.ServeHTTP   (unchanged)
      └─ human path   → web.Handler
                          → session middleware (load actor from cookie, or anon)
                          → CSRF middleware (state-changing POSTs)
                          → page handler → html/template render
```

---

## 3. Sessions (new infrastructure)

### 3.1 Schema — migration `0013_web_sessions.sql`

> Migration number confirmed against the tree: latest existing is `0012_quota_bigint.sql`.

```sql
-- 0013_web_sessions.sql
CREATE TABLE sessions (
    id_hash     TEXT    NOT NULL PRIMARY KEY,  -- SHA-256 of the random 256-bit id (see §3.1)
    user_id     TEXT    NOT NULL,
    provider    TEXT    NOT NULL,              -- 'password' | 'oidc'
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    last_seen   INTEGER NOT NULL,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);
CREATE INDEX sessions_user_idx    ON sessions(user_id);
CREATE INDEX sessions_expires_idx ON sessions(expires_at);

-- password login (provider 'password'); nullable so OIDC-only users have no password
ALTER TABLE users ADD COLUMN password_hash TEXT;
```

Notes:
- **id stored hashed.** The cookie carries the raw 256-bit id; the DB stores only its
  hash, so a DB read cannot forge a session cookie. (Same posture as token secrets.)
- The hash function should match the project's existing fast lookup needs. Tokens use
  argon2id PHC for secrets; session ids are high-entropy random, so a single SHA-256
  is acceptable and far cheaper per request. **Decision for the plan:** use SHA-256 of
  the raw id for the `id_hash` lookup key (high-entropy id ⇒ no brute-force surface);
  document the rationale in code.

### 3.2 SessionStore (new, in `internal/auth/sqlitestore` + interface method)

```go
// internal/auth (interface additions)
type SessionStore interface {
    CreateSession(ctx, userID, provider string, ttl time.Duration) (rawID string, err error)
    LookupSession(ctx, rawID string) (*Session, error)   // returns nil, ErrNoSession if absent/expired
    TouchSession(ctx, rawID string, ttl time.Duration) error // sliding expiry, best-effort
    DeleteSession(ctx, rawID string) error               // logout
    SweepExpiredSessions(ctx, now time.Time) (int, error) // periodic GC
}

type Session struct {
    UserID    string
    Provider  string
    CreatedAt time.Time
    ExpiresAt time.Time
}
```

A background sweeper (one goroutine in `serve.go`, ~ every 10 min) calls
`SweepExpiredSessions` to bound table growth. Nil/disabled UI ⇒ no sweeper.

### 3.3 Cookie & CSRF

- Cookie name `bvcs_session`; value = raw session id. Attributes: `HttpOnly`,
  `SameSite=Lax`, `Path=/`, `Secure` when the request arrived over TLS (honor
  `--trust-proxy-headers` / `X-Forwarded-Proto` consistent with M18).
- TTL `--ui-session-ttl` (default `168h` = 7 days), **sliding**: `TouchSession`
  extends `expires_at` on activity (rate-limited to ≤ 1 write/min/session to avoid
  write amplification).
- **CSRF:** per-session token (random, stored in session-scoped signed cookie or
  rendered hidden field). All state-changing POSTs (`/login`, `/logout`; Phase 3 forms)
  require a matching token. GET is never state-changing. Reject mismatches with `403`.

### 3.4 Login rate-limiting

Reuse the **M18 `auth.Limiter`** (per-IP token bucket) for failed password logins.
`web_login` failures count via the shared `auth.IsCredentialError` set. Success resets
the bucket. Nil limiter (disabled) ⇒ no-op, same as M18.

---

## 4. Credential providers (pluggable)

```go
// internal/web/provider
type Provider interface {
    Name() string // "password" | "oidc"
    // Authenticate validates the request's credentials and returns the local Actor
    // to bind a session to, or an error (auth.IsCredentialError-classified on bad creds).
    Authenticate(ctx context.Context, r *http.Request) (*auth.Actor, error)
}
```

The login page posts to a single handler that dispatches to the configured provider(s).
The session layer is provider-agnostic: it only ever stores `(user_id, provider)`.

### 4.1 Password provider (Phase 1 — full)

- New nullable `users.password_hash` (migration 0013). **argon2id PHC**, reusing the
  existing token hasher in `internal/auth` (single source of truth for params).
- New store methods:
  ```go
  SetPassword(ctx, userName, plaintext string) error      // hashes + stores
  VerifyPassword(ctx, userName, plaintext string) (*auth.Actor, error) // ErrInvalidCredential on mismatch / no hash / disabled user
  ```
- New CLI: `bucketvcs user set-password <user> --password-stdin` (reads the plaintext
  from stdin; never via argv). Also supports clearing via a future flag (out of scope).
- Users with `password_hash = NULL` cannot password-login (they must use OIDC once
  Phase 1.5 lands). This is not an error state — just "no password set".

### 4.2 OIDC provider (Phase 1.5 — fast-follow, NOT built in Phase 1)

Documented here so Phase 1's interfaces accommodate it with zero rework:

- Full OAuth2 **authorization-code + PKCE** relying-party flow: `/login/oidc` →
  redirect to IdP → `/login/oidc/callback` → validate id_token → map identity →
  create session.
- **Account linking: require a pre-provisioned local user.** Login succeeds only if a
  local user already exists matching the IdP `issuer + subject` (email as a fallback
  match is a Phase 1.5 design choice). No auto-provisioning.
- Schema reserved for Phase 1.5 (a `user_identities(user_id, issuer, subject, email)`
  table) — **not created in Phase 1.** Phase 1 must not bake assumptions that block it.
- Note: the existing M22 OIDC infrastructure is **token-exchange / inbound verification**,
  not a browser RP flow. Phase 1.5 builds the RP flow fresh; it may reuse M22's JWKS/
  token-validation helpers where applicable.

---

## 5. Pages (Phase 1 deliverable)

| Route | Method | Behavior |
|---|---|---|
| `/login` | GET | Render login form (username + password), CSRF token embedded |
| `/login` | POST | Verify via password provider → create session → set cookie → 303 redirect to `/` (or `?next=`) |
| `/logout` | POST | Delete session row, clear cookie, redirect to `/` |
| `/` | GET | Landing: list repos visible to the current actor |
| `/_ui/static/...` | GET | Embedded CSS / vendored htmx (long cache headers) |
| (unmatched) | — | `404` page |

- **Landing visibility:** anonymous → public-read repos only; authenticated → public +
  repos the actor has any grant on. Requires a read method:
  ```go
  ListAccessibleRepos(ctx, actor *auth.Actor) ([]*Repo, error)
  ```
  Implemented in `sqlitestore` as: all `public_read=1` repos UNION repos in
  `repo_permissions` for `actor.UserID` (admins see all). Grouped by tenant in the view.
- Navbar shows the logged-in user name + a logout control, or a "log in" link when anon.
- `?next=` redirect target is validated to be a local path (no open redirect).

**Explicitly NOT in Phase 1:** repo home, file tree, commit log, blob/diff views (all
Phase 2); any settings/admin form (Phase 3); the OIDC flow (Phase 1.5).

---

## 6. Aesthetic — retro ASCII / minimalist

- `html/template` with a small base layout; a single embedded CSS file: system
  monospace stack, box-drawing borders (`┌ ─ ┐ │ └ ┘ ├ ┤`), near-zero color (one accent
  max), generous whitespace, no images/icons beyond ASCII glyphs.
- htmx **vendored locally** under `static/` (no CDN) for progressive enhancement
  (e.g., logout without full reload). Pages must work without JS (htmx is enhancement,
  not a requirement).
- `--ui-dir <path>` overrides the embedded `templates/` + `static/` from disk and
  **disables template caching** (parse-per-request) so designers can hot-iterate.
- The detailed visual treatment is produced during implementation via the
  `frontend-design` skill; this spec fixes the direction and the layout grammar.

Reference landing sketch:

```
┌─ bucketvcs ─────────────────────────────────[ alice ▾ ]─┐
│                                                          │
│  acme/                                                   │
│    ├─ demo            public    updated 2h ago           │
│    ├─ infra           private   updated 1d ago           │
│    └─ www             public    updated 3d ago           │
│                                                          │
└──────────────────────────────────────────────────────────┘
```

---

## 7. Configuration (new serve flags)

| Flag | Default | Purpose |
|---|---|---|
| `--ui` | `true` (when HTTP served) | Enable/disable the web UI |
| `--ui-addr` | empty | Optional separate listener for the UI; empty ⇒ share `--addr` |
| `--ui-dir` | empty | Serve templates/static from disk (dev) instead of embedded |
| `--ui-session-ttl` | `168h` | Sliding session lifetime |

Existing `--trust-proxy-headers` (M18) governs `Secure`-cookie / client-IP decisions.

---

## 8. Observability

- **Metrics:** `web_requests_total{route,status}`, `web_login_total{result}` (result ∈
  `success|invalid|ratelimited`), `web_sessions_active` (gauge, optional/sampled).
- **Audit events:** `auth.session.created`, `auth.session.destroyed`,
  `auth.password.set` (emitted by the CLI/store), reusing the existing audit emitter
  and `auth.*` namespace.

---

## 9. Testing

- Session lifecycle: create → lookup → touch (sliding) → expire → sweep → delete.
- Cookie security: `HttpOnly`, `SameSite=Lax`, `Secure` set under TLS / proxy header.
- CSRF: POST without/with bad token → 403; valid token → success.
- Password: `SetPassword`/`VerifyPassword` round-trip; wrong password and NULL-hash and
  disabled-user all classified as `ErrInvalidCredential`.
- Dispatcher: representative git paths (`/t/r.git/info/refs`, `/t/r.git/git-upload-pack`,
  LFS `/t/r.git/info/lfs/...`), internals (`/_lfs/`, `/healthz`) → gateway; human paths
  (`/`, `/login`, `/acme`) → web.
- Landing visibility: anon sees only public; authed sees public + granted; admin sees all.
- Login rate-limit: N failures → `429`/rejection via M18 limiter; success resets.
- `--ui-dir` override loads from disk and reflects edits without restart.
- Open-redirect guard on `?next=`.

---

## 10. Out of scope (deferred)

- **Phase 2:** all git-content browsing (tree/log/commit/diff/blob/raw), README render,
  branch/tag switching, the hybrid reader.
- **Phase 3:** all settings/admin forms and the read/write store wiring behind them.
- **Phase 1.5:** the OIDC relying-party flow + `user_identities` schema.
- Password reset / email flows, account self-registration, MFA, remember-me beyond the
  session TTL, themeing beyond the single retro CSS, i18n.

---

## 11. Risks & mitigations

| Risk | Mitigation |
|---|---|
| Dispatcher misroutes a git client to the UI | Deterministic `.git/`-segment rule + explicit tests over real git/LFS paths; git paths are structurally unambiguous |
| Session-cookie theft / forgery | Store id hashed; `HttpOnly`+`Secure`+`SameSite=Lax`; short-ish sliding TTL; logout deletes row |
| CSRF on POST | Per-session token on all state-changing requests |
| Login brute force | Reuse M18 per-IP limiter; argon2id verify cost |
| OIDC rework risk | Provider interface + `(user_id, provider)` session shape designed now; `user_identities` reserved |
| Import cycle web↔gateway | Web depends only on `auth`/stores; dispatcher lives in `serve.go` |
| Migration numbering drift (memory said 0010) | Verified against tree: next is **0013** |
