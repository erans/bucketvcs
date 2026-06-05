# Upgrade notes

Per-release operator notes: breaking changes, new opt-in features, and
anything to check before rolling a new version. Newest first. Install
instructions live in the [README](../README.md#install); full feature docs in
the [operator guides](operator-guides/).

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
