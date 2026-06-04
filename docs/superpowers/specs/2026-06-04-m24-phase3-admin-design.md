# M24 Web UI Phase 3 — manage (admin) design

Date: 2026-06-04
Status: approved (brainstorm 2026-06-04)
Predecessors: Phase 1 chassis+identity (`2026-06-01-m24-web-ui-phase1-chassis-identity-design.md`),
Phase 1.5 OIDC login, Phase 2 code browse (+ polish pass).

## 1. Goal

Complete M24 by making bucketvcs administrable from the browser: CSRF-protected
settings forms wrapping the existing stores and services. Three audiences in one
phase:

- **Self-service** — any logged-in user manages their own tokens, SSH keys, and
  password.
- **Repo settings** — repo admins manage a repo's visibility, grants, deploy keys,
  webhooks, and protected refs/paths.
- **Instance admin** — global admins manage users, repos, quotas, and Tier 3 hooks.

No new stores, no schema migrations: every operation already exists on
`*sqlitestore.Store`, `*webhooks.Service`, the M14/M16 policy services,
`*hooks.Store`, or the quota service. Phase 3 is the form layer plus a deliberate
audit story.

## 2. Page map

```
/settings                      self-service (any logged-in user)
  /settings           profile: name, email, admin badge; change-password form
  /settings/tokens    list (id prefix, label, scopes, expiry, state) + create/revoke/rotate
  /settings/keys      SSH keys: list + add + revoke

/{tenant}/{repo}/settings      repo settings (repo-admin perm OR global admin)
  /settings           general: public toggle; tenant LFS usage/cap (read-only);
                      danger zone: rename (repo-admin+), delete (global admin only)
  /settings/access    user grants (add/change/revoke read|write|admin) + deploy SSH keys
  /settings/webhooks  endpoints (add/enable/disable/rotate-secret/remove) +
                      per-endpoint deliveries view with replay
  /settings/policy    protected-ref rules + protected-path rules (add/remove)
  /settings/hooks     rendered for global admins only (see §3)

/admin                         instance admin (global admin only)
  /admin/users        list, create (name, optional initial password, admin flag),
                      disable/enable, set-email, delete
  /admin/repos        list all, register (tenant/name + in-process storage init), delete
  /admin/quotas       per-tenant usage/cap list, set/clear, reconcile
```

Navigation: navbar gains `[ settings ]` when logged in and `[ admin ]` when
`IsAdmin`; repo browse pages gain a `settings` link when the viewer is repo-admin+.

## 3. Authorization model

- `requireUser`: anonymous → 302 `/login?next=<path>` (next validated local, as today).
- **Repo settings**: global admin OR the existing repo-level `admin` permission on
  (tenant, repo), checked per request via `LookupRepoPerm`. This is the first place
  the repo `admin` perm means something web-side. Unauthorized → **uniform 404**
  (same anti-enumeration stance as browse/`GetVisibleRepo`).
- **`/admin/*` and the hooks tab**: `Session.IsAdmin` required; non-admin → 404.
- **Hooks are global-admin only by design**: an M20 hook registration points at a
  script in the operator's `--hooks-dir` and executes server-side on every push.
  Letting a repo-admin attach operator scripts to their repo is privilege
  escalation. Repo admins do not see the tab.
- **Quotas are global-admin only by design**: M13.5 quotas are per-*tenant* LFS byte
  caps that constrain what the operator pays for; a repo-admin raising their own
  tenant's cap would defeat them. Repo settings show the tenant's usage/cap
  read-only; set/clear/reconcile live under `/admin/quotas`.
- **Repo delete in the UI is global-admin only and never purges storage**
  (`--purge-storage` remains CLI-only). Rename is repo-admin+ (rename shipped in M21).
- `Session.IsAdmin` is joined fresh from `users` at session lookup, so admin
  revocation takes effect on the next request. Disabled users' sessions must fail
  lookup (believed already true from Phase 1; the plan verifies and adds a
  regression test).
- **Password change hides itself for OIDC-only users** (NULL password hash — there is
  no current password to verify; the CLI remains the bootstrap path). Password change
  requires current password + new + confirm.

## 4. Architecture (Approach A — extend the existing pattern)

Phase 1/2 established: `internal/web` declares consumer-side interfaces;
concrete stores/services satisfy them structurally; serve.go wires. Phase 3
extends that, adding no new layers.

- `web.DataStore` gains the sqlitestore-backed admin methods: users
  (create/list/disable/enable/delete/set-email/set-password), tokens
  (create/list/revoke/rotate/resolve-prefix), SSH keys (add/list/revoke, user +
  deploy), repos (register-if-new/list/delete/rename/set-public), grants
  (grant/revoke/lookup-perm). Still satisfied by `*sqlitestore.Store`.
- `web.Deps` gains small consumer-declared interfaces per domain service:
  - `Webhooks` — endpoint CRUD + enable/disable + rotate-secret + delivery
    list/show/replay (satisfied by `*webhooks.Service`).
  - `PolicyRefs`, `PolicyPaths` — add/list/remove (M14/M16 services).
  - `Hooks` — add/list/remove/set-enabled (`*hooks.Store`).
  - `Quotas` — set/show/clear/reconcile.
  - `RepoInit` — in-process repo storage init used by `/admin/repos` register. The
    CLI currently shells out to `bucketvcs init`; the plan extracts the underlying
    init into a function callable by both (serve already holds the ObjectStore).
- Any nil service ⇒ that tab/page renders a "not enabled on this server" notice
  instead of forms (mirrors `Content == nil` disabling browse).
- Webhook (M15) emission reuses the existing emitters; nothing new invented:
  `repo.created` on register, `repo.deleted` on delete, `repo.renamed` on rename.

## 5. Form mechanics

- Every settings GET issues a CSRF token (`issueCSRF`, the login pattern); every
  form embeds `csrf_token`; every POST runs `checkCSRF` first.
- POST flow: CSRF → authorize → validate → store call → audit emit → flash →
  **303 redirect** back to the tab (POST-redirect-GET; pages work without JS).
- One handler per action with explicit paths (`POST /settings/tokens/create`,
  `POST /{t}/{r}/settings/access/grant`, ...). No multiplexed `action=` fields.
- **Flash messages**: one-shot `bvcs_flash` cookie (HttpOnly, short plain text,
  rendered escaped in the base layout, cleared on render) for confirmations and
  validation errors.
- **Secret-revealing exception**: token create/rotate and webhook endpoint
  add/rotate-secret render the result page **directly** (no redirect) so the
  secret can be shown exactly once, with a "this won't be shown again" notice.
  A refresh re-submits; the page warns, and CSRF makes blind re-POSTs
  unexploitable. Secrets never pass through the flash cookie.
- Destructive actions (repo delete, user delete, endpoint remove, hook remove)
  require a type-the-name confirm field validated server-side — no JS-only
  confirms.
- Templates: a shared `settingsnav` partial (tabs) + one template per page
  (`settings_tokens.html`, `repo_settings_access.html`, `admin_users.html`, ...),
  reusing the existing box/table styling and `_partials.html` conventions.

## 6. Audit & webhooks

Web handlers emit audit events for every state change (the way `auth.session.*`
already works). Actor = session user; all events carry `source=web` plus target
attrs (tenant/repo/user/token-id-prefix/etc. as applicable). Existing event names
are reused where they exist; the rest follow the established namespaces:

| Domain | Events |
|---|---|
| users | `auth.user.created` `.disabled` `.enabled` `.deleted` `.email_set` `.password_changed` |
| tokens | `auth.token.created` `.revoked`; reuse `auth.token.rotated` (M17) |
| SSH keys | `auth.sshkey.added` `.revoked` (user + deploy distinguished by a `kind` attr) |
| repos | `repo.created` `.deleted` `.renamed` `.public_set`; `repo.grant.added` `.removed` |
| webhooks | `webhooks.endpoint_created` `_removed` `_enabled` `_disabled`; reuse `webhooks.endpoint_secret_rotated` (M15.1); `webhooks.delivery_replayed` |
| policy | `policy.ref.rule_added` `_removed`; `policy.path.rule_added` `_removed` |
| hooks | `policy.hook.added` `_removed` `_enabled` `_disabled` (M20 namespace) |
| quotas | `quota.set` `.cleared` `.reconciled` |

CLI audit gaps stay as-is (out of scope); moving emission into the stores was
considered and rejected for this phase (large, risky refactor touching every
store for one consumer's benefit).

## 7. Observability

- New counter `web_admin_actions_total{domain,action,result}` with
  `result ∈ ok|invalid|denied|error`. Existing `web_requests_total` covers traffic.
- No new flags. Repo registration, hooks, quotas, webhooks availability follow
  what serve already wired; nil services degrade to notice pages.

## 8. Error handling

- Validation failure → flash the message, 303 back to the form (input not
  preserved — acceptable for these short forms).
- Authz failure → uniform 404. CSRF failure → 403 (matches login).
- Store/service error → 500 error page + `ERROR` log with detail; pages never
  leak sqlite/filesystem detail.
- Secret-revealing renders buffer the template before writing (no partial-200),
  per the Phase 2 `renderBrowse` convention.

## 9. Testing

- Per-form httptest matrix: missing/bad CSRF → 403; anon → 302 login; non-admin →
  404; repo-admin allowed on their repo's settings but 404 on `/admin` and on the
  hooks tab; happy path mutates fake store, emits the expected audit event, 303s.
- Secret shown exactly once (POST response contains it; subsequent GET does not).
- Disabled-user session rejected at lookup (regression test).
- Nil quota/hooks/webhooks service → notice page, no panic.
- Type-the-name confirm: wrong name → flash error, no mutation.
- End-to-end localfs smoke: create user → grant → toggle public → add webhook →
  add protected ref → token create/rotate — all via HTTP forms.
- Operator guide gains a Phase 3 section; a Phase 3 smoke script joins the
  existing ones under `scripts/`.

## 10. Out of scope (deferred)

Self-registration; tenant-admin role; per-repo quotas; audit-log viewer UI;
session list/revocation UI; OIDC identity link/unlink UI; hook script content
editing (scripts always live on the operator filesystem); repo transfer between
tenants; JSON API; webhook delivery payload inspector beyond existing show
fields; storage purge from the UI.
