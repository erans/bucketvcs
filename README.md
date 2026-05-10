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

## Documentation

- [`docs/`](docs/) — design specs, quickstart guides, milestone plans
- [`internal/storage/README.md`](internal/storage/README.md) — storage
  interface contract and conformance suite
- [`internal/storage/s3compat/README.md`](internal/storage/s3compat/README.md) — AWS S3 / R2 adapter
- [`internal/storage/gcs/README.md`](internal/storage/gcs/README.md) — GCS adapter
- [`internal/storage/azureblob/README.md`](internal/storage/azureblob/README.md) — Azure Blob adapter
