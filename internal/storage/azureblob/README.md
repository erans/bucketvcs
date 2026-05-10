# internal/storage/azureblob

Azure Blob Storage adapter for `storage.ObjectStore`. Canonical M7 backend (§11.1).

## Capabilities

- Strong read-after-write and read-after-delete consistency (§11.1).
- Conditional writes via `If-None-Match: *` and `If-Match: <etag>` (§12.4).
- Service SAS URLs (requires Shared Key credential; returns `ErrNotSupported` otherwise).
- Block blobs only. Multipart uploads map to `StageBlock` + `CommitBlockList`
  with `If-None-Match: *` on the commit (the §29 #8 invariant).

## Configuration

| Env var | Purpose |
|---|---|
| `BUCKETVCS_AZURE_ACCOUNT` | Storage account name |
| `BUCKETVCS_AZURE_ACCOUNT_KEY` | Shared Key (enables SAS) |
| `BUCKETVCS_AZURE_CONNECTION_STRING` | Full connection string (precedence; primary use is Azurite) |
| `BUCKETVCS_AZURE_SERVICE_URL` | Override default service URL |

## Running conformance against real Azure Blob

```bash
export BUCKETVCS_AZURE_CONTAINER=<your-container>
export BUCKETVCS_AZURE_ACCOUNT=<account>
export BUCKETVCS_AZURE_ACCOUNT_KEY=<key>
go test -count=1 -timeout=15m ./internal/storage/azureblob
```

## Running conformance against Azurite

```bash
docker compose -f docker-compose.cloud.yml up -d --wait

# Pre-create the container
docker run --rm --network host \
  mcr.microsoft.com/azure-cli:2.66.0 \
  az storage container create \
    --name bucketvcs-conformance \
    --connection-string \
    "DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;BlobEndpoint=http://127.0.0.1:10000/devstoreaccount1;"

export BUCKETVCS_AZURE_CONTAINER=bucketvcs-conformance
export BUCKETVCS_AZURE_CONNECTION_STRING="DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;BlobEndpoint=http://127.0.0.1:10000/devstoreaccount1;"

go test -count=1 -timeout=10m ./internal/storage/azureblob
```

`§29 #15 ThrottlingClassification` is skipped against Azurite (the emulator does not inject 429 responses). All other correctness and stress subtests pass.

## Documented Azurite gaps the adapter defends against

The adapter performs a `GetProperties` pre-check before `DeleteIfVersionMatches` because Azurite returns 412 (PreconditionFailed) instead of 404 (NotFound) when the target blob is absent. The pre-check translates "absent" to `ErrNotFound` correctly. Real Azure also performs the server-side conditional check; the pre-check costs one extra round-trip but is invariant across emulator and real service.
