# Quickstart — Azure Blob Storage

Run bucketvcs with your repositories stored in an **Azure Blob Storage**
container. This is the Azure branch of the main [Quickstart](quickstart.md); the
repository, auth, and push/clone steps are identical — only the storage setup
below is Azure-specific.

bucketvcs uses the scheme **`azureblob://<container>[/<prefix>]`** and the
[`azureblob`](../internal/storage/azureblob/README.md) adapter. Block blobs are
used exclusively.

---

## 1. Create the storage account and container

Create a storage account (if you don't have one) and a **private** container in
it. bucketvcs serves objects itself, so the container's public access level
stays **Private**.

```bash
az storage account create \
  --name mystorageacct \
  --resource-group my-rg \
  --location eastus \
  --sku Standard_LRS

az storage container create \
  --account-name mystorageacct \
  --name my-container \
  --auth-mode login \
  --public-access off
```

One container can hold many repositories; each lives under its own blob prefix.
Scope bucketvcs to a sub-path with `azureblob://my-container/some/prefix`.

## 2. Choose credentials

Two supported paths:

- **Account key (simplest; required for presigned downloads).** The account key
  lets bucketvcs mint Shared-Key SAS URLs, which the LFS and bundle/pack-URI
  *direct* download modes use. Read the key:

  ```bash
  az storage account keys list \
    --account-name mystorageacct \
    --query "[0].value" -o tsv
  ```

- **Managed identity / `DefaultAzureCredential` (keyless).** Run on an Azure VM,
  AKS, or Container Apps with a managed identity granted the **Storage Blob Data
  Contributor** role on the account/container:

  ```bash
  az role assignment create \
    --assignee <managed-identity-principal-id> \
    --role "Storage Blob Data Contributor" \
    --scope "/subscriptions/<sub>/resourceGroups/my-rg/providers/Microsoft.Storage/storageAccounts/mystorageacct"
  ```

  Note: identity-based auth covers read/write but **cannot generate Shared-Key
  SAS**. If you use LFS or bundle/pack-URI *direct* mode, use the account key
  (or run those features in gateway-proxied mode — see the LFS guide).

## 3. Place the secrets

Credentials are never put in the `--store` URL. Pick one:

**A. Account + key (env vars):**

```bash
export BUCKETVCS_AZURE_ACCOUNT="mystorageacct"
export BUCKETVCS_AZURE_ACCOUNT_KEY="<account-key>"
```

**B. Connection string** (bundles account + key; precedence over the above):

```bash
export BUCKETVCS_AZURE_CONNECTION_STRING="DefaultEndpointsProtocol=https;AccountName=...;AccountKey=...;EndpointSuffix=core.windows.net"
```

**C. Managed identity / `DefaultAzureCredential` (keyless):** set only the
account name; the SDK resolves a managed identity, `az login` session, or the
standard `AZURE_*` env vars automatically:

```bash
export BUCKETVCS_AZURE_ACCOUNT="mystorageacct"
```

Treat the account key / connection string like a password: source it from a
secret manager (Azure Key Vault, a Kubernetes `Secret`, a `chmod 600`
`EnvironmentFile`), not your shell history. Rotate keys periodically — managed
identity avoids long-lived secrets entirely.

Azure-specific env vars:

| Variable | Purpose |
|----------|---------|
| `BUCKETVCS_AZURE_ACCOUNT` | storage account name (**required** unless using a connection string) |
| `BUCKETVCS_AZURE_ACCOUNT_KEY` | Shared Key (enables SAS presign) |
| `BUCKETVCS_AZURE_CONNECTION_STRING` | full connection string (takes precedence) |
| `BUCKETVCS_AZURE_SERVICE_URL` | custom blob endpoint (Azurite/sovereign clouds) |

## 4. Run bucketvcs

Point `--store` at the container and follow the main Quickstart from step 3:

```bash
export STORE="azureblob://my-container"   # or azureblob://my-container/prefix
export AUTHDB="./auth.db"

# Create a repo in the container
bucketvcs init --store="$STORE" acme my-repo
bucketvcs inspect-manifest --store="$STORE" acme my-repo   # sanity check

# Start the gateway
bucketvcs serve --store="$STORE" --auth-db="$AUTHDB" --addr=127.0.0.1:8080
```

User/token/grant setup and the push/clone flow are identical to the localfs
walkthrough — see [Quickstart §4–6](quickstart.md#4-set-up-access). Only the
`--store` value changed.

## 5. Incomplete uploads

Azure automatically garbage-collects **uncommitted blocks** (from interrupted
multipart pushes) after 7 days, so no container lifecycle rule is required for
them. You may still configure lifecycle management for your own retention
policy.

---

**See also:** [main Quickstart](quickstart.md) ·
[azureblob adapter README](../internal/storage/azureblob/README.md) ·
[Git LFS](m13-lfs-operator-guide.md) (large files; account key needed for direct mode)
