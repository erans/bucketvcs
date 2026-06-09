# Observability overview (operator guide)

This guide is the single answer to **"where do bucketvcs logs, metrics, and
audit events live, and how do I read them?"** It maps the three signals
bucketvcs emits to where each one lands, gives the exact slog shapes so you can
parse them, and indexes the per-feature observability sections in the other
operator guides.

bucketvcs does **not** expose a Prometheus `/metrics` endpoint or a built-in
query UI. Everything is emitted through Go's `log/slog`. Metrics and audit
events are slog lines you parse from the log stream; audit and usage records
are *additionally* shipped as durable objects into your bucket by
`bucketvcs serve` (see [log shipping](log-shipping.md)). The honest limitations
are spelled out in §6.

---

## 1. The three signals

bucketvcs emits three kinds of structured output. All three travel over the
same `log/slog` default logger, which `bucketvcs serve` installs as a
**`TextHandler` writing to stderr** (`time=… level=… msg=… k=v`). CLI
subcommands log to stderr the same way.

### (a) Application logs

Ordinary operational log lines — startup banners, warnings, errors, request
diagnostics. They are slog records with a human `msg` and whatever attributes
the call site attached:

```
time=2026-06-05T21:30:45.123Z level=INFO msg="authdb opened backend=postgres"
```

**Where they go:** stderr only. Persisting stderr is your environment's job —
journald (`journalctl -u bucketvcs`), a container log driver, or a file
redirect. bucketvcs does not write a log file itself.

### (b) Metrics

Metrics are slog records whose **message is the literal string `metric`**, with
the metric name and value carried as attributes. They are **not** Prometheus —
there is no scrape endpoint. You parse them out of the same log stream:

```
time=2026-06-05T21:30:45.123Z level=INFO msg=metric metric_name=policy_refs_check_total value=1 outcome=blocked_force_push
```

Counters are **cumulative** unless a specific feature guide says otherwise
(gauges and point-sample histograms are called out per metric in those guides).

> **Attribute-key note.** The metric *name* is always carried under the
> **`metric_name`** attribute, across every subsystem; `value` is always
> present too. (Some subsystems previously emitted the name under `name` —
> this was normalized to `metric_name` everywhere in this release.)

### (c) Audit events

Audit events are slog records that carry the boolean attribute **`audit=true`**
and a string attribute **`event`** whose value equals the record's `msg`. They
record *who did what*: pushes accepted, policy rejections, token rotations, LFS
transfers, webhook deliveries, OIDC exchanges, admin actions.

```
time=2026-06-05T21:30:45.123Z level=INFO msg=policy.ref.rejected audit=true event=policy.ref.rejected tenant=acme repo=site refname=refs/heads/main reason="non-fast-forward push blocked" actor=alice
```

**Where they go:** stderr (always), **and** — for audit events emitted by a
running `bucketvcs serve` — durably to `sys/logs/activity/` in the store via
[log shipping](log-shipping.md), which is **on by default**. Usage / metering
records (bytes and durations for fetches, pushes, LFS transfers, bundle / pack
serves) ship to `sys/logs/usage/`.

**Not every audit event ships.** The activity stream contains exactly the audit
events produced *inside `serve`*. Audit events whose only emitter is a one-shot
CLI subcommand run in their own process with no shipping tap installed, so they
reach stderr only:

- `gc.mark.completed` / `gc.sweep.completed` (`bucketvcs gc`)
- `maintenance.started` / `maintenance.completed`, `bundle.generated` /
  `bundle.retired` (`bucketvcs maintenance`)
- `lfs.gc.mark` / `lfs.gc.sweep` (`bucketvcs gc --lfs`)
- `lfs.quota.reconcile` (`bucketvcs quota reconcile`)
- `repo.renamed` (`bucketvcs repo rename`)

To capture those durably, scrape the subcommand's stderr (e.g. redirect the
cron job's output to the same aggregator as `serve`). The full shipped-vs-CLI
split is enumerated in [log shipping §1.1](log-shipping.md#11-the-two-streams).

---

## 2. Where to look

| Question | Where |
|---|---|
| Live tail of everything (warnings, errors, requests) | stderr of the process (`journalctl -u bucketvcs`, container logs) |
| Durable audit trail — who pushed / rejected / rotated, from `serve` | `sys/logs/activity/` in the store (gzipped NDJSON) — [log shipping](log-shipping.md) |
| Billing / usage — bytes and durations per tenant/repo | `sys/logs/usage/` in the store (gzipped NDJSON) — [log shipping §6](log-shipping.md#6-consuming-the-logs) |
| A specific feature's metric names | that feature's guide, §observability (index in §4 below) |
| GC / maintenance audit + records | stderr of the CLI run, plus stored mark/sweep records under `tenants/.../gc/` — **not** shipped (see [gc.md §7](gc.md#7-reading-mark-and-sweep-records-for-post-incident-analysis), [log shipping §1.1](log-shipping.md#11-the-two-streams)) |
| Log-shipper health (drops, upload errors) | `shiplog_*` metrics on stderr — [log shipping §7](log-shipping.md#7-metrics) |

A "live tail" question is always answered at stderr. A "what happened last
week" question is answered from `sys/logs/` (durable) — provided the event was
emitted by `serve` and shipping was on.

---

## 3. Conventions reference

### 3.1 Slog shapes at a glance

| Signal | `msg` | Distinguishing attrs |
|---|---|---|
| Application log | human string | (none reserved) |
| Metric | `metric` | `metric_name` + `value` (+ label attrs) |
| Audit event | the event name | `audit=true` + `event=<same name>` |

The default console format is slog **text** (`k=v`). If you wire a JSON handler
(or after shipping, where records are serialized as JSON), the same record
looks like:

```json
{"time":"2026-06-05T21:30:45.123Z","level":"INFO","msg":"policy.ref.rejected","audit":true,"event":"policy.ref.rejected","tenant":"acme","repo":"site","refname":"refs/heads/main","reason":"non-fast-forward push blocked","actor":"alice"}
```

A shipped activity record drops the `audit` flag and keeps the event + attrs
(`{ts, level, event, ...attrs}`); see the worked example in
[log shipping §1.1](log-shipping.md#11-the-two-streams).

### 3.2 Grep / jq recipes

The deep recipes already live in two places — link to them rather than
duplicating:

- Reading the **shipped** gzipped-NDJSON streams (per-tenant usage totals,
  tailing the activity stream for one repo): [log shipping §6](log-shipping.md#6-consuming-the-logs).
- Filtering **live** audit events out of a JSON stderr stream by event prefix:
  [bundles §8.4](bundles.md#84-example-slog-grep-recipes).

A first cut against a JSON stderr stream:

```bash
# All audit events, live:
jq -c 'select(.audit == true)' < bucketvcs.json

# One metric, aggregated by a label (text stderr):
grep 'msg=metric' bucketvcs.log | grep 'metric_name=policy_refs_check_total' \
  | sed -E 's/.*outcome=([^ ]+).*/\1/' | sort | uniq -c
```

---

## 4. Per-feature observability index

Every feature documents its own metrics and audit events. This table links each
guide's observability section.

| Feature | Metrics / audit section |
|---|---|
| Bundle & pack acceleration | [bundles §8](bundles.md#8-observability-reference) |
| Git LFS | [lfs §6](lfs.md#6-observability-reference) |
| Build triggers | [build-triggers §9](build-triggers.md#9-operations-and-observability) |
| Webhooks | [webhooks §7](webhooks.md#7-observability) |
| Repositories (rename / aliases) | [repositories §6](repositories.md#6-observability) |
| Protected refs / paths / hooks | [hooks-policy §5](hooks-policy.md#5-observability) |
| OIDC token exchange | [oidc §7](oidc.md#7-observability) |
| Web UI (login, browse, admin) | [web-ui §9](web-ui.md#9-observability) |
| Garbage collection | [gc §7](gc.md#7-reading-mark-and-sweep-records-for-post-incident-analysis) (CLI-emitted, **not** shipped) |
| Maintenance | [maintenance §6](maintenance.md#6-json-output-schema) (CLI-emitted, **not** shipped) |
| Multi-node | [multinode §7](multinode.md#7-verifying-your-deployment) |
| Multi-region read replicas | [multi-region §6](multi-region.md#6-monitoring) |
| authdb replication | [authdb replication §8](authdb-replication.md#8-metrics-and-audit-events) |
| Log shipping (the shipper itself) | [log shipping §7](log-shipping.md#7-metrics) |

> BYOB has no dedicated metrics/audit section; its observability is the doctor
> `byob.bindings` check and the bind/verify CLI — see
> [byob §7](byob.md#7-monitoring-and-verification).

---

## 5. Where durable shipping lands (multi-tenant and multi-region)

Log shipping always targets the **operator's system store** — the bucket passed
to `serve` as `--store` — under the reserved `sys/logs/` prefix. Two
consequences worth stating precisely:

- **BYOB.** Even when tenants bind their own buckets, logs ship to the operator
  `--store`, **never** to a tenant bucket. See [byob](byob.md).
- **Multi-region.** A replica gateway is started with `--store` pointing at its
  **regional** bucket and `--replica-of` pointing at the canonical bucket. The
  shipper is wired to the raw operator store (`--store`), constructed *before*
  the replica fallback composition — so a replica ships its own activity and
  usage records into its **regional** bucket's `sys/logs/`, not the write
  region's. Each region therefore holds the log timeline for the traffic it
  served; to reconstruct a global picture, collect `sys/logs/` from every
  regional bucket (plus the write region) and merge on `ts`. See
  [multi-region](multi-region.md).

---

## 6. Limitations and roadmap

- **No Prometheus endpoint.** There is no `/metrics` scrape target. Metrics are
  slog lines; translate them to your metrics backend (Loki/Vector/Promtail can
  parse the stream) if you want time series and alerting.
- **No querying UI.** Shipped logs are durable objects, not a searchable index.
  Bring your own query engine — the `sys/logs/` layout is Hive-style date
  partitions of gzipped NDJSON and loads directly into Athena / BigQuery /
  DuckDB external tables ([log shipping §6](log-shipping.md#6-consuming-the-logs)).
- **CLI-emitted audit events are not shipped.** `gc.*`, `maintenance.*`,
  `lfs.gc.*`, `lfs.quota.reconcile`, and `repo.renamed` reach stderr only,
  because their emitters run outside `serve`. Scrape those subcommands' stderr
  for a durable trail ([log shipping §1.1](log-shipping.md#11-the-two-streams)).
- **Metrics are log lines.** A `kill -9` can lose the OS-buffer tail of the
  active spool file (no fsync-per-event); the durable streams are an audit
  trail, not a write-ahead log ([log shipping §4](log-shipping.md#4-delivery-semantics)).
