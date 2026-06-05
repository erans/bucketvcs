# Bring-your-own-bucket (operator guide)

This guide explains how to configure bucketvcs in bring-your-own-bucket (BYOB)
mode, where each tenant supplies their own cloud storage bucket instead of
sharing the operator's bucket. bucketvcs acts as the control plane — handling
auth, policy, webhooks, and Git protocol — while the tenant's bucket is the
data plane that holds their repositories. Objects never leave the tenant's
account; bucketvcs opens the bucket on demand using credentials the tenant
provides and you store, encrypted, in the central auth database.

---

## 1. Prerequisites

**Central auth database.** BYOB stores encrypted per-tenant credentials in the
auth database. In multi-node deployments all gateway nodes must share the same
database so they can reach the binding for any tenant. A local SQLite file is
fine for single-node deployments; PostgreSQL is required for replicas or multiple
gateway nodes:

```bash
--auth-db 'postgres://bv@central-host/bucketvcs_auth?sslmode=require'
```

**Encryption key file.** Each binding's credentials are encrypted with AES-256-GCM
before being written to the auth database. The key file must contain at least 32
bytes; only the first 32 bytes are used as the key material.

Generate a key file:

```bash
dd if=/dev/urandom bs=32 count=1 > /etc/bucketvcs/byob.key
chmod 600 /etc/bucketvcs/byob.key
```

The key file must be present and identical on every gateway node. If you lose
the key file, existing bindings cannot be decrypted — you must re-bind all
affected tenants. Treat it with the same care as a TLS private key.

Pass it to every `bucketvcs serve`, `bucketvcs gc`, `bucketvcs maintenance`,
and `bucketvcs doctor` invocation that needs to resolve tenant stores:

```bash
--byob-encryption-key /etc/bucketvcs/byob.key
```

---

## 2. Credential file formats

Create a JSON file containing the credentials for the tenant's bucket. The exact
keys depend on the storage backend.

**S3 and Cloudflare R2:**

```json
{
  "access_key_id": "AKIAIOSFODNN7EXAMPLE",
  "secret_access_key": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
  "region": "us-east-1"
}
```

For R2, add `"endpoint_url"` pointing at the R2 S3-compatible endpoint:

```json
{
  "access_key_id": "...",
  "secret_access_key": "...",
  "endpoint_url": "https://<accountid>.r2.cloudflarestorage.com",
  "region": "auto"
}
```

**Google Cloud Storage:**

```json
{
  "type": "service_account",
  "project_id": "my-project",
  "private_key_id": "key-id",
  "private_key": "-----BEGIN RSA PRIVATE KEY-----\n...\n-----END RSA PRIVATE KEY-----\n",
  "client_email": "bucketvcs@my-project.iam.gserviceaccount.com",
  "client_id": "123456789",
  "auth_uri": "https://accounts.google.com/o/oauth2/auth",
  "token_uri": "https://oauth2.googleapis.com/token"
}
```

This is the standard service account JSON file downloaded from the GCP console.

**Azure Blob Storage:**

```json
{
  "account_name": "mystorageaccount",
  "account_key": "base64encodedkey=="
}
```

**Local filesystem (development only):**

```json
{}
```

The localfs backend requires no credentials; an empty object is the correct
credential document.

---

## 3. Bind workflow

Binding a tenant attaches their bucket to their account in the auth database.
bucketvcs probes the bucket before saving the binding — if the probe fails, the
command returns an error and nothing is written.

**Step 1.** Create a credentials file for the tenant's bucket (see section 2).

**Step 2.** Run `bucketvcs tenant storage bind`:

```bash
cat > creds.json <<'EOF'
{"access_key_id": "AKIA...", "secret_access_key": "...", "region": "us-east-1"}
EOF

bucketvcs tenant storage bind \
  --auth-db 'postgres://bv@central-host/bucketvcs_auth?sslmode=require' \
  --tenant acme \
  --store 's3://acme-bucket/repos' \
  --creds-file creds.json \
  --byob-encryption-key /etc/bucketvcs/byob.key
```

**What happens:**

1. bucketvcs opens the bucket at `--store` using the credentials from
   `--creds-file` and issues a List probe. If the bucket is unreachable or the
   credentials are invalid, the command exits non-zero with a diagnostic.
2. On success, the credentials are encrypted with the key file and written to
   the `storage_bindings` table in the auth database alongside the store URL
   and the current `verified_at` timestamp.
3. From this point on, every git operation for tenant `acme` goes to
   `s3://acme-bucket/repos`. The operator's shared bucket is not used for that
   tenant.

Delete the plaintext `creds.json` after binding:

```bash
rm -f creds.json
```

**Step 3.** Verify the binding from the gateway's perspective:

```bash
bucketvcs tenant storage verify \
  --auth-db 'postgres://...' \
  --tenant acme \
  --byob-encryption-key /etc/bucketvcs/byob.key
```

This decrypts the stored credentials, re-runs the probe, and updates
`verified_at` to the current time. Use it after credential rotation to confirm
the new credentials work before they go live.

---

## 4. Unbind and fallback

Remove a binding with:

```bash
bucketvcs tenant storage unbind \
  --auth-db 'postgres://...' \
  --tenant acme
```

After unbind:

- All subsequent git traffic for tenant `acme` falls back to the operator's
  shared bucket (the `--store` URL passed to `bucketvcs serve`).
- **Objects in the tenant's bucket are not moved.** If repositories were
  previously stored in the tenant's bucket, they become inaccessible unless you
  manually migrate the data to the operator's bucket before unbinding, or
  re-bind the tenant. Plan migrations carefully.

---

## 5. Serve configuration

Add `--byob-encryption-key` to every `bucketvcs serve` invocation:

```bash
bucketvcs serve \
  --store s3://operator-bucket/repos \
  --auth-db 'postgres://bv@central-host/bucketvcs_auth?sslmode=require' \
  --byob-encryption-key /etc/bucketvcs/byob.key \
  --addr :8080
```

At request time the gateway resolves the tenant's store by looking up the
binding in the auth database, decrypting the credentials, and opening the
store. Successfully opened stores are cached in memory for one hour (configurable
with `--byob-creds-ttl`) to avoid a database round-trip on every request. The
operator's shared `--store` is the fallback for tenants with no binding.

**All gateway nodes must use the same key file.** If nodes have different keys,
they cannot decrypt each other's bindings and will silently fall back to the
shared bucket for tenants that should be using a BYOB bucket. Synchronise the
key file via your secret-management system (Vault, AWS Secrets Manager, etc.)
and verify with `bucketvcs doctor --byob-encryption-key ...` on each node.

---

## 6. GC and maintenance

`bucketvcs gc` and `bucketvcs maintenance` must operate on the same bucket as
the live gateway for a given tenant. Pass `--byob-encryption-key` so these
commands can detect and use a tenant's binding automatically:

```bash
# GC for a specific BYOB tenant's repo.
bucketvcs gc \
  --auth-db 'postgres://...' \
  --byob-encryption-key /etc/bucketvcs/byob.key \
  --repo acme/website

# Maintenance for a BYOB tenant's repo.
bucketvcs maintenance \
  --auth-db 'postgres://...' \
  --byob-encryption-key /etc/bucketvcs/byob.key \
  --repo acme/website
```

When `--repo` and `--byob-encryption-key` are both present, the command looks
up the tenant binding and routes to the tenant's bucket automatically. Without
`--byob-encryption-key`, the command falls back to the operator's `--store` and
would silently operate on the wrong bucket — always pass the key file for any
per-repo operation on a BYOB deployment.

---

## 7. Monitoring and verification

**Doctor check.** `bucketvcs doctor` with `--byob-encryption-key` runs the
`byob.bindings` check, which decrypts every stored binding, opens the bucket,
and issues a List probe:

```bash
bucketvcs doctor \
  --store s3://operator-bucket/repos \
  --auth-db 'postgres://...' \
  --byob-encryption-key /etc/bucketvcs/byob.key
```

| Outcome | Detail |
|---|---|
| OK | All bindings reachable, `verified_at` within 30 days |
| WARN | One or more bindings have a stale `verified_at` (> 30 days); run `bucketvcs tenant storage verify` for each |
| FAIL | One or more bindings are unreachable or the credentials cannot be decrypted |

Run this check routinely — for example as a cron job or in your deployment
pipeline — to catch credential expiry or bucket permission changes before they
affect users.

**List all bindings.** Show all tenants with a binding and their `verified_at`
timestamps:

```bash
bucketvcs tenant storage list \
  --auth-db 'postgres://...'
```

**Re-run the probe and refresh verified_at:**

```bash
bucketvcs tenant storage verify \
  --auth-db 'postgres://...' \
  --tenant acme \
  --byob-encryption-key /etc/bucketvcs/byob.key
```

---

## 8. Limitations

- **Proxied bundle, pack, and LFS URLs.** The `/_bundle/`, `/_pack/`, and
  `/_lfs/` proxied-download paths presign URLs against the operator's shared
  bucket, not the tenant's bucket. BYOB tenants should use **direct** LFS
  transfer mode (the default when `--lfs=true` is set and the bucket supports
  presigned GET URLs) so LFS objects are fetched directly from the tenant's
  bucket. Bundle-URI and pack-URI caching is served from the operator's bucket
  for all tenants regardless of BYOB status.

- **Per-repo overrides.** There is no mechanism to route individual repositories
  within a tenant to different buckets. The binding is per-tenant; all repos for
  that tenant go to the same bucket.

- **KMS integration.** Credentials are encrypted with a symmetric AES-256-GCM
  key that you manage. Integration with external KMS providers (AWS KMS,
  Cloud KMS, Azure Key Vault) for key wrapping is deferred.
