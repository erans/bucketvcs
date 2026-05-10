# M7 cloud backends — quickstart

This document covers the two new canonical storage backends added in M7:
Google Cloud Storage and Azure Blob Storage. For AWS S3 and Cloudflare R2,
see `docs/m5-cloud-quickstart.md`.

## Google Cloud Storage

### Prerequisites

- A GCS bucket in a region you can write to.
- A service account with `Storage Object Admin` on the bucket (Object User
  is sufficient if you do not need to set bucket-level lifecycle).
- Either `GOOGLE_APPLICATION_CREDENTIALS` pointing at the service-account
  JSON, or `BUCKETVCS_GCS_CREDENTIALS_FILE`.

### URL form

```
gcs://<bucket>[/<prefix>]
```

### Example

```bash
export GOOGLE_APPLICATION_CREDENTIALS=/path/to/sa.json

bucketvcs init --store=gcs://my-bucket/my-org/my-repo.bv
bucketvcs serve --store=gcs://my-bucket/my-org/my-repo.bv --listen=:8080

git clone http://user:$(bucketvcs token issue --user=user)@localhost:8080/my-org/my-repo.git
```

### Smoke test against fake-gcs-server

```bash
docker run -d --name fake-gcs -p 4443:4443 \
  fsouza/fake-gcs-server -scheme http -public-host localhost:4443
curl -X POST -H "Content-Type: application/json" \
  -d '{"name":"smoke"}' \
  "http://localhost:4443/storage/v1/b?project=bucketvcs"

export BUCKETVCS_GCS_ENDPOINT=http://localhost:4443/storage/v1/
export STORAGE_EMULATOR_HOST=localhost:4443
bucketvcs init --store=gcs://smoke/repo.bv
```

## Azure Blob Storage

### Prerequisites

- A storage account in a region you can write to.
- A container under that account.
- Either Shared Key (account key), DefaultAzureCredential (workload identity,
  managed identity, or `az login`), or a connection string.

### URL form

```
azureblob://<container>[/<prefix>]
```

### Example

```bash
export BUCKETVCS_AZURE_ACCOUNT=mystorageacct
export BUCKETVCS_AZURE_ACCOUNT_KEY=<key>

bucketvcs init --store=azureblob://my-container/my-org/my-repo.bv
bucketvcs serve --store=azureblob://my-container/my-org/my-repo.bv --listen=:8080
```

### Smoke test against Azurite

```bash
docker run -d --name azurite -p 10000:10000 \
  mcr.microsoft.com/azure-storage/azurite azurite-blob --blobHost 0.0.0.0
docker run --rm --network host \
  mcr.microsoft.com/azure-cli:2.66.0 \
  az storage container create --name smoke \
    --connection-string "DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;BlobEndpoint=http://127.0.0.1:10000/devstoreaccount1;"

export BUCKETVCS_AZURE_CONNECTION_STRING="DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;BlobEndpoint=http://127.0.0.1:10000/devstoreaccount1;"
bucketvcs init --store=azureblob://smoke/repo.bv
```

## Rotating CI secrets

The nightly conformance job in `.github/workflows/conformance.yml` reads
credentials from repo secrets. To rotate:

1. Generate a new key in the cloud console.
2. Update the GitHub repo secret (`Settings -> Secrets and variables -> Actions`).
3. Trigger the workflow manually via `workflow_dispatch` to confirm the new key works.
4. Revoke the old key in the cloud console.

Do not rotate via local CLI commands that print the key — keys can leak through
shell history. Generate, copy directly into the GitHub UI, then close the tab.

## Bucket lifecycle: incomplete multipart uploads

bucketvcs M8 GC does **not** clean up incomplete multipart uploads in-binary.
Per spec §33.5 this is delegated to the bucket-lifecycle branch — configure
your bucket to abort incomplete multipart uploads automatically.

For Google Cloud Storage and Azure Blob lifecycle recipes, see
[docs/m8-gc-operator-guide.md §5](m8-gc-operator-guide.md#5-bucket-lifecycle-for-incomplete-multipart-uploads-335).
