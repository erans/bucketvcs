# Web UI (operator guide)

This guide covers the bucketvcs web UI. It explains what the UI serves, how to
configure the embedded HTTP server, how to create browser-login accounts, the
session and CSRF security model, OIDC browser login, read-only code browsing,
the settings and admin screens, and the repo-visibility rules.

---

## Production readiness

| Concern | Status | Notes |
|---|---|---|
| Login / logout (username + password) | ✅ shipped | `bucketvcs user set-password` |
| Landing page — public + granted repos | ✅ shipped | grouped by tenant |
| Session management (sqlite-backed) | ✅ shipped | sliding TTL, periodic sweep |
| CSRF double-submit protection | ✅ shipped | all POST handlers |
| Per-IP rate-limiting on login failures | ✅ shipped | shares the login rate limiter |
| OIDC browser login | ✅ shipped | see §6 |
| Code browse (tree, blob, diff, log) | ✅ shipped | see §7 |
| Repo settings / admin screens | ✅ shipped | see §8 |
| Session management UI (list / revoke) | ✅ shipped | see §10 |
| Audit-log viewer (global + per-repo) | ✅ shipped | see §10 |

Schema migration `0013_sessions.sql` is forward-only and applied by the existing
`RunMigrations` on first startup.

---

## 1. Overview

The web UI mounts a human-readable interface on the same HTTP listener as the git
gateway (or on a separate listener via `--ui-addr`). A built-in dispatcher
inspects each request path and routes it: paths ending in `.git` or containing
`.git/`, `/healthz`, or `/_` internal prefixes go to the git handler; everything
else goes to the UI handler.

The UI serves three groups of routes.

**Identity and landing:**

- `GET /login` — login form (HTML).
- `POST /login` — credential check + session cookie.
- `GET /login/oidc/start`, `GET /login/oidc/callback` — OIDC browser login (see §6).
- `POST /logout` — session teardown.
- `GET /` — landing page listing all repos the current visitor can see.
- `GET /_ui/static/*` — embedded CSS/JS/font assets.

**Read-only code browse** (see §7):

- `GET /{tenant}/{repo}` — repository home: default-branch root tree + rendered README.
- `GET /{tenant}/{repo}/tree/{ref}/{path}` — directory listing.
- `GET /{tenant}/{repo}/blob/{ref}/{path}` — file view with syntax highlighting.
- `GET /{tenant}/{repo}/raw/{ref}/{path}` — raw file bytes (safely served).
- `GET /{tenant}/{repo}/commits/{ref}` — paginated commit log.
- `GET /{tenant}/{repo}/commit/{oid}` — single commit with diff.

**Settings and admin** (see §8):

- `/settings`, `/settings/tokens`, `/settings/keys` — self-service.
- `/{tenant}/{repo}/settings/*` — per-repo settings (repo-admin or global admin).
- `/admin/*` — instance administration (global admin only).

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
| `--ui-dir` | `""` (embedded assets) | Serve HTML templates and static files from this directory instead of the compiled-in assets. Use only during development. Custom template directories must supply all page templates **and** `_partials.html` (the shared ref-switcher and tree-row fragment templates); without it, any tree or repo-home render will fail. |
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

A startup `WARN` is emitted when the login rate limiter is enabled without
`--trust-proxy-headers` because every request would appear to come from the
proxy IP.

### 4.4 Login rate limiting

Login failures (wrong password, unknown user) are counted by the shared per-IP
token-bucket login rate limiter (default burst 10, refill 1 per minute). When the
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

## 6. OIDC browser login

Browser login can additionally delegate authentication to an external OIDC
identity provider (Okta, Google Workspace, Microsoft Entra, Auth0, etc.). When
enabled, the login page shows a single-sign-on button alongside the username +
password form. The flow is Authorization Code + PKCE with nonce binding; the
returned ID token's RS256/ES256 signature is verified against the provider's
JWKS (fetched via OIDC discovery), and users are matched to local accounts by
**verified email** on first login.

OIDC login requires the UI to be enabled (`--ui`, default on) and an HTTP
listener (`--addr` or `--ui-addr`). It does not change git HTTPS/SSH auth.

### 6.1 Enabling OIDC

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

### 6.2 Verified-email TOFU (trust on first use)

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

### 6.3 Inspecting and revoking pinned identities

```sh
# List the (issuer, subject) identities pinned to a user (NDJSON).
bucketvcs user identity list alice --auth-db /var/lib/bucketvcs/auth.db

# Unpin an identity (e.g. after an account is re-keyed at the IdP).
bucketvcs user identity remove https://accounts.example.com sub-12345 \
    --auth-db /var/lib/bucketvcs/auth.db
```

After removal, the next OIDC login for that subject falls back to verified-email
TOFU and re-pins.

### 6.4 OIDC audit events

| Event | When |
|---|---|
| `auth.oidc.login` | OIDC login succeeded; session issued |
| `auth.oidc.identity_linked` | A verified email was matched and `(issuer, subject)` pinned to a user (first login) |
| `auth.oidc.rejected` | OIDC login rejected; carries a `reason` attribute (`state_mismatch`, `idp_error`, `token_invalid`, `email_unverified`, `no_user`, `disabled`, `server_error`) |

Login outcomes also increment `web_login_total` with `provider=oidc`.

---

## 7. Code browse

Read-only git code browsing. All browse routes share the same visibility rules as
the landing page (see §5): anonymous visitors see only public repositories;
logged-in users see their granted repos; admins see all. Both not-found and
not-authorized conditions return a uniform HTTP 404 to prevent repository
enumeration.

### 7.1 Routes

| Route | Description |
|---|---|
| `GET /{tenant}/{repo}` | Repository home: root directory tree of the default branch + rendered README |
| `GET /{tenant}/{repo}/tree/{ref}/{path}` | Directory listing at `path` on `ref` |
| `GET /{tenant}/{repo}/blob/{ref}/{path}` | File view with syntax highlighting |
| `GET /{tenant}/{repo}/raw/{ref}/{path}` | Raw file bytes (see §7.5 for safety headers) |
| `GET /{tenant}/{repo}/commits/{ref}` | Paginated commit log (50 commits per page, `?page=N`) |
| `GET /{tenant}/{repo}/commit/{oid}` | Single commit: metadata, message, and unified diff |

`{ref}` accepts a branch name, tag name, or 40-hex commit OID. The resolver
prefers the longest matching branch/tag prefix so that refs containing slashes
(e.g. `feature/foo`) are resolved correctly. When a branch and a tag share the
same name, the branch wins; use the tag's commit OID to browse the tag.

### 7.2 Branch and tag switcher

Each tree page shows a branch/tag `<select>` dropdown. When JavaScript is
available, htmx intercepts the `change` event and swaps only the tree table
in place (`hx-target="#tree"`, `hx-push-url="true"`), so the rest of the page
stays stable. Without JavaScript, the form falls back to a plain GET
(`?ref=<name>`) that navigates to the selected ref's root tree. Switching the
ref from a blob or commits page always performs a full navigation to the new
ref's tree root. The single-commit view is the exception: commits are addressed
by OID, so it omits the switcher (and skips the ref load entirely).

### 7.3 README rendering

The repository home page automatically renders a `README.md` or
`README.markdown` (case-insensitive) found at the root of the default-branch
tree. Rendering is a two-step pipeline:

1. **goldmark** converts Markdown to HTML.
2. **bluemonday** (UGC policy) sanitizes the HTML — scripts, event handlers,
   and unsafe markup are stripped before the result is embedded in the page.

If no README file is present, the root tree is shown without a rendered preamble.
README files that are binary or exceed the 10 MiB blob limit are silently
skipped.

### 7.4 Syntax highlighting and blob caps

| Condition | Behaviour |
|---|---|
| Text blob ≤ 1 MiB | Syntax-highlighted via **chroma** (class-based monokai with line numbers) |
| Text blob > 1 MiB | Plain escaped `<pre>` (no highlighting) |
| Binary blob (NUL byte in first 8 KiB) | Message + download link; no source rendered |
| Any blob > 10 MiB | "Too large" message; bytes are not fetched and the file is not downloadable (the raw endpoint returns HTTP 413) |

Markdown blobs (`.md` / `.markdown`, text, ≤ 1 MiB) offer a `[rendered]` toggle next to `[raw]`; appending `?view=rendered` shows the goldmark-rendered, bluemonday-sanitized HTML in the same `<div class="readme">` frame used by the repo-home README — the rendered view links back via `[source]`.

Chroma selects a lexer by filename; if that fails it falls back to content
analysis, then to a plain-text lexer. Output uses CSS classes (`WithClasses(true)`)
rather than inline styles — a requirement of the strict UI CSP. The stylesheet is
generated at startup and served at `/_ui/static/chroma.css`. If `--ui-dir` is
set and a `static/chroma.css` file exists under that directory, it is served
instead (theming hook). No separate download is required; the page `<base.html>`
links the stylesheet automatically. The monokai dark theme is embedded in the
generated stylesheet.

Line numbers in highlighted blobs are anchors (`#L42`), and ranges are
supported as `#L42:50` (lenient parsing also accepts `#L42:L50`, `#L42-50`,
and `#L42-L50`). Visiting a URL with such a fragment highlights the line or
range and scrolls the first line into view. Clicking a line number selects
that line and updates the fragment; click-and-drag on the number column
selects a range.

Blob and tree views also show relative times ("2h ago") with absolute UTC
tooltips, and file sizes are displayed in binary units (e.g. "1.2 KiB").

### 7.4a Tree activity column

Each directory listing includes a "last commit" column: the most recent commit
that touched each entry, with a relative timestamp and commit summary. The
attribution is computed from a single bounded history walk per tree page:

- **Walk depth**: the 200 most recent commits reachable from the current tree OID
  (constant `treeActivityWindow = 200`).
- **Output cap**: 8 MiB of `git log` output; if the walk hits the cap, the
  captured prefix is still parsed and used.
- **Renames**: `--no-renames` is passed, so a rename is attributed to the rename
  commit (shown as both an `A` and a `D` by git). The renamed-from path is not
  followed back into history.
- **Entries not touched in the window**: shown as "—" in the last-commit column.
  This is expected on repositories with long-lived paths or large histories.
- **Degradation**: if the walk fails for any reason (subprocess error, mirror
  unavailability), the column renders entirely as "—" and a `WARN` log is emitted.
  The rest of the tree page is unaffected.
- **Mirror cost**: the walk opens a mirror handle — the same handle used for the
  tree listing — so a tree page incurs one extra mirror-open wait recorded in the
  `web_browse_mirror_wait_seconds` metric.

### 7.5 Raw endpoint safety headers

The `/{tenant}/{repo}/raw/{ref}/{path}` endpoint serves file bytes directly.
Because repo content is attacker-controlled, every response is hardened:

| Header | Value |
|---|---|
| `X-Content-Type-Options` | `nosniff` |
| `Content-Security-Policy` | `default-src 'none'; sandbox` |
| `Content-Type` (text) | `text/plain; charset=utf-8` |
| `Content-Type` (binary) | `application/octet-stream` |
| `Content-Disposition` (binary) | `attachment; filename*=UTF-8''<RFC 5987 encoded name>` |

Blobs over the 10 MiB cap are not served at all: the raw endpoint returns
HTTP 413 rather than an empty attachment.

All other HTML browse pages (tree, blob, commits, commit, landing, login, error)
carry a strict UI-wide `Content-Security-Policy` applied by a middleware wrapper:

```
default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'
```

This policy is enforced because the UI has no inline scripts or styles
(class-based chroma + class-based diff rows). Remote images embedded in a
rendered README are **blocked** by `img-src 'self'` — the image alt text is
shown instead. This prevents a viewer's IP from being disclosed to a remote
image host. The raw endpoint overrides the UI-wide policy with its own stricter
`default-src 'none'; sandbox` directive.

### 7.6 Diff caps

Commit diffs are capped to prevent runaway page rendering:

- **300 files per commit** — additional files are silently omitted and a
  truncation notice is displayed.
- **3 000 changed (added/removed) lines per file** — files exceeding this limit
  show a "too large" notice in place of the diff hunks. Context lines (unchanged
  lines shown for surrounding context) are not counted toward this cap.
- **20 MiB raw patch** — the raw unified patch read from git is additionally
  byte-capped at 20 MiB before any line counting begins. An over-cap commit
  renders as truncated (the parsed prefix of the diff is shown; the final,
  possibly incomplete, file entry is dropped). Tree listings and raw commit
  objects carry similar internal byte caps (32 MiB and 4 MiB respectively).

### 7.7 Hybrid reader and cold-mirror warming

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

#### Serve flag

| Flag | Default | Description |
|---|---|---|
| `--ui-browse-timeout` | `20s` | Maximum wait for **cold mirror materialization** on a browse request. Requests that exceed this deadline receive HTTP 503. Does not cover the subsequent read-lock acquisition or git reads. |

### 7.8 Observability

Browse requests emit two metrics:

| Metric | Labels | Description |
|---|---|---|
| `web_browse_total` | `view` | Browse requests by view, counted after authorization (includes reads that subsequently fail with 404/503; per-outcome counts are in web_requests_total); `view` ∈ `repo`, `tree`, `blob`, `raw`, `commits`, `commit` |
| `web_browse_mirror_wait_seconds` | — | Time spent opening (and possibly materializing) the git mirror; emitted once per git read operation (a single page may perform several, e.g. repo home = tree + README), not once per request |

Code browse emits no audit events. Read operations are not audited.

### 7.9 Known limitations

- Path-filtered commit log (`git log -- <path>`).
- Blame view.
- File and commit search.
- Compare / branch-diff views.
- Cursor-based pagination (current log pagination is offset-based).
- Per-read audit events.
- Web clone / zip download.
- Branch and tag management through the UI.
- README remote-image proxy (remote images are blocked by the page CSP; a
  proxy would be needed to display them — alt text renders in their place).
- Git errors during a read surface as HTTP 404 when the object, ref, or path does
  not exist (missing-ref/missing-path/missing-object checks return ErrNotFound →
  404); an unexpected git failure that occurs after the object's existence is
  confirmed (e.g. cat-file content read, diff generation) surfaces as HTTP 500.

---

## 8. Settings and admin

CSRF-protected settings forms for three audiences: any logged-in user
(self-service), repo admins (per-repo settings), and global admins (instance
management). No dedicated stores or schema migrations: every operation wraps an
existing service that can also be driven from the CLI.

### 8.1 Page map

```
/settings                         self-service (any logged-in user)
  /settings                       profile: name, email, admin badge; change-password form
  /settings/tokens                list + create/revoke/rotate API tokens
  /settings/keys                  SSH public keys: list + add + revoke

/{tenant}/{repo}/settings         repo settings (repo-admin perm OR global admin)
  /settings                       general: public toggle; tenant LFS usage/cap (read-only);
                                  danger zone: rename (global admin only), delete (global admin only)
  /settings/access                user grants (add/change/revoke read|write|admin)
                                  + deploy SSH keys (add/revoke)
  /settings/webhooks              webhook endpoints (add/enable/disable/rotate-secret/remove)
                                  + per-endpoint delivery view with replay
  /settings/policy                protected-ref rules + protected-path rules (add/remove)
  /settings/triggers              build triggers (add/edit/enable/disable/rotate-secret/remove)
                                  + per-trigger delivery history with bounded replay (see §8.9)
  /settings/hooks                 custom hook scripts (global admin only — see §8.2)

/admin                            instance admin (global admin only)
  /admin/users                    list, create, disable/enable, set-email, delete
  /admin/repos                    list all repos, register (with in-process storage init), delete
  /admin/quotas                   per-tenant LFS usage/cap: set/clear/reconcile
```

Navigation: the navbar shows `[ settings ]` when a user is logged in and
`[ admin ]` when `IsAdmin`; repo browse pages show a `[settings]` link when the
viewer is repo-admin or global-admin.

Reserved tenant names: `admin`, `settings`, `login`, `logout`, `healthz`, and `_ui`
collide with the web UI's top-level routes — a tenant with one of those names
is unreachable through web browse/settings (git-protocol access via `.git`
paths is unaffected). Web repo registration refuses these names; the CLI does
not, so an operator who registers one accepts the web shadowing.

### 8.2 Authorization tiers

| Area | Who can access | Why |
|---|---|---|
| `/settings` | Any logged-in user | Self-service: own tokens, SSH keys, password |
| `/{t}/{r}/settings` (all tabs except hooks) | Global admin OR users with `admin` perm on the repo | Web-side meaning of the `admin` perm level |
| `/{t}/{r}/settings/hooks` | Global admin only | Custom hooks execute operator scripts on the server. Allowing repo-admins to register hooks would be privilege escalation. |
| `/admin/*` | Global admin only | Instance-wide user, repo, and quota management |
| **Repo rename** | Global admin only (not repo-admin) | Repo rename is auth-only: the auth.db row + dependent tables move atomically, but storage keys are NOT migrated — the operator moves the storage prefix out of band (see §8.3 *Repo rename and storage migration*). A repo-admin can't complete that, and the UI runs the same destination-prefix collision probe as the CLI to refuse renaming onto leftover/foreign objects |
| **Repo delete** | Global admin only (not repo-admin) | Irreversible; never purges storage from the UI — `--purge-storage` remains a CLI-only path |
| **Quota set/clear/reconcile** | Global admin only | LFS quotas are per-tenant LFS byte caps that constrain operator spend; a repo-admin raising their own cap defeats them |

Note on quota visibility: the repo-settings general tab shows the TENANT's
aggregate LFS usage/cap read-only to any repo-admin in that tenant (deliberate).
Quotas are tenant-scoped, so a repo-admin of one repo sees the tenant-wide
figure; operators who consider that sensitive should reserve the repo `admin`
perm accordingly.

All authorization failures return a uniform HTTP 404 (same anti-enumeration
stance as the browse and git gateway handlers). Unauthorized access to any
settings page or action is indistinguishable from "not found".

`Session.IsAdmin` is re-joined from the `users` table on every session lookup,
so admin revocation takes effect on the next request without requiring a
re-login.

### 8.3 Repo rename and storage migration

A UI rename (global admin only) updates **auth.db only** — the repo row plus
every FK-bearing dependent table (grants, deploy keys, protected refs/paths,
webhook endpoints) move atomically. Storage keys at
`tenants/<tenant>/repos/<old-name>/...` are **NOT** migrated by the rename.
Before the auth-side rename the UI runs the same destination-prefix collision
probe as the `bucketvcs repo rename` CLI (`store.List(destPrefix, MaxKeys:1)`)
and refuses the rename when the prefix is non-empty, so a rename can never point
a name at leftover/foreign objects (for example after a delete-without-purge).

After a successful rename the new name reads from an **empty** storage prefix
until you migrate. Move the storage tree out of band:

```
aws s3 mv s3://<bucket>/tenants/<tenant>/repos/<old>/ \
          s3://<bucket>/tenants/<tenant>/repos/<new>/ --recursive
```

(or `gsutil mv` / `az storage blob move` / `mv` on localfs). The manifest body
also carries absolute key references (`pack_key`, `idx_key`, index keys) that
contain the old prefix; rewrite them as part of the migration. If LFS quotas
drifted during the cutover, run `bucketvcs quota reconcile --tenant=<tenant>`.
See the [Repositories operator guide §3](repositories.md#3-rename-auth-only-semantics) for the full out-of-band procedure.

After a successful rename the **old name keeps working**: the web UI returns a
302 redirect to the new name (sub-path and query preserved); HTTPS/SSH git and
LFS transparently resolve to the renamed repo. The redirect is removed
automatically if the old name is later registered as a new repo. Manage aliases
with `bucketvcs repo alias list / remove`; see
[repositories §4](repositories.md#4-rename-redirects--aliases) for full details.

### 8.4 Form mechanics

Every settings GET issues a CSRF token embedded in a hidden `csrf_token` form
field; every POST runs the double-submit CSRF check before anything else. The
login `bvcs_csrf` cookie is reused (same double-submit model — see §4.2).

**POST-redirect-GET**: after a successful mutation the handler 303-redirects to
the current tab so that a browser refresh reloads the page, not the form. Flash
messages are carried in a short-lived `bvcs_flash` cookie (HttpOnly, cleared
on first render) to cross the redirect.

**Secret-once exception**: token create/rotate and webhook endpoint add/rotate-secret
render the result page *directly* (no redirect) so the plaintext credential can
be displayed exactly once. The page carries `Cache-Control: no-store, private`.
The page warns that refreshing re-submits the form; CSRF makes blind re-POSTs
unexploitable.

**Destructive actions** (repo delete, user delete, endpoint remove, hook remove)
require a type-the-name confirm field validated server-side; no JS-only confirms.

**Password change revokes other sessions**: a successful change at
`/settings/password` deletes the user's *other* web sessions (attacker-held
cookies die; the current session survives so the operator is not logged out).
API tokens are NOT auto-revoked — rotate or revoke those separately via
`/settings/tokens`.

### 8.5 SSRF note — webhook endpoint URLs

Repo admins can register webhook endpoint URLs. A malicious URL could cause the
server to issue HTTP requests to internal services. Restrict outbound egress
from the bucketvcs process using network policy or an egress firewall; see the
webhooks operator guide §11 for recommendations.

### 8.6 Nil-service degradation

Each settings service dependency (`Webhooks`, `Policy`, `Hooks`, `Quotas`) is
optional. When a service is not wired at startup, the corresponding tab or page
renders a "not enabled on this server" notice instead of forms; no panic or 500
occurs. This mirrors the `Content == nil` behavior that disables code browse.

Quotas are wired only when `--lfs=true`: LFS quota enforcement lives in the
LFS Batch handler, so with LFS off the `/admin/quotas` pages degrade to the
unavailable notice rather than offering knobs that nothing enforces (the
repo-settings quota display is likewise hidden).

The hooks tab returns HTTP 404 unconditionally for non-admin users regardless of
service availability (authz check precedes nil check).

### 8.7 Repo deletion across backends

Repo deletion works on every auth-db backend (sqlite, libsql, Postgres).

### 8.8 Settings observability

**Metrics**

| Metric | Labels | Description |
|---|---|---|
| `web_admin_actions_total` | `domain`, `action`, `result` | Count of settings-form actions; `result` ∈ `ok`, `invalid`, `error`; `domain` matches the settings area (e.g. `token`, `webhook`, `admin_users`, `admin_repos`, `admin_quotas`, `user`) |

**Audit events** — all settings events carry `source=web`; actor is the session user.

| Domain | Events |
|---|---|
| Users | `auth.user.created`, `auth.user.disabled`, `auth.user.enabled`, `auth.user.deleted`, `auth.user.email_set`, `auth.user.password_changed` |
| Tokens | `auth.token.created`, `auth.token.revoked`, `auth.token.rotated` |
| SSH keys | `auth.sshkey.added`, `auth.sshkey.revoked` (user vs deploy keys distinguished by a `kind` attr) |
| Repos | `repo.created`, `repo.deleted`, `repo.renamed`, `repo.public_set`, `repo.grant.added`, `repo.grant.removed` |
| Webhooks | `webhooks.endpoint_created`, `webhooks.endpoint_removed`, `webhooks.endpoint_enabled`, `webhooks.endpoint_disabled`, `webhooks.endpoint_secret_rotated`, `webhooks.delivery_replayed` |
| Policy | `policy.ref.rule_added`, `policy.ref.rule_removed`, `policy.path.rule_added`, `policy.path.rule_removed` |
| Hooks | `policy.hook.added`, `policy.hook.removed`, `policy.hook.enabled`, `policy.hook.disabled` |
| Quotas | `quota.set`, `quota.cleared`, `quota.reconciled` |

Existing event names from the CLI and gateway are reused; the settings screens
add no new event names.

### 8.9 Build triggers

The **Triggers** tab on a repository's settings page lets repo admins manage
build triggers — outbound calls that fire when a push updates a matching ref.
It is the browser equivalent of the `bucketvcs trigger` CLI; for the full
trigger model (token modes, ref globs, delivery semantics, cloud-specific
setup) see the [build triggers operator guide](build-triggers.md).

The tab is available only when build triggers are enabled at startup with
`--build-triggers=true`. When they are disabled the tab renders a "build
triggers are not enabled on this server" notice instead of forms (the same
nil-service degradation described in §8.6).

### Authorization

The Triggers tab is **repo-admin** — global admins and users holding the
`admin` permission on the repo, exactly like the Webhooks tab. Every mutation
is CSRF-protected and ownership-scoped: a trigger (and a delivery) is only
reachable through the repo it belongs to. A trigger id that names a different
repo or tenant returns a uniform HTTP 404, indistinguishable from "not found",
so the tab cannot be used to enumerate triggers in other repositories.

### Trigger kinds and fields

The create form's **kind** dropdown swaps the per-kind fields in place:

| Kind | Fields | Secret |
|---|---|---|
| `generic` | target URL | generated, shown once |
| `cloudbuild` | target URL (Cloud Build trigger run URL) | generated, shown once |
| `codebuild` | connector (optional), AWS region, AWS project | none |
| `azurewebhook` | Azure webhook URL, optional signature header | operator-supplied |
| `azurepipelines` | connector, Azure project, pipeline id | none (PAT lives in the connector) |

All kinds also accept optional ref include/exclude globs, a token mode
(`none` / `inject`), and optional token scopes and TTL.

### Connectors

`codebuild` and `azurepipelines` resolve their cloud credentials through an
operator-configured **connector** named in `--build-config` (AWS connectors hold
a region/profile or static keys; Azure connectors hold a PAT). The form's
connector dropdown lists only the connector names defined in that file — never
their secrets. **If the dropdown is empty, no connectors are configured**: add
them to `--build-config` and restart the gateway before creating a trigger of
that kind. A `codebuild` trigger may also omit the connector to fall back to the
gateway's ambient AWS credential chain.

### Secrets

`generic` and `cloudbuild` triggers sign their POST with a shared secret that
the server **generates and displays exactly once** on a secret-once page
(`Cache-Control: no-store, private`, same model as webhook secrets in §8.4) —
copy it before navigating away. `azurewebhook` secrets are **operator-supplied**
and must match the secret configured on the Azure service connection; an empty
value sends the webhook unsigned. `codebuild` and `azurepipelines` carry no
shared secret (they authenticate to the cloud API directly).

The **rotate-secret** action appears only on `generic` and `cloudbuild` rows —
the only kinds with a server-generated, rotatable secret. It renders a fresh
secret on the same secret-once page. Posting a rotate-secret request for any
other kind is rejected with a flash message.

### Editing

Edit changes the trigger's name, ref globs, token settings, and active state.
The **kind and its connection config are immutable** — to change the kind,
delete the trigger and create a new one.

### Delivery history and replay

Each trigger row links to its **deliveries** sub-page, a paginated record of
fire attempts (status, attempt count, last HTTP code, last error, delivery
time). The list shows **20 deliveries per page** with an `[older]` pager and a
status filter. Only the **10 most recent deliveries** are replayable; a
**replay** button appears on those rows and re-queues the delivery for another
attempt. The 10-delivery window is enforced server-side — a hand-crafted replay
POST for an older delivery is rejected even though no button is shown for it.

### Observability

Trigger mutations increment `web_admin_actions_total` with `domain=trigger`
and `action` ∈ `add`, `edit`, `enable`, `disable`, `remove`, `rotate_secret`,
`delivery_replay`. Audit events are emitted under the `buildtrigger.*` namespace
(`buildtrigger.created`, `buildtrigger.edited`, `buildtrigger.enabled`,
`buildtrigger.disabled`, `buildtrigger.removed`, `buildtrigger.secret_rotated`,
`buildtrigger.delivery_replayed`), each carrying `source=web` and the session
user as actor.

---

## 9. Observability

### 9.1 Metrics

| Metric | Labels | Description |
|---|---|---|
| `web_requests_total` | `route`, `status` | Request count by UI route and HTTP status |
| `web_login_total` | `result` | Login outcomes: `success`, `invalid`, `ratelimited` |
| `web_sessions_active` | — | Count of non-expired sessions |
| `web_browse_total` | `view` | Browse requests by view, counted after authorization (includes reads that subsequently fail with 404/503; per-outcome counts are in web_requests_total); `view` ∈ `repo`, `tree`, `blob`, `raw`, `commits`, `commit` |
| `web_browse_mirror_wait_seconds` | — | Mirror open/materialize latency; emitted once per git read operation (a single page may perform several, e.g. repo home = tree + README), not once per request |
| `web_admin_actions_total` | `domain`, `action`, `result` | Settings-form actions (see §8.8) |

### 9.2 Audit events

| Event | When |
|---|---|
| `auth.session.created` | Session cookie issued after successful password check |
| `auth.session.destroyed` | Session deleted via `/logout` |
| `auth.session.revoked` | A single session revoked from the session manager (self-service) |
| `auth.session.revoked_all` | A user signed out all their other sessions |
| `auth.session.admin_revoked` | An admin revoked a session from `/admin/sessions` |
| `auth.password.set` | Password hash updated via `user set-password` |

OIDC login and settings-form actions emit additional events; see §6.4 and §8.8.
The session manager and audit viewer that surface these events are documented in
§10.

> These web-login audit events are emitted inside `bucketvcs serve`, so they are
> **shipped durably** to `sys/logs/activity/` by default — see
> [log shipping](log-shipping.md) and the
> [observability overview](observability.md).

---

## 10. Session management and audit viewer

Two operator-facing observability surfaces ship in the UI: a session manager (a
user sees and revokes their own browser sessions; an admin sees every session)
and an audit-log viewer (a global view for admins and a repo-scoped tab for repo
admins). Both are read-mostly views over data the gateway already produces — the
session manager reads the sqlite session table directly, and the audit viewer
reads the activity log objects that log shipping has already written to object
storage.

### 10.1 Session management

**Self-service — `/settings/sessions`.** Any logged-in user sees a table of their
own active browser sessions: the login provider (`password` or `oidc`), when each
was created, when it was last seen, and when it expires. The session backing the
current browser is badged **current** and cannot be individually revoked — use
**log out** to end it.

| Action | Route | Effect |
|---|---|---|
| List own sessions | `GET /settings/sessions` | The current session is badged `current`; all others show a **revoke** button. |
| Revoke one session | `POST /settings/sessions/revoke` | Signs out a single other session by its id hash. A revoked session's cookie is immediately invalid — the next request from that browser bounces to `/login`. |
| Revoke all others | `POST /settings/sessions/revoke-all` | Signs out every session except the current one. Use this after losing a device or suspecting a leaked cookie. |

Revocation is user-scoped at the store: a session id hash belonging to another
user cannot be revoked even if guessed, and the current session is protected
against a hand-crafted revoke POST (the handler refuses to revoke the cookie it
arrived on).

Note: a successful **password change** at `/settings/password` already revokes
the user's *other* sessions automatically (see §8.4). The session manager is the
manual equivalent, and additionally surfaces what is currently active.

**Admin — `/admin/sessions`.** A global admin sees **all** sessions across all
users, each row labelled with the owning user's name, provider, and timestamps,
and a **revoke** button. This is the operator tool for forcibly signing out a
specific user's session (for example during an incident). The page renders the
first 500 sessions; on a larger deployment use `bucketvcs session list`
(NDJSON; one row per session). CLI revocation: `bucketvcs session revoke
--id-hash=<h>` for one session, or `--user=<name>` for all of a user's
sessions — a session whose user row was deleted shows as `(deleted)` and is
revocable by hash only. Admin
revocation removes the session regardless of which user owns it.

Every session revocation emits a tagged audit event (`auth.session.revoked`,
`auth.session.revoked_all`, or `auth.session.admin_revoked`) carrying the acting
user; admin revocations additionally record the target user (resolved
server-side before the delete). These join `auth.session.created`, `auth.session.destroyed`, and
`auth.oidc.login` as part of the shipped activity stream, so the full session
lifecycle — login through revocation — is visible in the audit viewer below.

Note: other web-originated admin actions (build-trigger, webhook, policy, and
repo changes made through the UI) are written to the server's process log with
`source=web`, but are **not** currently part of the shipped activity stream the
viewer renders. Surfacing those in the viewer is a documented follow-up (§11); for
now, audit them via the process log.

### 10.2 Audit-log viewer

The audit viewer renders the durable activity log — the stream of audit events
(logins, repo changes, policy rejections, webhook and LFS activity, …) that
`bucketvcs serve` ships to `sys/logs/activity/` in your object store. There are
two entry points:

- **Global — `/admin/audit`** (global admin only): every audit event across all
  tenants and repos, with filter controls.
- **Per-repo — `/{tenant}/{repo}/settings/audit`** (repo-admin or global admin):
  the same viewer hard-scoped to a single repository.

#### Shipping-lag semantics

This is the most important operational fact about the viewer: **audit events
appear only after the next ship.** The gateway buffers audit records in a local
spool and ships them to object storage in batches; the viewer reads the shipped
objects, never the live spool. As a result:

- **Log shipping must be enabled** (`--log-shipping=on`, the default). With
  shipping off, the gateway emits no activity objects and the viewer shows the
  empty state — it is not a live tail.
- **The most recent activity lags.** An event you just triggered does not appear
  until the spool rotates and ships, which happens after `--log-ship-interval`
  (default 15m), after `--log-ship-max-events` accumulate, or on a graceful
  shutdown (which flushes the spool before exit). Both audit pages carry a banner
  to this effect: *"audit events are shipped to object storage on a delay; the
  most recent activity may not appear yet."*
- For a tighter feedback loop in a staging environment, lower
  `--log-ship-interval` (e.g. `5s`) so events surface within seconds. In
  production the default interval keeps object-write volume reasonable; the
  viewer is an after-the-fact audit trail, not a real-time monitor.
- **On a deployment that has been idle for months, the first page can render
  empty with an `[older]` link** while the viewer walks older partitions — each
  page scans a bounded window of ~100 day partitions, so a long quiet stretch
  may exhaust the window before reaching data. Keep clicking `[older]`, or use a
  `since`/`until` filter to jump the walk straight to the right date range.

See the [log shipping operator guide](log-shipping.md) for the full shipping
model, spool sizing, and crash-recovery behaviour.

#### Filter controls

Both pages offer a filter form whose fields narrow the result set:

| Field | Matches |
|---|---|
| **event** | Event-name **prefix** — e.g. `policy.` matches every `policy.*` event; `auth.session.` matches session events. |
| **actor** | Exact actor (the acting user). |
| **since** / **until** | Date bounds (`YYYY-MM-DD`); `until` is inclusive of the named day. Unparseable dates are ignored. |
| **tenant** / **repo** | *Global page only* — exact tenant/repo. The per-repo tab omits these (the repo is forced). |

Results are paginated with an `[older]` link that carries the active filters
forward. Filtering happens over the shipped objects, so the same lag applies to
filtered views.

#### Per-repo scoping boundary

The per-repo tab at `/{tenant}/{repo}/settings/audit` is **strictly scoped to
that one repository**. The handler force-sets the tenant/repo filter to the repo
in scope and ignores any `?tenant=`/`?repo=` query override, so a repo admin can
never read another tenant's or repo's events through it. Two consequences worth
calling out:

- **Cross-repo events never leak.** An event belonging to `acme/other` will not
  appear on `acme/demo`'s audit tab, even for a global admin viewing it through
  the per-repo route.
- **Account- and auth-level events are not repo-scoped and do not appear here.**
  Logins, token rotation, user create/disable, session revocation, and similar
  identity events carry no tenant/repo, so they show up only on the global
  `/admin/audit` page — never on a per-repo tab. The per-repo banner states this
  explicitly. Use the global viewer for the full identity audit trail.

When no audit storage is configured the viewer renders an "audit log viewing is
not available" notice rather than an error.

---

## 11. Deferred work

Features not yet implemented:

**OIDC follow-ups:**

- Multiple simultaneous IdPs (one issuer per process today).
- Auto-provisioning of unknown users (operator pre-creation is required).
- RP-initiated logout / IdP session termination.
- OIDC identity link/unlink UI (use the `user identity` CLI today).

**Code browse:**

- Path-filtered commit log, blame view, file/commit search, compare/branch-diff
  views, cursor-based pagination, per-read audit events, web clone/zip download,
  branch and tag management through the UI, README remote-image proxy (see §7.9).

**Settings and admin:**

- Surfacing web-originated admin actions (build-trigger, webhook, policy, and repo
  changes made through the UI) in the shipped activity stream. They are logged to
  the process log with `source=web` today but are not yet rendered by the audit
  viewer (see §10.1).
- Per-user login-history view (the audit viewer surfaces session events, but a
  dedicated per-user login timeline is not yet built).
- Session TTL/expiry controls per user (TTL is instance-wide via
  `--ui-session-ttl`).
- Repo transfer between tenants.
- Storage purge from the UI (`--purge-storage` remains CLI-only).
- Repo delete on Postgres auth-databases (`ErrCascadeUnsupportedBackend`; see §8.7).
