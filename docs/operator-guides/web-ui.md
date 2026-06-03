# M24 — Web UI (operator guide)

This guide covers the M24 Phase 1 web UI feature. It explains what ships, how
to configure the embedded HTTP server, how to create browser-login accounts,
the session and CSRF security model, the repo-visibility rules, and the planned
follow-on phases.

---

## Production readiness

| Concern | Status | Notes |
|---|---|---|
| Login / logout (username + password) | ✅ shipped | `bucketvcs user set-password` |
| Landing page — public + granted repos | ✅ shipped | grouped by tenant |
| Session management (sqlite-backed) | ✅ shipped | sliding TTL, periodic sweep |
| CSRF double-submit protection | ✅ shipped | all POST handlers |
| Per-IP rate-limiting on login failures | ✅ shipped | shares M18 rate limiter |
| Code browse (tree, blob, diff, log) | ✅ shipped | Phase 2; see §6 |
| Repo settings / admin screens | ❌ deferred (Phase 3) | |
| OIDC browser login | ❌ deferred (Phase 1.5) | |
| Per-session audit trail | ❌ deferred | |

Schema migration `0013_sessions.sql` is forward-only and applied by the existing
`RunMigrations` on first startup.

---

## 1. Overview

M24 mounts a human-readable web UI on the same HTTP listener as the git gateway
(or on a separate listener via `--ui-addr`). A built-in dispatcher inspects each
request path and routes it: paths ending in `.git` or containing `.git/`,
`/healthz`, or `/_` internal prefixes go to the git handler; everything else goes
to the UI handler.

Phase 1 ships identity and a repository landing page:

- `GET /login` — login form (HTML).
- `POST /login` — credential check + session cookie.
- `POST /logout` — session teardown.
- `GET /` — landing page listing all repos the current visitor can see.
- `GET /_ui/static/*` — embedded CSS/JS/font assets.

Phase 2 ships read-only git code browsing (see §6):

- `GET /{tenant}/{repo}` — repository home: default-branch root tree + rendered README.
- `GET /{tenant}/{repo}/tree/{ref}/{path}` — directory listing.
- `GET /{tenant}/{repo}/blob/{ref}/{path}` — file view with syntax highlighting.
- `GET /{tenant}/{repo}/raw/{ref}/{path}` — raw file bytes (safely served).
- `GET /{tenant}/{repo}/commits/{ref}` — paginated commit log.
- `GET /{tenant}/{repo}/commit/{oid}` — single commit with diff.

The admin screen (user/repo management) is a planned Phase 3 item.

---

## 2. Enabling the UI

The web UI is enabled by default whenever `--addr` is set:

```
bucketvcs serve \
    --addr 0.0.0.0:8080 \
    --store localfs:/var/lib/bucketvcs \
    --auth-db /var/lib/bucketvcs/auth.db \
    --mirror-dir /var/lib/bucketvcs/mirror \
    --lfs=false
```

### 2.1 Flag reference

| Flag | Default | Description |
|---|---|---|
| `--ui` | `true` | Enable the web UI. Set `--ui=false` to disable (git gateway only). |
| `--ui-addr` | `""` (shares `--addr`) | Bind the web UI on a separate listen address. Requires `--addr` to also be set — a startup `WARN` is emitted if `--ui-addr` is set but `--addr` is not; the UI will not be served in that configuration. |
| `--ui-dir` | `""` (embedded assets) | Serve HTML templates and static files from this directory instead of the compiled-in assets. Use only during development. |
| `--ui-session-ttl` | `168h` (7 days) | Session cookie lifetime. The TTL is sliding: each authenticated request refreshes the expiry. Sessions are swept from the database every 10 minutes. |

### 2.2 Separate listener

To expose the UI on a different address or port from the git gateway:

```
bucketvcs serve \
    --addr 0.0.0.0:8080 \
    --ui-addr 0.0.0.0:8443 \
    ...
```

Both listeners must be set. The git gateway is served on `--addr`; the UI
handler is served on `--ui-addr`. If only `--ui-addr` is set, the gateway warns
at startup and skips mounting the UI.

---

## 3. Creating a browser-login account

Browser login uses the same user records as git HTTPS authentication. An
operator creates a user, then sets a password:

```sh
# Add the user (generates a random token, which is not needed for browser login).
bucketvcs user add alice --auth-db /var/lib/bucketvcs/auth.db

# Set a password (reads from stdin; do not pass on the command line).
echo "correct-horse-battery-staple" | \
    bucketvcs user set-password alice \
        --auth-db /var/lib/bucketvcs/auth.db \
        --password-stdin
```

`--password-stdin` is required. The CLI refuses to accept a password on the
command line to avoid it appearing in shell history. An empty password is also
rejected.

To change a password, run `user set-password` again; the new bcrypt hash
replaces the old one atomically.

---

## 4. Session security

### 4.1 Cookie attributes

The session cookie is named `bvcs_session` and is set with:

- `HttpOnly` — not accessible from JavaScript.
- `SameSite=Lax` — sent on top-level navigations but not cross-site sub-requests.
- `Secure` — set automatically when the request arrives over TLS or when
  `--trust-proxy-headers=true` is set and the `X-Forwarded-Proto: https` header
  is present (trusted-proxy mode, see §4.3).

### 4.2 CSRF protection

Every POST handler enforces a double-submit CSRF check. The login page embeds a
`csrf_token` hidden form field whose value matches a `bvcs_csrf` cookie set on
the GET. The POST handler rejects requests where the two values differ (constant-
time comparison). Requests with no CSRF cookie receive HTTP 403.

### 4.3 Reverse proxy and TLS offload

When running behind a reverse proxy (NGINX, Caddy, a cloud load balancer):

```
bucketvcs serve \
    --addr 127.0.0.1:8080 \
    --trust-proxy-headers=true \
    ...
```

With `--trust-proxy-headers=true` the gateway reads the client IP from the
rightmost value of the `X-Forwarded-For` header (standard appending-proxy
convention) and treats `X-Forwarded-Proto: https` as authoritative for the
`Secure` cookie flag.

A startup `WARN` is emitted when the M18 rate limiter is enabled without
`--trust-proxy-headers` because every request would appear to come from the
proxy IP.

### 4.4 Login rate limiting

Login failures (wrong password, unknown user) are counted by the M18 per-IP
token-bucket rate limiter (default burst 10, refill 1 per minute). When the
bucket is exhausted the server returns HTTP 429 with a `Retry-After` header.
A successful login resets the bucket. The UI login path shares the same limiter
as HTTPS git and LFS operations.

---

## 5. Repository visibility

The landing page groups repositories by tenant. Visibility rules:

| Visitor | Sees |
|---|---|
| Anonymous (no session) | Repos marked `public_read = true` only |
| Logged-in user | Public repos + repos where the user holds any permission (read, write, or admin) |
| Admin | All repos across all tenants |

To mark a repo public so anonymous visitors see it on the landing page:

```sh
bucketvcs repo public acme/site on --auth-db /var/lib/bucketvcs/auth.db
```

To revoke public access:

```sh
bucketvcs repo public acme/site off --auth-db /var/lib/bucketvcs/auth.db
```

---

## OIDC browser login (Phase 1.5)

Browser login can additionally delegate authentication to an external OIDC
identity provider (Okta, Google Workspace, Microsoft Entra, Auth0, etc.). When
enabled, the login page shows a single-sign-on button alongside the username +
password form. The flow is Authorization Code + PKCE with nonce binding; the
returned ID token's RS256/ES256 signature is verified against the provider's
JWKS (fetched via OIDC discovery), and users are matched to local accounts by
**verified email** on first login.

OIDC login requires the UI to be enabled (`--ui`, default on) and an HTTP
listener (`--addr` or `--ui-addr`). It does not change git HTTPS/SSH auth.

### Enabling OIDC

| Flag | Description |
|---|---|
| `--oidc-login` | Enable OIDC browser login. Off by default. |
| `--oidc-login-issuer` | Issuer URL, e.g. `https://accounts.example.com`. Discovery fetches `<issuer>/.well-known/openid-configuration`. Must be `https` except for loopback hosts (`localhost`, `127.0.0.1`, `::1`) used in local testing. |
| `--oidc-login-client-id` | OAuth client ID registered with the provider. |
| `--oidc-login-client-secret-file` | Path to a file containing the client secret. Alternatively set the secret in the `BUCKETVCS_OIDC_LOGIN_CLIENT_SECRET` environment variable (keeps it out of `ps` / shell history). |
| `--oidc-login-redirect-url` | The public callback URL. Must be `https://<host>/login/oidc/callback` and registered verbatim as an allowed redirect URI with the provider. |
| `--oidc-login-scopes` | Space- or comma-separated scopes. Defaults to `openid email`. `openid` and `email` are required for verified-email matching. |
| `--oidc-login-label` | Button label shown on the login page, e.g. `Sign in with Okta`. |

```
bucketvcs serve \
    --addr 0.0.0.0:8080 \
    --oidc-login \
    --oidc-login-issuer https://accounts.example.com \
    --oidc-login-client-id bucketvcs-web \
    --oidc-login-client-secret-file /run/secrets/oidc_client_secret \
    --oidc-login-redirect-url https://git.example.com/login/oidc/callback \
    --oidc-login-scopes "openid email" \
    --oidc-login-label "Sign in with SSO" \
    ...
```

The redirect URL path is fixed: register `https://<host>/login/oidc/callback`
with the IdP. A mismatch causes the IdP to refuse the authorization request.

### Verified-email TOFU (trust on first use)

There is **no auto-provisioning**. Operators pre-create accounts and set a
verified email; the first OIDC login then matches by that email and pins the
`(issuer, subject)` pair to the account:

```sh
# Pre-create the user with a verified email (TOFU match key).
bucketvcs user add alice --email alice@corp.com --auth-db /var/lib/bucketvcs/auth.db

# Or update an existing user's email.
bucketvcs user set-email alice alice@corp.com --auth-db /var/lib/bucketvcs/auth.db
```

On the first successful login, the verified `email` claim is matched to a local
user and the identity `(issuer, subject)` is linked. Subsequent logins resolve
directly by the pinned `(issuer, subject)` and ignore email changes at the IdP.
A login is **rejected** (no session, no account created) when:

- the email claim is absent or `email_verified` is not `true`;
- no local user has that verified email;
- the matched user is disabled;
- the token fails signature, `iss`, `exp`, `aud`, or `nonce` validation.

All rejections return a uniform error page so the wire never reveals which gate
failed.

### Inspecting and revoking pinned identities

```sh
# List the (issuer, subject) identities pinned to a user (NDJSON).
bucketvcs user identity list alice --auth-db /var/lib/bucketvcs/auth.db

# Unpin an identity (e.g. after an account is re-keyed at the IdP).
bucketvcs user identity remove https://accounts.example.com sub-12345 \
    --auth-db /var/lib/bucketvcs/auth.db
```

After removal, the next OIDC login for that subject falls back to verified-email
TOFU and re-pins.

### OIDC audit events

| Event | When |
|---|---|
| `auth.oidc.login` | OIDC login succeeded; session issued |
| `auth.oidc.identity_linked` | A verified email was matched and `(issuer, subject)` pinned to a user (first login) |
| `auth.oidc.rejected` | OIDC login rejected; carries a `reason` attribute (`state_mismatch`, `idp_error`, `token_invalid`, `email_unverified`, `no_user`, `disabled`, `server_error`) |

Login outcomes also increment `web_login_total` with `provider=oidc`.

### Deferred

- Multiple simultaneous IdPs (one issuer per process today).
- Auto-provisioning of unknown users (operator pre-creation is required).
- RP-initiated logout / IdP session termination.

---

## 6. Code browse (Phase 2)

Phase 2 adds read-only git code browsing. All browse routes share the same
visibility rules as the landing page (see §5): anonymous visitors see only
public repositories; logged-in users see their granted repos; admins see all.
Both not-found and not-authorized conditions return a uniform HTTP 404 to
prevent repository enumeration.

### 6.1 Routes

| Route | Description |
|---|---|
| `GET /{tenant}/{repo}` | Repository home: root directory tree of the default branch + rendered README |
| `GET /{tenant}/{repo}/tree/{ref}/{path}` | Directory listing at `path` on `ref` |
| `GET /{tenant}/{repo}/blob/{ref}/{path}` | File view with syntax highlighting |
| `GET /{tenant}/{repo}/raw/{ref}/{path}` | Raw file bytes (see §6.5 for safety headers) |
| `GET /{tenant}/{repo}/commits/{ref}` | Paginated commit log (50 commits per page, `?page=N`) |
| `GET /{tenant}/{repo}/commit/{oid}` | Single commit: metadata, message, and unified diff |

`{ref}` accepts a branch name, tag name, or 40-hex commit OID. The resolver
prefers the longest matching branch/tag prefix so that refs containing slashes
(e.g. `feature/foo`) are resolved correctly. When a branch and a tag share the
same name, the branch wins; use the tag's commit OID to browse the tag.

### 6.2 Branch and tag switcher

Each browse page shows a branch/tag dropdown populated from all known refs. The
switcher uses plain links (full-page navigation); htmx partial swaps are a
deferred item.

### 6.3 README rendering

The repository home page automatically renders a `README.md` or
`README.markdown` (case-insensitive) found at the root of the default-branch
tree. Rendering is a two-step pipeline:

1. **goldmark** converts Markdown to HTML.
2. **bluemonday** (UGC policy) sanitizes the HTML — scripts, event handlers,
   and unsafe markup are stripped before the result is embedded in the page.

If no README file is present, the root tree is shown without a rendered preamble.
README files that are binary or exceed the 10 MiB blob limit are silently
skipped.

### 6.4 Syntax highlighting and blob caps

| Condition | Behaviour |
|---|---|
| Text blob ≤ 1 MiB | Syntax-highlighted via **chroma** (inline styles, "bw" theme) |
| Text blob > 1 MiB | Plain escaped `<pre>` (no highlighting) |
| Binary blob (NUL byte in first 8 KiB) | Message + download link; no source rendered |
| Any blob > 10 MiB | Message + download link; bytes not fetched from the mirror |

Chroma selects a lexer by filename; if that fails it falls back to content
analysis, then to a plain-text lexer. Inline styles (`WithClasses(false)`) keep
the output self-contained — no separate CSS file is required.

### 6.5 Raw endpoint safety headers

The `/{tenant}/{repo}/raw/{ref}/{path}` endpoint serves file bytes directly.
Because repo content is attacker-controlled, every response is hardened:

| Header | Value |
|---|---|
| `X-Content-Type-Options` | `nosniff` |
| `Content-Security-Policy` | `default-src 'none'; sandbox` |
| `Content-Type` (text) | `text/plain; charset=utf-8` |
| `Content-Type` (binary or >10 MiB) | `application/octet-stream` |
| `Content-Disposition` (binary or >10 MiB) | `attachment; filename*=UTF-8''<RFC 5987 encoded name>` |

These headers together ensure that HTML, SVG, and other active content cannot
execute inline under the UI's origin, even when a browser ignores
`Content-Security-Policy`.

### 6.6 Diff caps

Commit diffs are capped to prevent runaway page rendering:

- **300 files per commit** — additional files are silently omitted and a
  truncation notice is displayed.
- **3 000 changed (added/removed) lines per file** — files exceeding this limit
  show a "too large" notice in place of the diff hunks. Context lines (unchanged
  lines shown for surrounding context) are not counted toward this cap.

### 6.7 Hybrid reader and cold-mirror warming

The browse backend uses a hybrid reading strategy:

- **Refs** (branches, tags, default branch) are resolved directly from the
  object-store manifest — no mirror access required.
- **Tree listings, blob content, commit log, and diffs** are served from the
  shared on-disk git mirror, the same warm cache used by the git gateway for
  clone and fetch operations.

For repositories that have no local mirror yet ("cold"), the first browse
request materializes the mirror synchronously. This cold materialization is the
only operation bounded by `--ui-browse-timeout` (default `20s`). If the mirror
is not ready within the timeout, the server returns HTTP 503 with the message
`repository is warming up — please retry shortly`. The operation is then logged
at `WARN` level for operator visibility.

Note: after cold materialization completes, a browse read can additionally wait
for an in-flight push (or maintenance run) to the same repository to complete
before it can acquire the per-repo read lock. This write-lock wait is **not**
covered by `--ui-browse-timeout` — it is identical to the behavior of a `git
fetch` on the gateway, and is expected to be brief in practice.

#### New serve flag

| Flag | Default | Description |
|---|---|---|
| `--ui-browse-timeout` | `20s` | Maximum wait for **cold mirror materialization** on a browse request. Requests that exceed this deadline receive HTTP 503. Does not cover the subsequent read-lock acquisition or git reads. |

### 6.8 Observability

Browse requests emit two new metrics:

| Metric | Labels | Description |
|---|---|---|
| `web_browse_total` | `view` | Browse requests by view, counted after authorization (includes reads that subsequently fail with 404/503; per-outcome counts are in web_requests_total); `view` ∈ `repo`, `tree`, `blob`, `raw`, `commits`, `commit` |
| `web_browse_mirror_wait_seconds` | — | Time spent opening (and possibly materializing) the git mirror; emitted once per git read operation (a single page may perform several, e.g. repo home = tree + README), not once per request |

No new audit events are emitted for Phase 2. Read operations are not audited.

### 6.9 Known limitations (deferred)

- Path-filtered commit log (`git log -- <path>`).
- Blame view.
- File and commit search.
- Compare / branch-diff views.
- Cursor-based pagination (current log pagination is offset-based).
- Per-read audit events.
- Web clone / zip download.
- htmx partial swaps for the ref switcher (currently full-page navigation).
- Branch and tag management through the UI.
- A `Content-Security-Policy` on the rendered HTML browse pages. The raw
  endpoint carries a strict CSP, but HTML pages rely on bluemonday's
  sanitization (scripts and event handlers are stripped). A rendered README
  may still reference remote images, which can disclose a viewer's IP to the
  image host; a UI-wide CSP / image proxy is deferred.
- Git errors during a read surface as HTTP 404 when the object, ref, or path does
  not exist (missing-ref/missing-path/missing-object checks return ErrNotFound →
  404); an unexpected git failure that occurs after the object's existence is
  confirmed (e.g. cat-file content read, diff generation) surfaces as HTTP 500.

---

## 7. Observability

### 7.1 Metrics

| Metric | Labels | Description |
|---|---|---|
| `web_requests_total` | `route`, `status` | Request count by UI route and HTTP status |
| `web_login_total` | `result` | Login outcomes: `success`, `invalid`, `ratelimited` |
| `web_sessions_active` | — | Count of non-expired sessions |
| `web_browse_total` | `view` | Browse requests by view, counted after authorization (includes reads that subsequently fail with 404/503; per-outcome counts are in web_requests_total); `view` ∈ `repo`, `tree`, `blob`, `raw`, `commits`, `commit` (Phase 2) |
| `web_browse_mirror_wait_seconds` | — | Mirror open/materialize latency; emitted once per git read operation (a single page may perform several, e.g. repo home = tree + README), not once per request (Phase 2) |

### 7.2 Audit events

| Event | When |
|---|---|
| `auth.session.created` | Session cookie issued after successful password check |
| `auth.session.destroyed` | Session deleted via `/logout` |
| `auth.password.set` | Password hash updated via `user set-password` |

---

## 8. Deferred work and planned phases

- **Phase 1.5 — OIDC browser login**: shipped — see "OIDC browser login
  (Phase 1.5)" above. Remaining OIDC follow-ups (multiple IdPs, auto-provisioning,
  RP-initiated logout) are listed in that section's "Deferred" note.
- **Phase 2 — code browse**: shipped — see §6 above. Remaining Phase 2 deferrals
  (path-filtered log, blame, search, compare views, cursor pagination, per-read
  audit, web clone/zip, htmx partial swaps, branch/tag management) are listed in
  §6.9.
- **Phase 3 — settings / admin screens**: manage users, repos, protected-ref
  policies, webhooks, and token scopes through the UI rather than the CLI.
- **Per-session audit trail**: expose session list and per-user login history to
  admins.
- **Rotate-password UI**: self-service password change from within the browser.
