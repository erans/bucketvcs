# Upgrade notes

Per-release operator notes: breaking changes, new opt-in features, and
anything to check before rolling a new version. Newest first. Install
instructions live in the [README](../README.md#install); full feature docs in
the [operator guides](operator-guides/).

## Unreleased (adversarial-review hardening)

Behavior changes from the security/correctness review. All live behind
explicit flags with safe defaults; the breaking items are the four default
flips below.

- **OIDC email auto-link is now opt-in (breaking).** `--oidc-login-allow-email-link`
  defaults to `false`. First-time OIDC logins no longer auto-link by verified
  email (TOFU); operators must pre-link identities (issuer, subject) or pass
  `--oidc-login-allow-email-link=true` if they trust their IdP's email
  verification. Existing linked users are unaffected.
- **Web sessions now have an absolute lifetime (breaking).** New
  `--ui-session-max-age` (default 24h) caps total session lifetime from
  creation; the sliding `--ui-session-ttl` can no longer extend a session
  past it. Active sessions older than the cap are revoked on next request.
  Raise the flag if 24h is too short for your users.
- **Short `--retention` now requires acknowledgment (breaking).**
  `bucketvcs gc --retention` below 4h (`gc.MinRetention`, covering the
  longest default signed-URL TTL) is rejected unless `--allow-short-retention`
  is passed. Retention between 4h and 24h still warns. If you raised
  presigned-URL TTLs above 4h, raise retention to match.
- **Build-trigger URLs must be https unless acknowledged (breaking).**
  Generic, Cloud Build, and Azure webhook triggers with `http://` URLs are
  rejected at creation and refused at delivery (no token is minted for a
  refused delivery) unless the trigger sets `allow_http` (`--allow-http` on
  the CLI, `allow_http` in `build apply` YAML). Pre-existing plaintext
  triggers stop delivering until updated.
- **Per-account auth rate limiting (new, on by default).** Failures against
  one username now accumulate across source IPs (`--auth-rate-limit-user-burst`,
  default 100, an order of magnitude above the per-IP burst to keep targeted
  lockout-DoS expensive). A successful login resets only that principal's
  bucket. Set `--auth-rate-limit-user-burst=0` for pure IP-only gating.
  New `auth_ratelimit_total{outcome="limited_user"}` log metric and
  per-bucket (`ip`/`user`) attribution on rate-limit audit lines.
- **Stable OIDC login-state HMAC key (new, opt-in).** Set
  `--oidc-login-hmac-key-file` or `BUCKETVCS_OIDC_HMAC_KEY` (>= 16 bytes) so
  OIDC logins verify on every instance and survive restarts. Without it the
  key stays ephemeral per boot (single instance only) and a warning is
  logged. A short key or missing key file fails startup closed.
- **Hooks failure mode documented; default stays fail-closed.** No behavior
  change: `--hooks-on-internal-error` still defaults to `reject`. The flag
  help now states the `allow` risk explicitly (pushes proceed unenforced when
  hook enforcement itself is broken).
- **GC now sweeps retired bundles and orphan commit markers.** Both are
  retention-gated like other categories and visible in `gc` text/JSON output
  (`bundles`, `orphan_markers`) and audit logs. A `gc.sweep.started` audit
  line now precedes deletions so a failed sweep-record write stays
  reconstructible from the mark record. Commit-marker write failures are
  logged (best-effort semantics unchanged).
- **Observable serving/usage changes.** Truncated proxied bundle/pack bodies
  are logged with expected-vs-served bytes and metered as `truncated` instead
  of `ok`. Token-usage queue drops now warn. Azure nil-ContentLength skips
  are counted per page. GCS reads use single-request metadata (no behavior
  change). `streamToFile` downloads are temp+rename with byte-count check.

## v0.6.0

No breaking changes. Two authdb schema migrations (0017 build triggers,
0018 repo aliases) apply automatically on first open, on both the sqlite and
postgres backends.

- **Build triggers (new, opt-in).** Pushes can now kick off CI: a generic
  signed webhook, Google Cloud Build, AWS CodeBuild, and Azure DevOps
  (service-hook webhook or direct pipeline run). Off unless `serve` is started
  with `--build-triggers` (connector credentials go in `--build-config`).
  Durable delivery with retries and permanent-error classification (a
  misconfigured trigger dead-letters instead of retrying forever). Manage via
  the new `bucketvcs build` CLI or the repo-admin **Triggers** tab in the web
  UI. See the [build-triggers guide](operator-guides/build-triggers.md).
- **Repo rename now leaves a redirect.** `bucketvcs repo rename` records an
  alias so clones keep working against the old name (Git over HTTPS/SSH,
  LFS, and web URLs). Registering a new repo under the old name removes the
  alias — a live repo always shadows an alias. Inspect or drop redirects with
  `bucketvcs repo alias list|remove`. See
  [repositories §4](operator-guides/repositories.md#4-rename-redirects--aliases).
- **Web UI grew an observability surface.** Global admins get `/admin/sessions`
  (view + revoke web sessions) and `/admin/audit` (browse the shipped audit
  stream with event/tenant/repo/actor/date filters); repo admins get a
  repo-scoped audit tab. Code browsing adds a compare view and per-file
  history. See [web-ui §10](operator-guides/web-ui.md#10-session-management-and-audit-viewer).
- **New `bucketvcs session list|revoke` CLI** — the escape hatch past the admin
  page's display cap, and session revocation without a browser. Like other
  CLI emitters, its audit line is stderr-only.
- **Audit viewer paging is now bounded per page.** The viewer walks the
  `sys/logs/activity/` date partitions backward with a per-page budget instead
  of listing the whole prefix, so it stays fast on long-lived deployments. On
  sparse prefixes a filtered page can legitimately render empty with an
  `[older]` link — follow it to continue the scan. Do not place foreign
  objects under `sys/logs/activity/`: keys that don't match the `YYYY/MM/DD/`
  partition layout are treated as corruption and fail the audit page until
  deleted.

## v0.5.1

No breaking changes.

- **Metric log lines now uniformly use the `metric_name` attribute.** Previously
  the webhooks, hooks, web-UI, read-replica controller, fallback store, auth
  rate-limiter, and code-browse metrics carried the metric name under a `name`
  attribute instead. All emitters now use `metric_name` (the convention already
  used everywhere else). Any log-pipeline filters or dashboards keyed on
  `name=<metric>` for those subsystems must be updated to `metric_name=<metric>`.

## v0.5.0

No breaking changes, but a few behavior changes to be aware of.

- **Usage & activity log shipping (new, on by default).** `bucketvcs serve` now
  ships two durable NDJSON streams into the object store under the reserved
  `sys/logs/` prefix — **activity** (the `audit=true` events emitted from the
  running `serve` process) and **usage**
  (operation metering: fetch/push/LFS/bundle/pack bytes and durations),
  gzipped. This is **on by default** whenever `--store` is configured; pass
  `--log-shipping=off` to restore the previous stderr-only behavior. Tunables:
  `--log-ship-max-events` (1000), `--log-ship-interval` (15m), `--log-spool-dir`
  (state dir), `--log-spool-max-bytes` (256MB). See the
  [log-shipping guide](operator-guides/log-shipping.md).
  - **Lifecycle rule recommended.** New objects now appear under `sys/logs/`.
    Add a bucket object-lifecycle rule scoped to `sys/logs/` with a retention
    that matches how far back you need usage/audit data, the same way the
    replication guide recommends for `sys/authdb/ltx/`. (`sys/` is already
    reserved — GC never touches it.)
- **Audit taxonomy normalized.** Every genuine audit emitter across
  `policy.*`, `lfs.*`, `auth.*`, `webhooks.*`, and `hooks` (plus `repo.renamed`
  and `replica.repo.*`) now carries `audit=true` and a matching `event`
  attribute; previously many of these were untagged and never reached the
  shipped activity stream. **Caveat:** only audit events emitted *from the
  `serve` process* are shipped — the slog tap lives in `serve` alone, so
  audit events whose only emitter is a CLI subcommand (`gc.*`,
  `maintenance.*`, `lfs.gc.*`, `lfs.quota.reconcile`, `repo.renamed`) are
  **not** shipped today. See the
  [log-shipping guide §1.1](operator-guides/log-shipping.md) for the exact
  shipped-vs-CLI split. If your log pipeline filters were keyed on the *old*
  shapes (e.g.
  matching `policy.ref.rejected` or `lfs.*` only by message with no `audit`
  field), update them to key on `audit=true` / the `event` attribute.
- **Console log format changed.** To install the log-shipping tap, `serve` now
  sets a concrete `slog` `TextHandler` as the process default logger (in **both**
  shipping modes, including `--log-shipping=off`). Console lines change from the
  stdlib bridge format —
  `2026/06/05 17:43:19 INFO msg ...` — to slog's `key=value` format —
  `time=2026-06-05T17:43:19.000-07:00 level=INFO msg=... key=value`. If you parse
  `serve`'s stderr (log scrapers, alert regexes), update your patterns
  accordingly.

## v0.4.0

No breaking changes.

- **Durable authdb (new, opt-in).** The embedded SQLite authdb can replicate
  continuously into object storage (~1 s RPO) and restore itself on boot —
  enable with `--auth-db-replica=auto` →
  [replication guide](operator-guides/authdb-replication.md), and see
  [choosing an authdb backend](operator-guides/authdb-hosting.md).
- **`sys/` prefix reserved.** The top-level `sys/` prefix in the store bucket
  is now reserved for system data. If you run bucket-wide lifecycle or cleanup
  rules, scope them away from `sys/` (or follow the replication guide's
  recommendation for `sys/authdb/ltx/`).

## v0.3.0

- **Webhook egress lockdown (breaking).** Webhook deliveries to private and
  loopback addresses are now blocked by default. If your deployment delivers
  webhooks to an internal receiver, add `--webhook-allow-cidr=<network>`
  (e.g. `--webhook-allow-cidr=192.168.1.0/24`) to `bucketvcs serve`. This is a
  breaking change for any deployment targeting internal endpoints.
