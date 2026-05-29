# bucketvcs

### Your repositories live in your bucket.

**bucketvcs is a Git server backed directly by cloud object storage** — Amazon S3, Cloudflare R2, Google Cloud Storage, or Azure Blob. No database cluster holding your Git objects. No ever-growing block-storage volume to snapshot and babysit. The bucket *is* the repository.

Point stock `git` at it over HTTPS or SSH, push, and your history lands in object storage that's effectively infinite, eleven-nines durable, and priced by the gigabyte.

---

## Why bucketvcs

### 💸 Object-storage economics
Version control at the price of a bucket. Pay per-GB at S3/R2/GCS/Azure rates instead of provisioning, growing, and backing up block volumes. On Cloudflare R2 there are **no egress fees** — clone and fetch all day. Durability and scale are your provider's problem, not yours.

### 🔐 Your data, your account
Bring your own bucket. Your code sits in **your** cloud account, under **your** encryption keys and **your** access policies — not on a vendor's servers. Delete the deployment tomorrow and your repositories are still right where you left them.

### 🧩 Real Git, no special client
Native **HTTPS and SSH**, Git **protocol v2**, and full compatibility with stock `git` and standard credential helpers. Nothing to install on developer machines. It behaves like the Git remote your team already knows.

### ⚡ Fast clones at scale
Protocol-v2 **bundle-URI** and **packfile-URI** acceleration offload heavy initial clones to signed object-storage URLs (CDN-frontable on cloud backends), so the gateway isn't streaming gigabytes on every onboarding.

### 🔋 Batteries included
Not a toy. bucketvcs ships the things a real Git host needs:

- **Git LFS** — batch transfer, file locks, per-tenant quotas, and LFS garbage collection
- **Keyless CI** — OIDC token exchange (RFC 8693): your pipeline trades its IdP identity for a short-lived, repo-scoped token, so there are no long-lived secrets to leak
- **Fine-grained auth** — scoped access tokens with rotation, SSH user & deploy keys, and per-IP rate-limiting on credential failures
- **Policy & governance** — protected refs, protected paths, custom pre/post-receive hooks, and signed, retryable **webhooks**
- **Self-maintaining** — background repack, commit-graph/reachability maintenance, and operator-driven garbage collection keep storage tight

### 🛠️ Operationally boring (the good kind)
A single pure-Go binary. The only local state is a small SQLite file for auth and metadata — your **Git data never touches a database**. The gateway is easy to run, easy to scale out, and has nothing stateful to lose.

---

## How it compares

Traditional self-hosted Git (GitHub Enterprise, GitLab, Gitea) keeps your repositories on a database and a block-storage filesystem you have to size, monitor, snapshot, and migrate. bucketvcs makes the object store the source of truth instead:

|                        | Traditional Git host        | bucketvcs                          |
|------------------------|-----------------------------|------------------------------------|
| Repo storage           | Block volume + database     | Object storage (the bucket *is* it) |
| Scaling storage        | Resize/migrate volumes      | Unbounded, automatic                |
| Durability & backup    | Your snapshots & ops        | Provider's (eleven 9s)              |
| Data ownership         | Vendor-managed              | Your cloud account, your keys       |
| Footprint              | Services + DB + storage     | One Go binary + a bucket            |

Runs on **S3, R2, GCS, and Azure Blob** (all first-class), plus a local-filesystem backend for development that needs no credentials.

---

## Get started

A complete end-to-end walkthrough — install, a repository, access control, the
gateway, and your first push (on local disk or any cloud backend) — lives in the
**[Quickstart](docs/quickstart.md)**.

The short version:

```bash
bucketvcs init   --store s3://my-bucket my-org my-repo
bucketvcs serve  --store s3://my-bucket --addr :8080
git push https://my-host/my-org/my-repo main
```

---

## Status

bucketvcs is open-source and built for production use as a **Git-protocol server and CLI**. It is a backend — there is **no web UI yet**; you administer it through the `bucketvcs` command and drive Git over HTTPS/SSH.

Run `bucketvcs <command> --help` for the full command surface, and browse **[`docs/`](docs/)** for design specs, operator guides, and quickstarts.

---

## License

Licensed under the [Apache License, Version 2.0](LICENSE). Copyright 2026 Eran Sandler.
