# Build Triggers UI — web-ui next-phase design

Date: 2026-06-09
Status: approved (brainstorm 2026-06-09)
Predecessors: M24 Web UI Phase 1 chassis+identity, Phase 1.5 OIDC, Phase 2 code
browse (+ polish), Phase 3 manage/admin (`2026-06-04-m24-phase3-admin-design.md`).
Backend: M30 build triggers (`2026-06-07-m30-build-triggers-design.md`) + M31 Azure
(`2026-06-08-azure-build-triggers-design.md`).

This is **Phase A** of the agreed web-ui roadmap (A: build-triggers UI → B:
code-browse depth → C: observability surface → D: self-service & lifecycle). Each
phase is its own spec → plan → implementation cycle; B/C/D are out of scope here.

## 1. Goal

Give the M30/M31 build-trigger engine a browser face. Build triggers are currently
CLI/API-only; this phase adds a repo-settings **Triggers** tab so repo admins can
create, edit, enable/disable, and delete triggers across all five kinds, and inspect
per-trigger delivery history with bounded replay — all wrapped around the existing
`internal/buildtrigger` service, reusing the Phase-3 settings chassis.

Non-goals: changing how triggers *fire* or *deliver* (the M30/M31 worker is
untouched); a JSON API; managing connector credentials from the browser.

## 2. Page map

New tab slotted into the existing `reposettings.go` chassis, gated by repo
`PermAdmin`, rendered only when the trigger service is enabled on the server.

| Route | Method | Purpose |
|-------|--------|---------|
| `/{t}/{r}/settings/triggers` | GET | List triggers + "add trigger" entry point |
| `/{t}/{r}/settings/triggers/new?kind=X` | GET | Create form (full page no-JS; htmx modal fragment when JS). `#kindfields` is the per-kind fieldset, swapped on kind change. |
| `/{t}/{r}/settings/triggers/add` | POST | Create |
| `/{t}/{r}/settings/triggers/edit` | POST | Edit safe fields |
| `/{t}/{r}/settings/triggers/enable` | POST | Enable |
| `/{t}/{r}/settings/triggers/disable` | POST | Disable |
| `/{t}/{r}/settings/triggers/remove` | POST | Delete |
| `/{t}/{r}/settings/triggers/rotate-secret` | POST | Regenerate HMAC secret (generic/cloudbuild only) |
| `/{t}/{r}/settings/triggers/deliveries?trigger=<id>` | GET | Paginated delivery history |
| `/{t}/{r}/settings/triggers/deliveries/replay` | POST | Replay a recent delivery |

Tab link added to `reposettingsnav`. When the trigger service is nil/disabled the
tab renders "build triggers are not enabled on this server" (mirrors webhooks'
`if .Enabled` guard).

## 3. Authorization model

- Every handler passes the existing `canAdminRepo` (repo `PermAdmin`) gate — the same
  tier as the webhooks tab. Triggers are a repo-level CI concern; token injection
  only grants what a repo admin can already mint; URL-based kinds share the webhooks
  SSRF surface (handled at the deliverer, §7).
- All five kinds are repo-admin-managed, including connector-backed
  `codebuild`/`azurepipelines`. A repo admin may point such a trigger at an
  operator-configured connector (a shared AWS/Azure credential) **by name only** —
  they never see the credential. This is an accepted, documented boundary (operator
  owns the connector; repo admin wires their repo to it).
- Chassis-level repo-existence probe gives a uniform 404 across all tabs (Phase 3).
- CSRF token required on every POST via the existing `postGuard`.

## 4. Architecture (extend the Phase-3 settings pattern)

No new web-side packages. New file `internal/web/reposettings_triggers.go` adds
`case "triggers"` to the `reposettings.go` tab switch and the sub-route dispatch.

Reused chassis: `reposettingsnav`, `base.html`, `_partials.html`, `renderPartial`
(htmx fragments), `postGuard`/CSRF, `flash`, `renderSecretOnce` (secret-once page,
`Cache-Control: no-store`), `renderBuffered`, the `web_admin_actions_total` metric,
and the `source=web` audit pattern.

New templates:
- `reposettings_triggers.html` — list table + "add trigger" button.
- `reposettings_triggers_form.html` — create/edit form; defines the `#kindfields`
  fragment rendered standalone for htmx swaps.
- `reposettings_triggers_deliveries.html` — history table + filter + pager.

A consumer interface for the trigger service lives in `services.go` with a
compile-time assertion (Phase-3 convention), so the web layer depends on a narrow
surface, not the concrete `*buildtrigger.Service`.

**Interactivity** follows the existing `browse.go` ref-switcher precedent — htmx
progressive enhancement with a no-JS fallback:
- `+ add trigger` / `[edit]`: with htmx, `hx-get` loads the form as a `<dialog>`
  modal fragment; without JS, the link navigates to the full `/new` (or
  `/new?id=<id>` prefilled) page. Form submits normally either way.
- Kind `<select>`: `hx-get .../new?kind=X` swaps `#kindfields`; without JS the page
  reloads at `?kind=X` showing the right fieldset.

## 5. Form mechanics & per-kind fields

Shared fields: name, ref include, ref exclude, token mode (none/inject), token
scopes, token ttl (validated ≤ `buildtrigger.TokenCeiling` = 1h).

Per-kind fieldset (`#kindfields`):

| Kind | Fields | Secret behaviour |
|------|--------|------------------|
| `generic` | url (req) + optional secret | blank secret ⇒ auto-generated, shown once |
| `cloudbuild` | url (req) + optional secret | blank secret ⇒ auto-generated, shown once |
| `azurewebhook` | webhook url (req) + optional secret + optional sig-header | operator-supplied; blank = unsigned; **not** shown-once |
| `codebuild` | connector ▾ + aws region + aws project | none |
| `azurepipelines` | connector ▾ + azure project + pipeline-id | none |

- **Connector dropdown**: populated from configured connector **names** (AWS + Azure)
  read from the parsed `--build-config` at web-handler construction (static for the
  process lifetime; names only, never secrets). Empty list ⇒ "no connectors
  configured; ask your operator."
- **Secret-once**: when `add` auto-generates a secret (generic/cloudbuild, blank
  secret submitted), redirect to `renderSecretOnce`. Other kinds redirect straight to
  the list with a success flash.
- **Edit**: same form prefilled, posting to `…/edit`. Mutates name, refs, token
  mode/scopes/ttl, active. **Kind and url/secret are fixed** — the form states
  "changing kind requires delete + recreate." Validation errors re-render the form
  with a flash (no data loss).
- **Row actions**: `[deliveries] [enable|disable] [edit] [remove]`, plus
  `[rotate-secret]` **only** on generic/cloudbuild rows.
- **List "last fire" column**: derived from each trigger's most-recent delivery
  status (✓ delivered / ✗ failed-or-dead-letter / dim "never"). Fetched via a single
  batched query for the repo's triggers, not N+1.

## 6. Backend additions (`internal/buildtrigger` + CLI)

1. **`Service.Edit(ctx, id, EditInput) (Trigger, error)`** — UPDATEs
   name/ref-include/ref-exclude/token-mode/token-scopes/token-ttl/active. Validates
   name uniqueness (`ErrConflict` via `findByName`), ttl ≤ `TokenCeiling`, scope mask.
   Leaves kind/config untouched. New `EditInput` struct.
2. **`Service.RotateSecret(ctx, id) (Trigger, error)`** — regenerates `Config.Secret`
   for generic/cloudbuild; returns the new secret once; errors (`ErrInvalidInput`) on
   connector or azurewebhook kinds (no server-owned secret to rotate).
3. **`Service.ListDeliveriesPage(ctx, triggerID, status string, before time.Time, limit int)`**
   — new keyset-paginated sibling of `ListDeliveries` (`created_at < before`, tie-break
   on id, `ORDER BY created_at DESC, id DESC`). Existing `ListDeliveries` signature is
   left intact so the CLI keeps working.
4. **`Service.RecentDeliveryIDs(ctx, triggerID string, n int) ([]string, error)`** —
   latest N ids for a trigger; backs the replay-authority check (§7).
5. **Connector-names accessor** — a read-only method/struct exposing configured AWS +
   Azure connector names from the parsed build config, wired serve→web at handler
   construction.
6. **CLI parity** — `bucketvcs trigger edit --id=… [flags]` and
   `bucketvcs trigger rotate-secret --id=…`, mirroring the existing
   `trigger add/list/remove/enable/disable` shape with NDJSON output.

## 7. Deliveries, pagination & bounded replay

- **History page**: table of `created · status · attempts · last-HTTP · last-error ·
  delivered-at`, newest first, via `ListDeliveriesPage`. Status filter
  (`all/pending/in_flight/delivered/dead_letter`) carried as `?status=`.
- **Pagination**: 20 rows/page, **keyset** on `created_at` (+id tie-break) — stable
  under concurrent inserts, no total count. `[older]`/`[newer]` via the `.pager`
  bracket idiom; cursor = last row's `created_at`.
- **Bounded replay**: the `[replay]` link renders only on rows within the
  most-recent 10 (`RecentDeliveryIDs(id, 10)`). The **POST replay handler
  re-verifies** the target id is in that set before calling `ReplayDelivery` — the
  UI gate is cosmetic, the server gate is authoritative; an out-of-window POST is
  rejected with a flash. `ErrReplayInFlight` surfaces as a flash.
- **Cross-trigger/tenant guard**: the handler confirms the delivery's `trigger_id`
  resolves to a trigger in this `(tenant, repo)` before display or replay — closes the
  cross-tenant-replay class Phase 3 caught in webhook deliveries.

## 8. Audit, metrics, security

- **Audit** (`source=web`): `buildtrigger.created`, `.edited`, `.enabled`,
  `.disabled`, `.removed`, `.secret_rotated`, `.delivery_replayed`, each with
  `{tenant, repo, trigger_id, actor}`. Align with any names the M30 backend already
  emits rather than duplicating.
- **Metrics**: reuse `web_admin_actions_total{domain,action,result∈ok|invalid|error}`
  with `domain="triggers"` — no new metric.
- **SSRF / egress**: operator-supplied URLs (generic/cloudbuild/azurewebhook) share
  the webhooks surface. Outbound calls are made by the M30 **delivery worker**, where
  egress policy belongs; the UI only stores config. Implementation includes a **check**
  that the M25 egress deny-list already covers the build-trigger deliverer; if it does
  not, that is a one-line follow-up security fix tracked separately, not a v1 blocker.
- **Secret hygiene**: full `Config.Secret` is never rendered (only `SecretPreview`,
  per the model contract); generated secrets only via the once-page with `no-store`;
  connector PATs/AWS keys never reach the web layer (names only).

## 9. Error handling

- Invalid form input (bad kind fields, ttl > ceiling, bad scope, duplicate name) →
  re-render the form with a flash, `result="invalid"`, HTTP 200, no data loss.
- Service/DB errors → generic flash, `result="error"`, logged at error level; no
  internal paths leaked to the client.
- Non-admin → 403; missing repo → uniform 404; missing CSRF → 400 (chassis).
- Replay out-of-window / cross-tenant / in-flight → flash, no state change.

## 10. Testing

- **Handler tests** (`reposettings_triggers_test.go`, webhooks/Phase-3 style):
  authz (non-admin 403, missing-repo 404), CSRF rejection, create per-kind (field
  validation + secret-once redirect for generic/cloudbuild), `#kindfields` fragment
  swap, edit (kind-immutability enforced), enable/disable/remove, replay-bound
  enforcement (out-of-window POST rejected), cross-tenant delivery guard, pagination
  cursor correctness.
- **buildtrigger unit tests**: `Edit` (uniqueness/ttl/scope, kind untouched),
  `RotateSecret` (kind gating), `ListDeliveriesPage` keyset boundaries,
  `RecentDeliveryIDs`.
- **CLI tests**: `trigger edit`, `trigger rotate-secret`.
- **Smoke script**: serve with a `--build-config` defining a connector → create one
  trigger of each kind via UI POSTs → push to fire → assert a delivery row → replay
  in-window (ok) and out-of-window (rejected) → edit → disable → remove.
- **Operator guide**: add a Build Triggers UI subsection to the web-ui guide (the
  tab, per-kind fields, connector prerequisite, replay bound).

## 11. Out of scope (deferred)

Code-browse depth (roadmap B), observability surface (C), self-service & lifecycle
(D); full edit incl. kind change; managing/creating connectors from the browser; a
JSON API; webhook/trigger egress deny-list hardening beyond the §8 check; per-trigger
metrics; trigger templates/cloning.
