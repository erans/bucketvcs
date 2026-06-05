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
  (or run those features in gateway-proxied mode, which needs no account key).

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

> **Metadata DB:** `--auth-db` is a local SQLite file here, independent of your
> `$STORE` container. It can also be a managed **Turso/libSQL** or **PostgreSQL**
> database, chosen by the `--auth-db` scheme — the secret always comes from the
> `BUCKETVCS_DB_AUTH_TOKEN` env var, never the command line:
>
> ```bash
> # Turso / libSQL (single node)
> export BUCKETVCS_DB_AUTH_TOKEN="<turso-database-token>"   # from: turso db tokens create
> bucketvcs serve --store="$STORE" --auth-db="libsql://<your-db>.turso.io" --addr=127.0.0.1:8080
>
> # PostgreSQL (single or multi-node; size the pool with --auth-db-max-conns, default 10)
> export BUCKETVCS_DB_AUTH_TOKEN="<postgres-password>"      # or the standard PGPASSWORD
> bucketvcs serve --store="$STORE" --auth-db="postgres://user@host:5432/dbname?sslmode=require" --addr=127.0.0.1:8080
> ```
>
> SQLite (the default) needs no setup, and all three backends are drop-in — every
> step in this guide is identical regardless of `--auth-db`.

> **Durability (optional, SQLite only):** add `--auth-db-replica=auto` to continuously
> replicate the authdb into the `--store` bucket (~1s RPO) and restore it automatically
> on boot — see [authdb replication](operator-guides/authdb-replication.md), and
> [authdb hosting](operator-guides/authdb-hosting.md) for choosing between SQLite,
> Turso, and PostgreSQL.

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
[azureblob adapter README](../internal/storage/azureblob/README.md)

**Large files (Git LFS):** supported on this backend. Direct-mode LFS downloads
use Shared-Key SAS, so they need the **account key** (§2); with managed-identity
auth, run LFS in gateway-proxied mode instead.
