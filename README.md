# bucketvcs

A Git-protocol-compatible version-control server backed by cloud object storage.

## Canonical storage backends

All four backends implement the `internal/storage.ObjectStore` interface and
pass the full §29 conformance suite.

| URL scheme      | Provider               | Status   |
|-----------------|------------------------|----------|
| `s3://`         | AWS S3                 | canonical (§11.1, M7) |
| `r2://`         | Cloudflare R2          | canonical (§11.1, M5) |
| `gcs://`        | Google Cloud Storage   | canonical (§11.1, M7) |
| `azureblob://`  | Azure Blob Storage     | canonical (§11.1, M7) |

Local filesystem (`localfs`) is available for development and testing.
It does not require credentials and passes the same conformance suite.

## Quick start

See [`docs/m5-cloud-quickstart.md`](docs/m5-cloud-quickstart.md) for an
end-to-end walkthrough using Cloudflare R2.

## CLI subcommands

- `bucketvcs export` — export a repository to a Git pack bundle
- `bucketvcs gc` — operator-driven garbage collection (orphan packs, unreachable packs, stale indexes, orphan tx records) per spec §25 / §43.6
- `bucketvcs import` — import a Git pack bundle into a repository
- `bucketvcs init` — initialize a new repository
- `bucketvcs inspect-manifest` — dump the current root manifest
- `bucketvcs maintenance` — operator-driven repack + commit-graph / object-map refresh per spec §15.3 (M9)
- `bucketvcs serve` — start the Git-protocol HTTPS/SSH gateway

## Documentation

- [`docs/`](docs/) — design specs, quickstart guides, milestone plans
- [`internal/gc/README.md`](internal/gc/README.md) — garbage-collection package overview
- [`internal/storage/README.md`](internal/storage/README.md) — storage
  interface contract and conformance suite
- [`internal/storage/s3compat/README.md`](internal/storage/s3compat/README.md) — AWS S3 / R2 adapter
- [`internal/storage/gcs/README.md`](internal/storage/gcs/README.md) — GCS adapter
- [`internal/storage/azureblob/README.md`](internal/storage/azureblob/README.md) — Azure Blob adapter
