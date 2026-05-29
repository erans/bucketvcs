# Quickstart

Get a working Git remote — backed by object storage — that you can `git push`
to and `git clone` from. This walks through install, a repository, access
control, the gateway, and your first push. It takes about ten minutes.

Replace `<...>` placeholders with your own values throughout.

---

## 1. Install

Prebuilt binaries for Linux, macOS, and Windows (amd64 + arm64) are attached to
each [GitHub Release](https://github.com/erans/bucketvcs/releases). Or build
from source (Go 1.25+):

```bash
git clone https://github.com/erans/bucketvcs
cd bucketvcs
go build -o bucketvcs ./cmd/bucketvcs
```

Put the resulting `bucketvcs` binary on your `PATH`.

---

## 2. Choose a storage backend

The `--store` URL decides where your repositories live. Everything else in this
guide is identical regardless of backend.

| Scheme            | Backend              | Credentials |
|-------------------|----------------------|-------------|
| `localfs:/path`   | Local filesystem     | none — great for trying it out |
| `s3://bucket`     | Amazon S3            | standard AWS credential chain (env, profile, or instance role) |
| `r2://bucket`     | Cloudflare R2        | `BUCKETVCS_R2_ENDPOINT` + AWS-style key/secret |
| `gcs://bucket`    | Google Cloud Storage | `GOOGLE_APPLICATION_CREDENTIALS` or workload identity |
| `azureblob://container` | Azure Blob     | account + key (or managed identity) |

For step-by-step cloud setup — creating the bucket, least-privilege
credentials, where to put the secrets, and how to run the gateway — see the
per-provider quickstarts:

- **[Amazon S3](quickstart-s3.md)**
- **[Google Cloud Storage](quickstart-gcs.md)**
- **[Azure Blob Storage](quickstart-azure.md)**
- **Cloudflare R2** — see [§7](#7-worked-cloud-example-cloudflare-r2) below

Lower-level credential details also live in the adapter READMEs:
[s3compat](../internal/storage/s3compat/README.md),
[gcs](../internal/storage/gcs/README.md),
[azureblob](../internal/storage/azureblob/README.md).

The rest of this guide uses **`localfs`** so you can follow along with zero
credentials. A fully-worked **Cloudflare R2** example is in
[§7](#7-worked-cloud-example-cloudflare-r2).

```bash
export STORE="localfs:/var/lib/bucketvcs"
export AUTHDB="./auth.db"
```

> **Metadata backend (`--auth-db`).** `$AUTHDB` is the small database holding
> users, tokens, repo registrations, permissions, policies, and webhooks — it is
> separate from where Git data lives (`--store`). It defaults to a local
> **SQLite** file (great for single-node), and can instead be a managed
> **Turso/libSQL** or **PostgreSQL** database — selected purely by the
> `--auth-db` scheme, with the secret supplied via `BUCKETVCS_DB_AUTH_TOKEN`
> (never on the command line):
>
> | `--auth-db` value | Backend | When |
> |-------------------|---------|------|
> | `./auth.db` (a path) | SQLite (default) | single node, zero setup |
> | `libsql://<db>.turso.io` | Turso / libSQL | managed, single node — [guide](m23-turso-operator-guide.md) |
> | `postgres://<host>/<db>` | PostgreSQL | single or **multi-node** — [guide](m23-b1-postgres-operator-guide.md) · [multi-node](m23-b2-multinode-operator-guide.md) |
>
> The rest of this guide uses the SQLite default; the backends are
> drop-in — every step below is identical regardless of `--auth-db`.

---

## 3. Create a repository

`init` creates the repository in object storage. Repositories are namespaced as
`<tenant>/<repo>`:

```bash
bucketvcs init --store="$STORE" acme my-repo
bucketvcs inspect-manifest --store="$STORE" acme my-repo   # sanity check
```

---

## 4. Set up access

bucketvcs authenticates Git over HTTPS with **access tokens** used as the
password, and authorizes per repository. Create a user, mint a token, register
the repo in the auth database, and grant access:

```bash
# A user (drop --admin for a normal user)
bucketvcs user add alice --auth-db="$AUTHDB"

# A scoped token — copy the printed `token=bvts_...` value; it is shown once
bucketvcs token create alice --auth-db="$AUTHDB" \
  --scopes=repo:read,repo:write --label="alice-laptop"

# Register the repo in the auth registry (storage already exists, so --no-init)
bucketvcs repo register acme/my-repo --auth-db="$AUTHDB" --no-init

# Grant alice write access (read | write | admin)
bucketvcs repo grant alice acme/my-repo write --auth-db="$AUTHDB"
```

> **Shortcut:** `bucketvcs repo register acme/my-repo --auth-db="$AUTHDB" --store="$STORE"`
> (without `--no-init`) creates the storage *and* registers it in one step,
> replacing step 3.

Optional — allow unauthenticated **read** (clone/fetch) of this repo:

```bash
bucketvcs repo public acme/my-repo on --auth-db="$AUTHDB"
```

---

## 5. Start the gateway

```bash
bucketvcs serve --store="$STORE" --auth-db="$AUTHDB" --addr=127.0.0.1:8080
```

Add `--ssh-addr=127.0.0.1:2222` to also serve Git over SSH.

**TLS:** the gateway speaks plain HTTP; in production terminate TLS at a
reverse proxy or load balancer in front of it. If it sits behind a proxy, start
it with `--trust-proxy-headers` so client IPs (used for audit and rate
limiting) are read correctly.

---

## 6. Push and clone

The token from step 4 is the HTTPS password (any username works once the token
is repo-scoped, but using the owning user is clearest):

```bash
# From an existing local repo:
git remote add origin "http://alice:<token>@127.0.0.1:8080/acme/my-repo"
git push -u origin main

# Or clone it elsewhere:
git clone "http://alice:<token>@127.0.0.1:8080/acme/my-repo"
```

To avoid putting the token in the URL, use a Git credential helper — bucketvcs
works with the standard ones (`git config credential.helper store`, the macOS
keychain helper, etc.).

That's it — your history now lives in your bucket.

---

## 7. Worked cloud example: Cloudflare R2

Provision an R2 bucket and an R2 access key in the Cloudflare dashboard, note
your S3 endpoint (`https://<account-id>.r2.cloudflarestorage.com`), then:

```bash
export BUCKETVCS_R2_ENDPOINT="https://<account-id>.r2.cloudflarestorage.com"
export AWS_ACCESS_KEY_ID="<r2-access-key-id>"
export AWS_SECRET_ACCESS_KEY="<r2-secret>"

export STORE="r2://my-bucket"
export AUTHDB="./auth.db"
```

Then follow steps 3–6 exactly as above — only the `--store` value changed.

**Migrating from localfs:** R2 (and S3) layouts are byte-identical to localfs.
Copy the tree with any S3-compatible tool and re-point `--store`:

```bash
aws s3 sync /var/lib/bucketvcs/ s3://my-bucket/ \
  --endpoint-url="$BUCKETVCS_R2_ENDPOINT"
bucketvcs inspect-manifest --store="r2://my-bucket" acme my-repo   # verify
```

**Bucket lifecycle:** garbage collection does not abort *incomplete multipart
uploads* in-process (spec §33.5) — configure your bucket to expire them
automatically. Recipes for S3 and R2 are in the
[GC operator guide §5](m8-gc-operator-guide.md#5-bucket-lifecycle-for-incomplete-multipart-uploads-335).

---

## Going further

| You want… | Start here |
|-----------|------------|
| Keyless CI auth (no long-lived secrets) | [OIDC token exchange](m22-oidc-operator-guide.md) |
| Large files | [Git LFS](m13-lfs-operator-guide.md) |
| Faster clones at scale | [bundle-URI & packfile-URI](m11-bundles-operator-guide.md) |
| Protect branches / paths, run hooks | [policy & hooks](m14-hooks-policy-operator-guide.md) |
| Keep storage tight | [`bucketvcs maintenance`](m9-maintenance-operator-guide.md) + `bucketvcs gc` ([guide](m8-gc-operator-guide.md)) |
| A managed/multi-node metadata DB | [Turso/libSQL](m23-turso-operator-guide.md) · [PostgreSQL](m23-b1-postgres-operator-guide.md) ([multi-node](m23-b2-multinode-operator-guide.md)) |

Run `bucketvcs <command> --help` for the full flag surface of any command, and
browse [`docs/`](.) for design specs and operator guides.
