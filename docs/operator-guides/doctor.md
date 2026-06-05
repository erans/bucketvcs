# bucketvcs doctor

Read-only health checks for a bucketvcs deployment. `doctor` accepts the same
flags as `serve` — take your serve command line, swap `serve` for `doctor`,
and it validates the configuration without binding any ports.

    bucketvcs doctor \
      --store s3://my-bucket/prefix \
      --auth-db /var/lib/bucketvcs/bucketvcs.db \
      --lfs=true --proxied-url-signing-key /etc/bucketvcs/urlkey --proxied-url-base https://gw.example

Output is one line per check; exit 0 when nothing failed, 1 when any check
failed, 2 on usage errors. `warn` and `skip` do not affect the exit code.
`--json` emits one NDJSON object per check (`{"check":..,"status":..,"detail":..}`).

## Checks

| Check | What it verifies |
|---|---|
| `storage.reachable` | the `--store` URL opens and a List succeeds |
| `storage.writable` | a probe object PUTs and DELETEs under the reserved `_doctor/` prefix — the only write doctor ever makes; user data is never touched. **Skipped on replicas** (`--replica-of` is set) — replicas are read-only and the probe write would be misleading |
| `authdb.open` | the auth DB exists and answers `SELECT 1` (sqlite paths are stat'ed first — doctor never creates a missing db) |
| `authdb.migrations` | applied schema version matches what this binary ships; flags both stale-db and binary-older-than-db |
| `config.lfs` | `--lfs=true` has its required `--proxied-url-signing-key` + `--proxied-url-base` |
| `config.proxied` | URI modes parse; signing key file is readable and >= 16 bytes; base URL is a valid http(s) URL |
| `config.hooks` | hooks root exists; bwrap present (warns when `--hooks-unsafe-no-sandbox=true`) |
| `config.replica` | present when `--replica-of` is set: validates `--replica-mode` is `strong-current` or `bounded-stale`; `--replica-lag-budget` is >= 30 s; `--auth-db` resolves to a postgres URL (SQLite/libSQL rejected for replicas) |
| `deps.git` | git CLI on PATH (import/export/maintenance shell out to it) |
| `replica.canonical` | present when `--replica-of` is set: opens the canonical store URL and confirms a List succeeds — verifies the canonical bucket is reachable from this region |
| `repo.<t>/<n>` | with `--repo tenant/name`: manifest loads, schema gate passes, up to 50 manifest-referenced pack/ref-shard keys exist in the bucket |

`doctor` applies no migrations and repairs nothing; it observes. Repair
tooling (`--fix`) is a possible future extension on the same check framework.
