# Sessions CLI + audit-reader date-partition listing — Design

Closes two documented Phase C deferrals
(`2026-06-10-observability-surface-design.md`):

- there is **no sessions CLI** — the admin sessions page's truncation hint and
  the operator guide point at querying the auth DB directly;
- the audit `Reader` **lists the entire activity prefix on every page view**
  (the `TODO(v2)` at `internal/auditlog/reader.go`), a cost that grows
  unboundedly with deployment age (~35k keys/year at default ship settings)
  and is reachable by every repo admin via the per-repo audit tab.

## 1. Goal

1. `bucketvcs session list` / `bucketvcs session revoke` — a CLI escape hatch
   past the web UI's 500-row display cap, and incident-grade revocation
   without a browser.
2. Replace the Reader's full-prefix `listKeys` with a **backward day-walk**
   over the date-sharded key layout
   (`<prefix>/activity/YYYY/MM/DD/HHMMSS-<instance>-<seq>.ndjson.gz`), so a
   page view costs one floor probe plus a handful of small day lists instead
   of an O(deployment-age) scan.

## 2. Sessions CLI

New top-level `session` command group, mirroring the `token` group's shape
(`--auth-db` flag, NDJSON output, exit 2 on usage errors).

### 2.1 `session list`

```
bucketvcs session list --auth-db=<path> [--user=<name>]
```

- Calls the existing `sqlitestore.ListAllSessions(ctx, 0)` (no limit; the
  COUNT+rows read transaction and `(deleted)` LEFT JOIN behavior from Phase C
  apply unchanged).
- Emits one NDJSON object per session:
  `{"id_hash":…,"user_id":…,"user":…,"provider":…,"created_at":…,"expires_at":…,"last_seen":…}`
  (unix-seconds timestamps, matching `auth.AdminSessionInfo`).
- `--user=<name>` filters client-side on the joined user name (exact match).
  No new store method.

### 2.2 `session revoke`

```
bucketvcs session revoke --auth-db=<path> --id-hash=<hex>
bucketvcs session revoke --auth-db=<path> --user=<name>
```

- `--id-hash` and `--user` are mutually exclusive; exactly one is required
  (exit 2 otherwise).
- `--id-hash` calls `DeleteSessionByHash`; `--user` resolves the user via the
  existing user-lookup (error if no such user) and calls
  `DeleteSessionsForUser(uid, "")` (delete all).
- Prints `revoked=<n>` on stdout. `n == 0` is success (idempotent), with a
  distinguishable `revoked=0` so scripts can detect the no-op.
- Audit: emits `auth.session.admin_revoked`-shaped events to **stderr only**,
  consistent with the documented CLI-emitter limitation (CLI events are not
  shipped; observability guide §6).

### 2.3 Docs

- `internal/web/templates/admin_sessions.html` truncation hint:
  "query the auth DB" → "use `bucketvcs session list`".
- `web-ui.md` §10.1 and `observability.md` likewise.

## 3. Reader date-partition walk

### 3.1 Ordering contract (prerequisite)

`ObjectStore.List` already returns keys in lexicographically **ascending**
order on all four adapters (S3/GCS/Azure guarantee it; localfs sorts
explicitly). The interface doc does not say so. This design:

- documents the guarantee on `ObjectStore.List` in
  `internal/storage/objectstore.go`;
- adds a conformance assertion (list a multi-page fixture, assert ascending
  across pages).

The day-walk's floor probe depends on this contract.

### 3.2 Walk algorithm

`Page(ctx, filter, cursor)` keeps its signature, cursor semantics (an object
key; the next page consumes strictly-older keys), and all Phase C behaviors
(skip logging, per-page object/byte/event caps, stable sort). Only key
discovery changes:

1. **Floor probe** (once per `Page` call):
   `List(prefix, {MaxKeys: 1})` → the first key is the oldest object; its
   embedded date is the **floor day**. Empty result → empty page, done.
2. **Start day**: the cursor's embedded date when a cursor is supplied,
   otherwise today (UTC). If `filter.Until` is set and earlier, start there;
   if `filter.Since` is set and later than the floor, raise the floor.
3. **Walk**: for day D from start down to floor, list
   `<prefix>/YYYY/MM/DD/` (small; one page in practice, paginate via
   ContinuationToken if not). Sort the day's keys descending, drop keys
   `>= cursor` (first day only), and feed them to the existing per-object
   consume loop (Get → DecodeGz → filter → caps). Stop when the page's
   object/byte/event cap fires or the floor day is exhausted.
4. **Day-list budget**: cap day lists per `Page` call (constant, 100). If the
   budget is hit before the page fills (a very sparse multi-year prefix), stop
   and return a **synthetic cursor** `<prefix>/YYYY/MM/DD/~` for the first
   not-yet-listed day — `~` sorts above the key charset, so the cursor is
   larger than every key in that day and smaller than every key in newer
   days; resume-strictly-older starts the next page at that day, inclusive.
   The UI just shows `[older]`.

The next-cursor rule is unchanged for the normal case: the key of the oldest
object consumed, or "" when the floor day was exhausted with no older days
remaining.

Old cursors (full object keys) work unchanged — their dates are embedded.

### 3.3 What this deletes

- `listKeys` (full-prefix enumeration) and its `TODO(v2)` comment.
- The round-9 "in-memory listing cache" stopgap idea — moot.

### 3.4 Unchanged

`DecodeGz`, `Filter`, all Reader fields/caps/Logger, web handlers, templates,
cursor opacity posture (raw keys; accepted-risk note stays).

## 4. Error handling

- CLI: sqlite open/query errors → message on stderr, exit 1. Usage errors →
  exit 2.
- Reader: floor-probe or day-list errors → returned from `Page` (same as
  today's `listKeys` error path). Per-object Get/decode failures keep the
  best-effort skip + Logger warning.

## 5. Testing

- **CLI**: table tests mirroring the `token` CLI tests — list output shape,
  `--user` filter, revoke by hash, revoke by user, mutual-exclusion usage
  errors, `revoked=0` no-op.
- **Reader** (fake store): multi-day spread walks newest-first across
  partitions; day gaps cost nothing extra; cursor resume mid-day and across
  days; synthetic-cursor resume after a day-budget stop; `since`/`until`
  narrowing the walk range; floor probe on empty store; **existing Reader
  tests pass with their key fixtures updated to the production date-sharded
  layout** (the old flat fake keys, e.g. `sys/logs/activity/120000`, never
  occur in production — shiplog always writes
  `…/YYYY/MM/DD/HHMMSS-<instance>-<seq>.ndjson.gz`). A key under the prefix
  that does not match the layout makes `Page` fail loudly (operator junk in
  the log prefix) rather than being silently invisible.
- **Conformance**: ascending-order assertion in the storage conformance
  suite (runs against all 4 adapters in CI).
- **Smoke**: extend `scripts/smoke-observability.sh` with a
  `bucketvcs session list` assertion (the serve-down CLI bootstrap step
  already has the auth DB).

## 6. Out of scope (deferred)

Opaque (HMAC) cursors; per-route rate limiting on the audit pages; retention
sweeping of `sys/logs/`; a `session list` JSON API over HTTP; shipping
CLI-emitted audit events.
