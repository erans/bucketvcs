# Quickstart — Amazon S3

Run bucketvcs with your repositories stored in an **Amazon S3** bucket. This is
the S3 branch of the main [Quickstart](quickstart.md); the repository, auth, and
push/clone steps are identical — only the storage setup below is S3-specific.

bucketvcs uses the scheme **`s3://<bucket>[/<prefix>]`** and the
[`s3compat`](../internal/storage/s3compat/README.md) adapter (the same adapter
serves Cloudflare R2 and MinIO via an endpoint override).

---

## 1. Create the bucket

Create a **private** bucket in the region closest to your gateway. Keep "Block
all public access" **on** — bucketvcs serves objects itself (or via presigned
URLs); the bucket never needs to be public.

```bash
aws s3api create-bucket \
  --bucket my-bucket \
  --region us-east-1 \
  --create-bucket-configuration LocationConstraint=us-east-1
# (omit --create-bucket-configuration for us-east-1 on older CLIs)
```

You can use a single bucket for many repositories; each repo lives under its own
key prefix automatically. Optionally scope bucketvcs to a sub-path with
`s3://my-bucket/some/prefix`.

## 2. Create least-privilege credentials

Create an IAM user (for static keys) or role (for instance/EKS roles) limited to
this one bucket.

**Create the user:**

```bash
aws iam create-user --user-name bucketvcs
```

**Attach a least-privilege policy.** Save the following as
`bucketvcs-s3-policy.json` (replace `my-bucket` with your bucket name):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "BucketvcsObjects",
      "Effect": "Allow",
      "Action": [
        "s3:GetObject",
        "s3:PutObject",
        "s3:DeleteObject",
        "s3:AbortMultipartUpload"
      ],
      "Resource": "arn:aws:s3:::my-bucket/*"
    },
    {
      "Sid": "BucketvcsList",
      "Effect": "Allow",
      "Action": [
        "s3:ListBucket",
        "s3:ListBucketMultipartUploads"
      ],
      "Resource": "arn:aws:s3:::my-bucket"
    }
  ]
}
```

`AbortMultipartUpload` / `ListBucketMultipartUploads` are needed because large
objects upload in multipart chunks. Attach the policy inline to the user:

```bash
aws iam put-user-policy \
  --user-name bucketvcs \
  --policy-name bucketvcs-s3 \
  --policy-document file://bucketvcs-s3-policy.json
```

**Generate an access key** — this is the only time the secret is shown, so copy
both values now:

```bash
aws iam create-access-key --user-name bucketvcs
```

The `AccessKeyId` and `SecretAccessKey` from the output become
`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` in step 3.

**Prefer keyless in production:** attach the same policy to an **EC2 instance
role**, **ECS task role**, or **EKS IRSA** role instead of minting a static key
— then skip the `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` env vars entirely
(step 3). (To attach the policy to a role instead, save it as a managed policy
with `aws iam create-policy --policy-name bucketvcs-s3 --policy-document
file://bucketvcs-s3-policy.json` and `aws iam attach-role-policy` it.)

## 3. Place the secrets

bucketvcs reads S3 credentials from the **standard AWS credential chain** —
credentials are never put in the `--store` URL. Pick one:

**A. Static access key (env vars):**

```bash
export AWS_ACCESS_KEY_ID="AKIA..."
export AWS_SECRET_ACCESS_KEY="..."
export AWS_SESSION_TOKEN="..."        # only for temporary STS credentials
export BUCKETVCS_S3_REGION="us-east-1"  # or set AWS_REGION
```

**B. Shared profile** (`~/.aws/credentials`):

```bash
export AWS_PROFILE="bucketvcs"          # or BUCKETVCS_S3_PROFILE
export BUCKETVCS_S3_REGION="us-east-1"
```

**C. Instance / task / IRSA role:** set nothing but the region — the SDK picks
up the role automatically.

In production, source these from a secret manager rather than your shell
history — e.g. a systemd unit `EnvironmentFile=`, a Kubernetes `Secret` mounted
as env vars, or AWS Secrets Manager injected at boot. Restrict the file to the
bucketvcs user (`chmod 600`).

S3-specific env vars (all optional except where noted):

| Variable | Purpose |
|----------|---------|
| `BUCKETVCS_S3_REGION` / `AWS_REGION` | bucket region (**required**) |
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | static key (omit to use a role) |
| `AWS_SESSION_TOKEN` | temporary STS credentials |
| `AWS_PROFILE` / `BUCKETVCS_S3_PROFILE` | shared-config profile name |
| `BUCKETVCS_S3_ENDPOINT` | custom endpoint (S3-compatible stores; leave unset for AWS) |

## 4. Run bucketvcs

Point `--store` at the bucket and follow the main Quickstart from step 3:

```bash
export STORE="s3://my-bucket"          # or s3://my-bucket/prefix
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

User/token/grant setup and the push/clone flow are identical to the localfs
walkthrough — see [Quickstart §4–6](quickstart.md#4-set-up-access). Only the
`--store` value changed.

## 5. Bucket lifecycle (recommended)

bucketvcs garbage collection does **not** abort *incomplete multipart uploads*
left by interrupted pushes — they linger in the bucket and consume storage. Add
an S3 lifecycle rule to abort them automatically (7 days is safe — longer than
any legitimate push). Create `lifecycle.json`:

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

Apply and verify:

```bash
aws s3api put-bucket-lifecycle-configuration \
  --bucket my-bucket --lifecycle-configuration file://lifecycle.json
aws s3api get-bucket-lifecycle-configuration --bucket my-bucket
```

If the bucket is shared with other systems, scope the rule to bucketvcs paths
with `"Prefix": "tenants/"`.

---

**See also:** [main Quickstart](quickstart.md) ·
[s3compat adapter README](../internal/storage/s3compat/README.md) ·
[Cloudflare R2 quickstart](quickstart-r2.md)

**Large files (Git LFS):** supported on this backend — LFS objects are served
via presigned URLs, which the credentials above already permit.
