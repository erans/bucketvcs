# Migrating from bucketvcs M3 to M4

M4 replaces M3's shared bearer token with real per-actor authentication.
The `--auth-mode`, `--auth-token`, and `--auth-scope` flags are gone. There
is no automated migration tool; M3's auth was a placeholder.

## What changes for operators

- Every Git request now requires a per-user token (HTTP Basic, token-as-password)
  except for repos explicitly flagged as public-read.
- Auth state lives in a SQLite database on the gateway host. By default at
  `$XDG_STATE_HOME/bucketvcs/bucketvcs.db` or `$HOME/.local/state/bucketvcs/bucketvcs.db`.
- Repos must be **registered** in the new registry to be served. Repos
  created via M1's `bucketvcs init` directly are not served until you run
  `bucketvcs repo register --no-init <tenant>/<repo>`.

## Upgrade steps

### 1. Install M4

Replace the `bucketvcs` binary on the gateway host. Do not start it yet.

### 2. Create the first admin user

```bash
bucketvcs user add <your-name> --admin
bucketvcs token create <your-name> --label "first admin"
```

The `token create` output is the only time the full token is shown.
Copy it now — there is no way to retrieve it later.

### 3. Register existing repos

For each repo that existed under M3:

```bash
bucketvcs repo register <tenant>/<repo> --no-init
```

`--no-init` skips the M1 `bucketvcs init` because the bucket state already
exists. Without `--no-init`, `register` will attempt M1 init and fail.

### 4. Grant access

For each user that should access a repo:

```bash
bucketvcs user add <name>
bucketvcs token create <name>
bucketvcs repo grant <name> <tenant>/<repo> <read|write|admin>
```

For repos that should remain world-readable:

```bash
bucketvcs repo public <tenant>/<repo> on
```

### 5. Start serve

```bash
bucketvcs serve --addr 127.0.0.1:8080 --bucket-root /var/lib/bucketvcs
```

`--auth-mode`, `--auth-token`, and `--auth-scope` are no longer recognized.
Passing any of them fails fast with a pointer to this document.

### 6. Update client `git` configuration

Clients use HTTP Basic with the username = bucketvcs username and the
password = the token printed in step 2 or 4. Standard `git credential`
helpers (osxkeychain, libsecret, manager-core, store) will remember it
after the first prompt.

For unattended CI:

```bash
git -c credential.helper='!f() { echo "username=ci-bot"; echo "password=$BUCKETVCS_TOKEN"; }; f' \
    clone https://gateway.example/acme/foo.git
```

## What does not change

- The bucket layout. M4 does not migrate any durable repo state.
- M1/M2/M3 protocol behavior other than the auth boundary.
- The differential-harness numbers (M3 ship state: 61 pass + 3 documented skips).

## Rolling back to M3

Re-deploy the M3 binary. The auth.db file will be ignored. Any registered
repos and tokens persist on disk but are unused.
