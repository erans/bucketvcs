# M5 Quickstart: Running bucketvcs against Cloudflare R2

This guide walks through configuring `bucketvcs` to use Cloudflare R2
as canonical storage. Replace `<...>` placeholders with your account
values.

## 1. Provision an R2 bucket

In the Cloudflare dashboard:

1. Create an R2 bucket (any name, e.g. `bucketvcs-prod`).
2. Mint an R2 access key. Copy the access-key-id and secret.
3. Note the S3 endpoint: `https://<account-id>.r2.cloudflarestorage.com`.

## 2. Initialize a repo on R2

```bash
export BUCKETVCS_R2_ENDPOINT=https://<account-id>.r2.cloudflarestorage.com
export AWS_ACCESS_KEY_ID=<r2-access-key-id>
export AWS_SECRET_ACCESS_KEY=<r2-secret>

bucketvcs init --store=r2://bucketvcs-prod acme my-repo
bucketvcs inspect-manifest --store=r2://bucketvcs-prod acme my-repo
```

## 3. Serve the gateway

```bash
bucketvcs serve --store=r2://bucketvcs-prod \
  --auth-db=./auth.sqlite --listen=:8080
```

## 4. Run M3 protocol tests against the live bucket

```bash
git clone http://localhost:8080/acme/my-repo.git /tmp/clone-test
```

## 5. Migrating existing repo data from localfs

R2 layouts are bit-identical to localfs layouts. Use any S3-compatible
sync tool, for example:

```bash
aws s3 sync /var/lib/bucketvcs/ s3://bucketvcs-prod/ \
  --endpoint-url=https://<account-id>.r2.cloudflarestorage.com
```

After migration, point `--store` at `r2://bucketvcs-prod` and verify
with `bucketvcs inspect-manifest`.

## Bucket lifecycle: incomplete multipart uploads

bucketvcs M8 GC does **not** clean up incomplete multipart uploads in-binary.
Per spec §33.5 this is delegated to the bucket-lifecycle branch — configure
your bucket to abort incomplete multipart uploads automatically.

For AWS S3 and Cloudflare R2 lifecycle recipes, see
[docs/m8-gc-operator-guide.md §5](m8-gc-operator-guide.md#5-bucket-lifecycle-for-incomplete-multipart-uploads-335).

> See also [`docs/m9-maintenance-operator-guide.md`](m9-maintenance-operator-guide.md) for the recommended `bucketvcs maintenance` scheduling alongside `bucketvcs gc`.
