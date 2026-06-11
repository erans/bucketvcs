# Observability surface: sessions + audit viewer — design

Date: 2026-06-10
Status: approved (brainstorm 2026-06-10)
Predecessors: M24 Web UI Phases 1–3; Build Triggers UI; Browse depth
(`2026-06-10-browse-compare-history-design.md`). Builds on M29 log shipping
(`internal/shiplog`) and the M24 `sessions` table.

This is **Phase C** of the web-ui roadmap (A build-triggers ✓ → B browse-depth ✓ →
**C observability surface** → D self-service & lifecycle). Blame/search (Phase B.2)
and roadmap D remain separate phases.

## 1. Goal

Make the server's existing observability data visible in the browser:

- **Sessions** — a self-service `/settings/sessions` (see your logged-in sessions,
  revoke individually or all-others) and a global-admin `/admin/sessions` (all
  sessions, revoke). Uses the real queryable `sessions` table.
- **Audit viewer** — read the shipped activity log (`sys/logs/activity/…ndjson.gz`)
  back from the bucket and render it, both globally (`/admin/audit`, all events) and
  per-repo (`/{t}/{r}/settings/audit`, strictly filtered to that tenant+repo).

Non-goals: a new queryable audit store (we read the shipped NDJSON); real-time audit
(shipping is async — viewer shows shipped events + a lag note); tailing local pending
files; security-event alerting; metrics dashboards.

## 2. Architecture

Two features under one surface; new code is isolated behind narrow interfaces so
`internal/web` never imports `internal/storage` directly.

| Layer | Addition |
|-------|----------|
| `internal/auth/sqlitestore/sessions.go` | `ListSessionsForUser`, `DeleteSessionByHashForUser`, `ListAllSessions`, `DeleteSessionByHash` + `SessionInfo`/`AdminSessionInfo` types |
| `internal/auditlog` (NEW) | read-side sibling of `internal/shiplog`: `Event` type, `DecodeGz`, `Reader` (lists/fetches/decodes/filters/paginates the activity stream), `Filter` |
| `internal/web/services.go` | `SessionAdmin` interface (sqlitestore slice) + `AuditReader` interface (satisfied by `*auditlog.Reader`) |
| `internal/web` | `/settings/sessions`, `/admin/sessions`, `/admin/audit`, `/{t}/{r}/settings/audit` handlers + templates + nav links |
| `cmd/bucketvcs/serve.go` | wire `AuditReader` from the in-scope `ObjectStore` + shiplog prefix; wire `SessionAdmin` from the auth store |

Reuse: admin chassis (`requireAdmin`, `adminnav`, `admin_*.html`), settings chassis
(`requireUser`, settings tabs), repo-settings chassis (`reposettingsnav`,
`handleRepoSettings` tab switch, `canAdminRepo`), `renderBuffered`/`EmitRequestMetric`,
`postGuard` (CSRF), `redirectFlash`, the `.pager` bracket idiom.

## 3. Sessions

### Backend (`internal/auth/sqlitestore/sessions.go`)
- `type SessionInfo struct { IDHash, Provider string; CreatedAt, ExpiresAt, LastSeen int64 }` — **never the raw id** (it exists only in the client cookie).
- `ListSessionsForUser(ctx, userID) ([]SessionInfo, error)` → `SELECT id_hash, provider, created_at, expires_at, last_seen FROM sessions WHERE user_id = ? ORDER BY last_seen DESC`.
- `DeleteSessionByHashForUser(ctx, userID, idHash) (int64, error)` → `DELETE … WHERE user_id = ? AND id_hash = ?` (user-scoped: a user cannot revoke another user's session even with a guessed hash). Returns rows affected.
- For admin: `type AdminSessionInfo struct { SessionInfo; UserID, UserName string }`; `ListAllSessions(ctx) ([]AdminSessionInfo, error)` (joins `users`); `DeleteSessionByHash(ctx, idHash) (int64, error)`.
- `hashSessionID(rawID)` already exists — the handler hashes the request cookie's raw id to mark the current row.

### Self-service `/settings/sessions`
A new tab on the existing `/settings` surface (alongside profile/tokens/keys). Lists
the user's sessions (provider · created · last-seen · expires), marking the **current**
one (`hashSessionID(cookieRawID) == IDHash`). The current session is shown but not
individually revocable (log out instead). Actions:
- `[revoke]` per non-current session → POST `id_hash` → `DeleteSessionByHashForUser(currentUserID, idHash)`; flash.
- `[revoke all other sessions]` → reuses the existing `DeleteSessionsForUser(userID, currentRawID)`; flash with count.

### Admin `/admin/sessions`
`requireAdmin`. Lists all sessions (user name · provider · created · last-seen · expires);
`[revoke]` per row → `DeleteSessionByHash(idHash)`. New `sessions` link in `adminnav`.

### Security
All revokes CSRF-guarded. Self-service revoke is user-scoped at the SQL level. Only
`id_hash` is ever rendered (already a non-reversible hash); the usable session id is
never exposed.

## 4. Audit reader (`internal/auditlog`)

### `Event`
```
type Event struct {
    Ts     time.Time
    Level  string
    Event  string            // slog message, e.g. "policy.ref.rejected"
    Tenant string            // from "tenant" attr ("" if absent)
    Repo   string            // from "repo" attr
    Actor  string            // from "actor", else "user" attr
    Attrs  map[string]any    // all remaining decoded fields (for a details expander)
}
```

### Decoder
`DecodeGz(r io.Reader) ([]Event, int, error)` — gunzip → scan NDJSON lines →
`json.Unmarshal` each into a `map[string]any`, lift `ts`/`level`/`event`/`tenant`/
`repo`/`actor`/`user` into typed fields, put the rest in `Attrs`. A malformed/empty
line is skipped and counted (returned int = skipped count); a single bad line never
fails the batch. A gzip-level error returns the error.

### `Reader`
Depends on a minimal object-store slice (the real `storage.ObjectStore` satisfies it):
```
type ObjectStore interface {
    List(ctx, prefix string, opts *storage.ListOptions) (*storage.ListPage, error)
    Get(ctx, key string, opts *storage.GetOptions) (*storage.Object, error)
}
```
Constructed with that store + the activity prefix (`<shiplogPrefix>/activity`, default
`sys/logs/activity`). Tunables: `ObjectsPerPage` (default 20), `MaxDecompressedBytes`
per page (default 32 MiB).

`Page(ctx, f Filter, cursor string) (events []Event, next string, err error)`:
- Lists keys under the activity prefix. Activity keys embed the date+time
  (`<YYYY>/<MM>/<DD>/<HHMMSS>-…`) so they sort lexically oldest→newest; the viewer pages
  from the **newest** end backward. `cursor` is the oldest object key already consumed
  (""=start at newest).
- Walks objects newest→older starting just-older than `cursor`, fetching up to
  `ObjectsPerPage` objects (and stopping early if cumulative decompressed bytes would
  exceed `MaxDecompressedBytes`), `DecodeGz` each, append matching events.
- Applies `f` in-memory; sorts the page's events by `Ts` descending; returns them and
  `next` = the oldest object key consumed this page ("" when no older objects remain).
- A `List` failure → error (handler → 500). A per-object `Get`/decode failure → logged
  and skipped (best-effort), page continues.

### `Filter`
```
type Filter struct {
    EventPrefix string    // "" = all; matches Event.Event by prefix (e.g. "policy.")
    Tenant      string    // exact; "" = any
    Repo        string    // exact; "" = any
    Actor       string    // exact; "" = any
    Since, Until time.Time // zero = unbounded
}
```
Applied entirely in-memory over the read window. **For the per-repo surface the handler
sets `Tenant`+`Repo` and they are non-overridable by the client.**

### Lag / availability
Only shipped objects are read. The page model surfaces "shows events shipped to
storage; in-flight events appear after the next ship". When the activity prefix lists
empty (shiplog disabled or nothing shipped yet) → an empty page (the handler renders a
"not being shipped / no events yet" notice).

## 5. Audit surfaces (`internal/web`)

### Global `/admin/audit`
`requireAdmin`. A plain GET filter form (event-prefix `<select>` of known prefixes
`auth.`/`policy.`/`lfs.`/`webhooks.`/`buildtrigger.`/`repo.`, tenant, repo, actor,
since/until text) + the object-cursor pager (`[older]` carries `?cursor=` plus the
active filters). Rows: time · event · tenant/repo · actor · `[details]` toggle
(renders `Attrs` as a definition list). New `audit` link in `adminnav`.

### Per-repo `/{t}/{r}/settings/audit`
`canAdminRepo` (via `handleRepoSettings`). Same reader, but the handler hard-sets
`Filter.Tenant = sr.tenant`, `Filter.Repo = sr.repo` and ignores any client-supplied
tenant/repo. Only repo-scoped events appear (push/lfs/policy/webhook/buildtrigger/repo
lifecycle); user-scoped auth/session events have no repo attr and are absent. New
`audit` tab in `reposettingsnav`. Row layout omits the (implied) tenant/repo columns.

### Authz / cross-tenant boundary
The per-repo filter is enforced in `Reader.Page` server-side; a test asserts a
foreign-tenant line sharing an NDJSON object is excluded, and that the per-repo handler
ignores a client `?tenant=`/`?repo=` override. The global page is unreachable without
`requireAdmin`.

### Enabled gate
When the `AuditReader` dep is nil (no `--store`, or wiring disabled) both surfaces
render a "audit log not available" notice (like the webhooks tab), never a 500.

## 6. Security & errors

- **Secret-free invariant**: the audit stream must remain secret-free *at emission*
  (verified across prior phases — secrets only flow through `renderSecretOnce`, never
  into audit attrs). The viewer renders shipped attrs as-is and adds no new sink; it
  does **not** attempt a redaction pass (it cannot reliably classify arbitrary attrs).
  The spec records this as a standing invariant.
- **Resource bounds**: `ObjectsPerPage` + `MaxDecompressedBytes` caps bound memory
  against a giant or compression-bomb object; `Get`/decode run under the request
  context deadline.
- **Errors**: nil reader / empty prefix / nothing shipped → empty-state (200). One bad
  object → skip + log. `List` failure → 500 (logged, generic message). Unparseable
  since/until → flash + re-render with that filter cleared. Revoke of an absent or
  not-owned hash → benign no-op flash (no enumeration).

## 7. Testing

- **`internal/auditlog`**: `DecodeGz` (typed fields + Attrs; skipped malformed line
  count; empty/!gzip input); `Reader.Page` over a fake object store (newest-first
  ordering; object-cursor paging; each filter incl. EventPrefix/Tenant/Repo/Actor/time;
  **cross-tenant exclusion**; object & byte caps; empty store → empty page + ""
  cursor).
- **`sqlitestore`**: `ListSessionsForUser` (order, fields, no raw id);
  `DeleteSessionByHashForUser` (user-scoped — cannot delete another user's row);
  `ListAllSessions`/`DeleteSessionByHash`.
- **`web`**: self-service sessions (list, current marked, revoke-one user-scoped,
  revoke-all-others, CSRF reject); `/admin/sessions` (admin-only 404 for non-admin,
  revoke); `/admin/audit` (rows + filter form + pager; non-admin 404; nil-reader
  notice); per-repo audit (forces tenant/repo; non-repo-admin 404; ignores client
  `?tenant=` override; nil-reader notice).
- **Smoke**: serve with `--store` + log shipping on; emit a couple of audit events
  (e.g. a push + a policy reject); flush a ship; assert `/admin/audit` shows them and
  the per-repo tab shows only that repo's.
- **Operator guide**: an "Observability" section — sessions management; the audit
  viewer, its shipping-lag semantics, and that it requires log shipping enabled.

## 8. Out of scope (deferred)

A new queryable audit store; real-time audit (no shipping lag); tailing local pending
files; per-user activity view; security-event alerting; metrics dashboards; CSV/JSON
export of audit results; saved filters. Roadmap D (self-service & lifecycle) is a
separate phase.
