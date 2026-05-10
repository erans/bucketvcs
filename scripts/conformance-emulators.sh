#!/usr/bin/env bash
# Boot the local cloud-emulator stack, export the env vars each adapter
# expects, create test buckets/containers, and run the storage
# conformance suite against MinIO + fake-gcs-server + Azurite.
#
# Used both locally and from .github/workflows/conformance.yml.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="$REPO_ROOT/docker-compose.cloud.yml"
KEEP_UP="${BUCKETVCS_KEEP_EMULATORS:-0}"

cleanup() {
  if [[ "$KEEP_UP" != "1" ]]; then
    docker compose -f "$COMPOSE_FILE" down -v >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

echo "==> Booting emulator stack"
docker compose -f "$COMPOSE_FILE" up -d --wait

# MinIO bucket creation via mc-in-container.
echo "==> Creating MinIO bucket bucketvcs-conformance"
docker run --rm --network host \
  --entrypoint sh \
  minio/mc:RELEASE.2025-01-17T23-25-50Z \
  -c '
    mc alias set local http://localhost:9000 minioadmin minioadmin >/dev/null
    mc mb --ignore-existing local/bucketvcs-conformance
  '

# fake-gcs-server creates buckets implicitly on first PUT, but the
# conformance suite calls List before Put on the same key, so we
# pre-create the bucket explicitly.
echo "==> Creating fake-gcs bucket bucketvcs-conformance"
curl -fsS -X POST \
  -H "Content-Type: application/json" \
  -d '{"name":"bucketvcs-conformance"}' \
  "http://localhost:4443/storage/v1/b?project=bucketvcs"

# Azurite creates containers on first PUT too, but tests assume
# existence. Create via the well-known dev account.
echo "==> Creating Azurite container bucketvcs-conformance"
docker run --rm --network host \
  mcr.microsoft.com/azure-cli:2.66.0 \
  az storage container create \
    --name bucketvcs-conformance \
    --connection-string \
    "DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;BlobEndpoint=http://127.0.0.1:10000/devstoreaccount1;" \
    >/dev/null

# Export env for the Go test process. Each adapter's loadConfigFromEnv
# helper reads these.

# MinIO -> s3compat
export BUCKETVCS_S3_BUCKET=bucketvcs-conformance
export BUCKETVCS_S3_REGION=us-east-1
export BUCKETVCS_S3_ENDPOINT=http://localhost:9000
export BUCKETVCS_S3_FORCE_PATH_STYLE=true
export AWS_ACCESS_KEY_ID=minioadmin
export AWS_SECRET_ACCESS_KEY=minioadmin

# fake-gcs -> gcs
export BUCKETVCS_GCS_BUCKET=bucketvcs-conformance
export BUCKETVCS_GCS_ENDPOINT=http://localhost:4443/storage/v1/
export STORAGE_EMULATOR_HOST=localhost:4443

# Azurite -> azureblob
export BUCKETVCS_AZURE_CONTAINER=bucketvcs-conformance
export BUCKETVCS_AZURE_CONNECTION_STRING="DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;BlobEndpoint=http://127.0.0.1:10000/devstoreaccount1;"

echo "==> Running storage conformance suite"
cd "$REPO_ROOT"
go test -count=1 -timeout=10m ./internal/storage/...

echo "==> Running GC conformance suite (localfs binding)"
go test -count=1 -timeout=5m ./internal/gc/conformance/...
