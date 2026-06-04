# M25: Deployment Hardening — Design

**Date:** 2026-06-04
**Status:** Approved
**Scope:** Three deliverables that close known operational gaps: (A) postgres
repo-delete cascade, (B) webhook egress deny-list (SSRF), (C) `bucketvcs doctor`
read-only diagnostics.

**Out of scope:** macOS notarization (stays YAGNI per the goreleaser design —
needs an Apple Developer account, Developer ID cert, and notarytool CI secrets;
external setup, revisit on demand). Repair/`--fix` tooling for doctor. Per-tenant
egress policy. Multi-region (next milestone, M26).

---

## A. Postgres repo delete

### Problem

`DeleteRepoCascade` (internal/auth/sqlitestore/deletecascade.go:54) refuses on
postgres (`ErrCascadeUnsupportedBackend`; CLI hint at cmd/bucketvcs/repocmd.go:503).
The M15.1 drain design requires `webhook_endpoints` rows to **survive** repo
deletion so pending `repo.deleted` deliveries drain. SQLite achieves this with
`PRAGMA foreign_keys=OFF` (per-connection), suppressing the `ON DELETE CASCADE`
FK from migration 0006. Postgres has no way to suppress a CASCADE FK
mid-transaction, so the cascade would destroy pending deliveries.

### Decision: drop the FK on postgres

Chosen over (rejected) alternatives:
- *Snapshot URL+secret into webhook_deliveries* — also fixes the operator-guide
  §2.4 orphan-endpoint pitfall, but reworks the M15 claim query (joins
  endpoints for `active=1`) and worker; too large a blast radius for the same
  operator-visible outcome.
- *Special-case repo.deleted deliveries with embedded credentials* — creates a
  delivery shape diverging from every other event.

### Changes

1. **Migration 0015**, both backends, numbering kept in lockstep:
   - Postgres: `ALTER TABLE webhook_endpoints DROP CONSTRAINT <fk-name>` (exact
     constraint name read from migration 0006 / pg catalog).
   - SQLite: no-op placeholder. Its FK stays in the schema but remains
     effectively decorative under the existing pragma path.
   - Migration comments on both sides explain the drain-design rationale.
2. **`DeleteRepoCascade`**: remove the `backend.Name() == "postgres"` refusal.
   The postgres path runs the same single transaction with the same explicit
   child-table DELETEs as sqlite (protected_refs, protected_paths, hooks,
   repo_permissions, repo-scoped ssh_keys, lfs_locks, then repos). Explicit
   DELETEs are kept even where PG's remaining CASCADE FKs would do the job, so
   behavior doesn't silently depend on FK presence and both backends read
   identically. No pragma needed on PG.
3. **CLI**: drop the "repo delete requires a sqlite/libsql auth-db today" hint.
   `ErrCascadeUnsupportedBackend` stays defined for future backends.
4. **Tests**: gated postgres conformance suite gains cascade cases — child rows
   gone, `webhook_endpoints` rows survive, pending `repo.deleted` delivery
   drains, `--purge-storage` unaffected.
5. **Docs**: postgres.md operator-guide limitation paragraph removed.

---

## B. Webhook egress deny-list (SSRF)

### Problem

internal/webhooks/worker.go:249 POSTs to the stored endpoint URL with a stock
`http.Client` — no IP filtering, follows up to 10 redirects. The only validation
anywhere is a scheme-prefix check in the web UI form (CLI-registered endpoints
bypass even that). A compromised or malicious admin account can aim deliveries
at cloud metadata (169.254.169.254), loopback admin ports, or RFC1918 services.

### Decision: dial-time enforcement, deny-private by default

Enforcement lives in a `DialContext` wrapper, not URL-string validation:

1. **Hostname deny-list** (checked first, pre-resolution): repeatable
   `--webhook-deny-host=<pattern>`, case-insensitive, glob-style — exact names
   (`metadata.google.internal`) or wildcard suffix (`*.internal.example.com`).
   Default: empty. This is **policy, not a security boundary** — a raw IP or an
   alternate DNS name bypasses it. It earns its keep where IP rules can't
   reach: internal services that resolve to *public* IPs (split-horizon DNS,
   internal apps behind public load balancers).
2. **IP deny set** (checked on the **resolved** address, closing DNS rebinding):
   denied by default —
   - loopback: `127.0.0.0/8`, `::1`
   - link-local: `169.254.0.0/16` (covers cloud metadata), `fe80::/10`
   - private: `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, ULA `fc00::/7`
   - unspecified, multicast, broadcast
3. **Allow holes**: repeatable `--webhook-allow-cidr=<cidr>` punches holes in
   the IP deny set (e.g. `192.168.1.0/24` for a LAN receiver).
   `--webhook-allow-cidr=0.0.0.0/0` is the documented "old behavior" form — no
   separate allow-all flag.
4. **No allow-host list**, deliberately: allow-by-name against an IP deny set
   means "this name may resolve to private IPs", which re-opens DNS rebinding
   through the allowed name. Private receivers are reached via allow-cidr
   scoped to their range — rebinding-safe.

Evaluation order: deny-host match → denied; else resolve → IP in deny set and
not in any allow-cidr → denied; else dial.

### Client hardening (rides along)

- Built in worker.go:86 when `cfg.HTTPClient == nil`; the injection point stays
  for unit tests.
- **No redirects**: `CheckRedirect` returns `http.ErrUseLastResponse`; a 3xx is
  a delivery failure (industry convention; the dialer would re-check hops
  anyway, but not following is simpler to reason about).
- **Scheme gate at the worker**: http/https enforced at delivery time, covering
  CLI-registered endpoints.

### Failure semantics

An egress-denied delivery is a normal failure: existing 1m/30m/2h/12h backoff,
dead_letter after 5 attempts, distinct `last_error`
("egress denied: 10.0.0.5 in blocked range, see --webhook-allow-cidr").
Rationale for retrying rather than instant dead-letter: the common case is an
operator who forgot the flag — they add it, restart, and queued deliveries
succeed on the next attempt instead of needing manual replay.

### Registration-time UX sugar

Endpoint add (CLI + web UI) rejects *literal* denied IPs in the URL up front
(`http://127.0.0.1/...`) with the same message. Dial-time remains the real gate
(DNS at registration ≠ DNS at delivery).

### Observability + docs

- 1 metric: `webhook_egress_denied_total` (no per-URL/host labels — cardinality
  under attacker probing, same reasoning as M19's token-invalid metric).
- 1 audit event: `webhooks.egress_denied` with endpoint id, denied IP/host, and
  `denied_by: host|ip` plus the matched pattern.
- webhooks.md gains an "Egress policy" section; §1 deferred-items list updated.
- Existing smokes deliver to loopback receivers → they all gain
  `--webhook-allow-cidr=127.0.0.0/8`, doubling as the flag's regression test.
- Release notes flag the breaking-change default for existing
  internal-endpoint deployments.

---

## C. `bucketvcs doctor`

### Problem

Last unimplemented §35 Optional CLI item (docs/original-spec.md:1925). Scope
chosen: **read-only diagnostics only** — "check my bucket/authdb/config". The
old decomposition doc's repair/recovery ambitions (manifest reconstruction,
orphan cleanup, `--fix`) are explicitly out of scope.

### Shape

- New `doctor` subcommand in the cmd/bucketvcs/main.go switch dispatch.
- Small `internal/doctor` package: `Check{Name string, Run(ctx) Result}`,
  `Result{Status: ok|warn|fail|skip, Detail string}`. Sequential runner, one
  line per check.
- Exit codes: 0 all pass, 1 any `fail`, 2 usage error (house convention).
  `warn`/`skip` don't affect the exit code.
- `--json` emits NDJSON per check (house style).
- **Flags mirror `serve`**: the operator invocation is their serve command line
  with `serve` swapped for `doctor`. Validates config without binding ports.

### Checks (registered conditionally on which flags are set)

| Check | What it does |
|---|---|
| `storage.reachable` | open ObjectStore from `--bucket`, List under a reserved prefix |
| `storage.writable` | probe PUT → GET → DELETE under reserved key `_doctor/probe-<random>`; the only mutation doctor makes, never user data |
| `authdb.open` | open + ping the auth store (sqlite/libsql/postgres) |
| `authdb.migrations` | schema version == latest known migration (catches stale db and too-new db) |
| `config.lfs` | `--lfs` ⇒ both `--proxied-url-signing-key` + `--proxied-url-base` present (mirrors serve's startup gate) |
| `config.proxied` | proxied URI mode pairing rules, base URL parses |
| `config.hooks` | hooks enabled ⇒ bwrap present + version ≥ 0.12, or unsafe-no-sandbox explicitly set (warn) |
| `deps.git` | git CLI on PATH + version (import/export/maintenance need it) |
| `repo.<tenant>/<name>` | optional `--repo` flag: manifest loads, SchemaGate passes, sampled manifest-referenced pack/ref keys exist in the bucket (capped at 50) |

Serve's existing startup validations stay where they are — doctor *mirrors*
them: shared helpers where extraction is trivial, duplication where it isn't.
No refactor of serve's startup path in this milestone.

### Docs

New short `docs/operator-guides/doctor.md`; README command list gains `doctor`.

---

## Testing summary

- **A**: postgres conformance cascade cases (gated suite, runs in CI against pg).
- **B**: egress unit tests against a fake resolver/local listener covering
  deny-host glob matching, resolved-IP denial, allow-cidr holes, redirect
  refusal, scheme gate; loopback smoke with `--webhook-allow-cidr=127.0.0.0/8`.
- **C**: doctor unit tests per check with mock stores; smoke running doctor
  against a healthy localfs/sqlite setup (exit 0) and a deliberately broken
  config — missing LFS signing key — asserting exit 1.

## Deferred

macOS notarization (external Apple account/cert dependency); doctor repair
actions (`--fix`); per-endpoint/per-tenant egress policy; allow-host list
(rebinding-unsafe by construction); webhook delivery snapshot denormalization
(would fix orphan-endpoint pitfall — candidate for a later webhooks polish
pass).
