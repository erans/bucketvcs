# Upgrade notes

Per-release operator notes: breaking changes, new opt-in features, and
anything to check before rolling a new version. Newest first. Install
instructions live in the [README](../README.md#install); full feature docs in
the [operator guides](operator-guides/).

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
