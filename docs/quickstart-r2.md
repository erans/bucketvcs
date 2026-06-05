# Quickstart — Cloudflare R2

Run bucketvcs with your repositories stored in a **Cloudflare R2** bucket. This
is the R2 branch of the main [Quickstart](quickstart.md); the repository, auth,
and push/clone steps are identical — only the storage setup below is R2-specific.

bucketvcs uses the scheme **`r2://<bucket>[/<prefix>]`**, served by the
[`s3compat`](../internal/storage/s3compat/README.md) adapter (R2 is
S3-compatible). The `r2://` scheme automatically applies R2's defaults —
region `auto` and path-style addressing — so you only need to supply the
endpoint and credentials.

---

## 1. Create the bucket

Create a **private** R2 bucket (R2 buckets are private by default; do not attach
a public `r2.dev` domain or a custom public domain — bucketvcs serves objects
itself).

Dashboard: **R2 → Create bucket**. Or with Wrangler:

```bash
wrangler r2 bucket create my-bucket
```

Note your account's **S3 API endpoint**, shown on the bucket's settings page:

```
https://<account-id>.r2.cloudflarestorage.com
```

One bucket can hold many repositories, each under its own key prefix. Scope
bucketvcs to a sub-path with `r2://my-bucket/some/prefix`.

## 2. Create an R2 API token

> **Use the R2-specific token page — not the regular Cloudflare API Tokens page.**
> R2 API tokens produce **S3-compatible** credentials (an Access Key ID + a
> Secret Access Key); the account/profile *API Tokens* page produces
> general-purpose Cloudflare REST API tokens that do **not** work for S3 object
> access. Mixing the two up is the most common R2 setup mistake.

Open the R2 API Tokens page directly:

```
https://dash.cloudflare.com/?to=/:account/r2/api-tokens
```

(the `:account` placeholder resolves to your account automatically). Or navigate
there: **R2 → Overview → Manage** next to *API Tokens* (a.k.a. **Manage R2 API
Tokens**) → **Create API token**.

Configure the token:

- **Permissions:** *Object Read & Write* — sufficient for bucketvcs; avoid the
  broader *Admin Read & Write*.
- **Scope:** restrict it to this one bucket, not account-wide.
- **Token type:** an **Account** API token (valid until revoked, usable by an
  unattended server) is the right choice for a long-running gateway; a **User**
  token is tied to your personal login and stops working if your user is removed
  from the account.

Cloudflare then shows, **once**:

- an **Access Key ID**
- a **Secret Access Key**

These are S3-style credentials — bucketvcs consumes them through the standard
`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` variables. Copy them now; the
secret is not shown again. See Cloudflare's
[R2 authentication docs](https://developers.cloudflare.com/r2/api/tokens/) for
details, including creating tokens programmatically via the API.

## 3. Place the secrets

Credentials and endpoint are supplied via env vars — never embedded in the
`--store` URL:

```bash
export AWS_ACCESS_KEY_ID="<r2-access-key-id>"
export AWS_SECRET_ACCESS_KEY="<r2-secret-access-key>"
export BUCKETVCS_S3_ENDPOINT="https://<account-id>.r2.cloudflarestorage.com"
```

> **Note the variable name:** the endpoint is set via `BUCKETVCS_S3_ENDPOINT`
> (the shared S3/R2 endpoint variable), **not** `BUCKETVCS_R2_ENDPOINT`. You do
> **not** need to set a region or path-style flag — the `r2://` scheme applies
> `region=auto` and path-style addressing automatically.

In production, source these from a secret manager rather than your shell
history — e.g. a systemd unit `EnvironmentFile=`, a Kubernetes `Secret` mounted
as env vars, or Cloudflare-adjacent secret tooling. Restrict any file holding
them to the bucketvcs user (`chmod 600`), and rotate the R2 API token
periodically.

R2-relevant env vars:

| Variable | Purpose |
|----------|---------|
| `BUCKETVCS_S3_ENDPOINT` | R2 S3 API endpoint (**required** for `r2://`) |
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | R2 API token credentials (**required**) |
| `BUCKETVCS_S3_REGION` / `AWS_REGION` | optional; `r2://` already defaults to `auto` |

## 4. Run bucketvcs

Point `--store` at the bucket and follow the main Quickstart from step 3:

```bash
export STORE="r2://my-bucket"          # or r2://my-bucket/prefix
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

## 5. Bucket lifecycle (recommended)

bucketvcs garbage collection does **not** abort *incomplete multipart uploads*
left by interrupted pushes. Add an R2 lifecycle rule to abort them automatically
(7 days is safe). With Wrangler:

```bash
wrangler r2 bucket lifecycle add my-bucket \
  --abort-incomplete-multipart-upload-days 7
```

Or via R2's S3-compatible API with a `lifecycle.json`:

```json
{
  "Rules": [
    {
      "ID": "abort-incomplete-mpu",
      "Status": "Enabled",
      "Filter": { "Prefix": "" },
      "AbortIncompleteMultipartUpload": { "DaysAfterInitiation": 7 }
    }
  ]
}
```

```bash
aws s3api put-bucket-lifecycle-configuration \
  --endpoint-url "$BUCKETVCS_S3_ENDPOINT" \
  --bucket my-bucket --lifecycle-configuration file://lifecycle.json
```

**Migrating from localfs:** R2 (and S3) layouts are byte-identical to localfs.
Copy the tree with any S3-compatible tool and re-point `--store`:

```bash
aws s3 sync /var/lib/bucketvcs/ s3://my-bucket/ \
  --endpoint-url="$BUCKETVCS_S3_ENDPOINT"
bucketvcs inspect-manifest --store="r2://my-bucket" acme my-repo   # verify
```

---

**See also:** [main Quickstart](quickstart.md) ·
[s3compat adapter README](../internal/storage/s3compat/README.md) ·
[Amazon S3 quickstart](quickstart-s3.md)

**Large files (Git LFS):** supported on this backend — LFS objects are served
via presigned URLs, which the credentials above already permit.
