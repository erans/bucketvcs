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
| Code browse / repo settings / admin screens | ❌ deferred (Phase 2+) | |
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

Phase 1 ships identity and a repository landing page only:

- `GET /login` — login form (HTML).
- `POST /login` — credential check + session cookie.
- `POST /logout` — session teardown.
- `GET /` — landing page listing all repos the current visitor can see.
- `GET /_ui/static/*` — embedded CSS/JS/font assets.

Phase 2 will add code browse, diff, and blob views. The admin screen (user/repo
management) is a planned Phase 3 item.

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

## 6. Observability

### 6.1 Metrics

| Metric | Labels | Description |
|---|---|---|
| `web_requests_total` | `route`, `status` | Request count by UI route and HTTP status |
| `web_login_total` | `result` | Login outcomes: `success`, `invalid`, `ratelimited` |
| `web_sessions_active` | — | Count of non-expired sessions |

### 6.2 Audit events

| Event | When |
|---|---|
| `auth.session.created` | Session cookie issued after successful password check |
| `auth.session.destroyed` | Session deleted via `/logout` |
| `auth.password.set` | Password hash updated via `user set-password` |

---

## 7. Deferred work and planned phases

- **Phase 1.5 — OIDC browser login**: allow users to authenticate via an OIDC
  provider (GitHub, Google, etc.) in addition to username + password.
- **Phase 2 — code browse**: tree, blob, and diff views for git repositories.
- **Phase 3 — settings / admin screens**: manage users, repos, protected-ref
  policies, webhooks, and token scopes through the UI rather than the CLI.
- **Per-session audit trail**: expose session list and per-user login history to
  admins.
- **Rotate-password UI**: self-service password change from within the browser.
