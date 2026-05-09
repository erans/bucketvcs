# internal/storage/gcs

Google Cloud Storage adapter for `storage.ObjectStore`. Canonical M7 backend (§11.1).

## Capabilities

- Strong read-after-write and read-after-delete consistency (§11.1).
- Conditional writes via `ifGenerationMatch` (§12.2).
- v4 signed URLs (`gstorage.SigningSchemeV4`).
- Resumable uploads model `MultipartUpload`. Parts are buffered in
  adapter memory and flushed in order on `CompleteMultipartIfAbsent`.

## Configuration

| Env var | Purpose |
|---|---|
| `BUCKETVCS_GCS_ENDPOINT` | Override default endpoint (CI uses fake-gcs URL) |
| `BUCKETVCS_GCS_CREDENTIALS_FILE` | Path to service-account JSON |
| `BUCKETVCS_GCS_USER_PROJECT` | Billing project for requester-pays buckets |
| `GOOGLE_APPLICATION_CREDENTIALS` | Standard ADC path (honored by SDK) |

## Running conformance against real GCS

```bash
export BUCKETVCS_GCS_BUCKET=<your-bucket>
export GOOGLE_APPLICATION_CREDENTIALS=/path/to/sa.json
go test -count=1 -timeout=15m ./internal/storage/gcs
```

## Running conformance against fake-gcs-server

```bash
docker compose -f docker-compose.cloud.yml up -d --wait
curl -fsS -X POST -H "Content-Type: application/json" \
  -d '{"name":"bucketvcs-conformance"}' \
  "http://localhost:4443/storage/v1/b?project=bucketvcs"
export BUCKETVCS_GCS_BUCKET=bucketvcs-conformance
export BUCKETVCS_GCS_ENDPOINT=http://localhost:4443/storage/v1/
export STORAGE_EMULATOR_HOST=localhost:4443
go test -count=1 -timeout=10m ./internal/storage/gcs
```

`§29 #10 SignedURL` is skipped against fake-gcs (it does not implement signing). `§29 #15 ThrottlingClassification` is also skipped (emulators have no throttle).

## Documented fake-gcs gaps the adapter defends against

The adapter performs client-side pre-checks for two operations because fake-gcs (and some other emulators) do not enforce server-side conditions correctly. Real GCS still performs the server-side check too, so behavior is identical against real GCS — at the cost of one extra `Attrs` round-trip:

- `DeleteIfVersionMatches`: client-side generation check before issuing the conditional `DELETE`.
- `CompleteMultipartIfAbsent` (large objects via resumable upload): client-side existence check before finalizing with `ifGenerationMatch=0`.
