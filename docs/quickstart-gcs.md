# Quickstart — Google Cloud Storage

Run bucketvcs with your repositories stored in a **Google Cloud Storage** bucket.
This is the GCS branch of the main [Quickstart](quickstart.md); the repository,
auth, and push/clone steps are identical — only the storage setup below is
GCS-specific.

bucketvcs uses the scheme **`gcs://<bucket>[/<prefix>]`** and the
[`gcs`](../internal/storage/gcs/README.md) adapter.

---

## 1. Create the bucket

Create a **private** bucket with uniform bucket-level access (no per-object
ACLs). bucketvcs serves objects itself, so the bucket never needs public access.

```bash
gcloud storage buckets create gs://my-bucket \
  --location=US \
  --uniform-bucket-level-access
```

One bucket can hold many repositories; each lives under its own object prefix.
Scope bucketvcs to a sub-path with `gcs://my-bucket/some/prefix` if you like.

## 2. Create least-privilege credentials

Create a dedicated **service account** and grant it object-level access to just
this bucket (not project-wide):

```bash
gcloud iam service-accounts create bucketvcs \
  --display-name="bucketvcs storage"

gcloud storage buckets add-iam-policy-binding gs://my-bucket \
  --member="serviceAccount:bucketvcs@PROJECT_ID.iam.gserviceaccount.com" \
  --role="roles/storage.objectAdmin"
```

`roles/storage.objectAdmin` grants get/create/delete/list on objects in the
bound bucket — exactly what bucketvcs needs, and nothing project-wide. (Avoid
the broader `roles/storage.admin`.)

Then choose **how the gateway authenticates as that service account**:

- **Keyless (preferred in production):** run on GCE/GKE/Cloud Run with the
  service account attached, or use Workload Identity. No key file — bucketvcs
  picks up Application Default Credentials (ADC) from the metadata server.
- **JSON key (simplest off-GCP):** mint a key file:

  ```bash
  gcloud iam service-accounts keys create bucketvcs-key.json \
    --iam-account=bucketvcs@PROJECT_ID.iam.gserviceaccount.com
  ```

## 3. Place the secrets

bucketvcs uses **Application Default Credentials** — credentials are never put
in the `--store` URL. Pick one:

**A. JSON key file:**

```bash
export GOOGLE_APPLICATION_CREDENTIALS="/etc/bucketvcs/bucketvcs-key.json"
# or the bucketvcs-specific alias:
export BUCKETVCS_GCS_CREDENTIALS_FILE="/etc/bucketvcs/bucketvcs-key.json"
```

**B. Keyless (GCE/GKE/Cloud Run / Workload Identity):** set nothing — ADC is
resolved from the attached service account automatically.

Protect any key file (`chmod 600`, owned by the bucketvcs user) and source it
from a secret manager in production — e.g. a Kubernetes `Secret` mounted as a
file, or GCP Secret Manager written to disk at boot. Treat the JSON key like a
password; rotate it periodically (keyless avoids this entirely).

GCS-specific env vars (all optional):

| Variable | Purpose |
|----------|---------|
| `GOOGLE_APPLICATION_CREDENTIALS` | ADC key file (standard Google var) |
| `BUCKETVCS_GCS_CREDENTIALS_FILE` | key file (bucketvcs alias for the above) |
| `BUCKETVCS_GCS_USER_PROJECT` | billing/quota project for requester-pays buckets |
| `BUCKETVCS_GCS_ENDPOINT` | custom endpoint (emulators only) |

## 4. Run bucketvcs

Point `--store` at the bucket and follow the main Quickstart from step 3:

```bash
export STORE="gcs://my-bucket"         # or gcs://my-bucket/prefix
export AUTHDB="./auth.db"

# Create a repo in the bucket
bucketvcs init --store="$STORE" acme my-repo
bucketvcs inspect-manifest --store="$STORE" acme my-repo   # sanity check

# Start the gateway
bucketvcs serve --store="$STORE" --auth-db="$AUTHDB" --addr=127.0.0.1:8080
```

> **Metadata DB:** `--auth-db` is a local SQLite file here, independent of your
> `$STORE` bucket. It can also be a managed **Turso/libSQL** or **PostgreSQL**
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

GCS automatically expires **incomplete resumable uploads** after 7 days, so no
bucket lifecycle rule is required for them (unlike S3/R2). You may still add a
lifecycle rule for your own retention policy.

---

**See also:** [main Quickstart](quickstart.md) ·
[gcs adapter README](../internal/storage/gcs/README.md)

**Large files (Git LFS):** supported on this backend — LFS objects are served
via presigned URLs, which the service account above already permits.
