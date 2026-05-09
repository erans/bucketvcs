# M6 — SSH Gateway and SSH Public-Key Authentication

Date: 2026-05-08
Status: design draft
Scope: bucketvcs OSS milestone M6
Source spec: `docs/original-spec.md` §30.2 (SSH auth), §30.3 (transport-neutral authorization), §40.2 (Go protocol gateway), §41 (compatibility matrix)
Decomposition: `docs/superpowers/specs/2026-05-03-bucketvcs-oss-decomposition-design.md`
Predecessors: M3 (HTTP smart-Git gateway), M4 (HTTPS token auth), M5 (first cloud backend)

## 1. Purpose

M6 adds Git-over-SSH to the OSS gateway, plugging into the M4 transport-neutral authorization engine that already declared `auth.SSHKeyFingerprint` as a credential type. After M6, `git clone git@host:tenant/repo.git` and `git clone ssh://git@host:2222/tenant/repo.git` work end-to-end against the same backend, with the same actor and permission model as HTTPS, plus per-repo deploy keys for CI use.

The shipped artifact: a `bucketvcs serve` that runs HTTP and SSH listeners as sibling goroutines under one process, sharing the auth `Store`, the on-disk mirror, and the upload-pack / receive-pack engine.

## 2. What changes versus M4 / M5

Before M6 the gateway speaks HTTPS only. Authentication is HTTP Basic with a token-as-password. The upload-pack and receive-pack engines are method-on-`Server` functions tightly coupled to `http.ResponseWriter` / `http.Request`.

After M6:

- `internal/sshd` package implements an SSH server using `golang.org/x/crypto/ssh` directly.
- The protocol engines move to `internal/gitproto/{uploadpack,receivepack}` as transport-neutral drivers over `io.Reader` / `io.Writer`. The HTTP handlers in `internal/gateway` become thin adapters; SSH session handlers are the second adapter.
- `auth.Store.VerifyCredential` returns an additional optional `*Scope`, used to short-circuit `LookupRepoPerm` for deploy keys. M4's `Decide` is unchanged.
- The auth SQLite schema gains an `ssh_keys` table (migration `0002_ssh_keys.sql`).
- New CLI verbs: `bucketvcs user key {add,list,revoke}`, `bucketvcs repo deploy-key {add,list,revoke}`, `bucketvcs ssh fingerprint`.
- `bucketvcs serve` gains `--ssh-addr`, `--ssh-host-key`, `--ssh-grace` flags.

## 3. Non-goals

This milestone explicitly does not deliver:

- SSH certificate authentication (CA-signed principals). Spec §30.2 marks it MAY for enterprise; §44.15 asks whether SSH certs should be enterprise-only — answered here as "yes, deferred to enterprise scope."
- Machine keys / org-scoped CI tokens. Org primitives are a hosted-product concept.
- `git-upload-archive`. §30.2 lists it as optional; §41 does not require it.
- IP allowlists, connection-level rate limiting, fail2ban-style integration. Deferred hardening, matches M4's stance.
- Audit event durability. M15 owns the durable audit table; M6 emits structured logs only.
- SSH-over-HTTPS / port 22 multiplexing. Operational; a reverse proxy concern.
- Anonymous SSH read of public-read repos. The OpenSSH protocol has no clean "no key, succeed" path; public-read remains an HTTPS-only convenience. This is an intentional transport asymmetry vs §30.3, documented in §10.

## 4. Architecture

### 4.1 Package layout

```
internal/sshd/                          NEW
  doc.go            package overview + invariants
  server.go         Server type, Listen/Serve, host-key load/generate
  hostkey.go        load-or-generate ed25519 host key on disk
  session.go        per-session handler: parse exec command, call gitproto core
  command.go        parse & validate "git-upload-pack '<path>'" / "git-receive-pack '<path>'"
  fingerprint.go    SHA256 fingerprint helper (matches OpenSSH "SHA256:..." form)
  server_test.go
  command_test.go
  fingerprint_test.go
  hostkey_test.go
  session_test.go
  e2e_test.go

internal/gitproto/                      NEW package, refactored from gateway/
  uploadpack/
    engine.go       func Serve(req *EngineRequest) error
    engine_test.go
  receivepack/
    engine.go       func Serve(req *EngineRequest) error
    engine_test.go

internal/gateway/                       MODIFIED
  upload_pack.go    HTTP adapter: build EngineRequest from r.Body / w, call uploadpack.Serve
  receive_pack.go   HTTP adapter: same shape
  inforefs.go       unchanged behavior; calls a new gitproto helper for the advertisement
  routes.go         unchanged
  authmw.go         MODIFIED: handle optional *Scope returned from VerifyCredential

internal/auth/                          MODIFIED (additive)
  types.go          + Scope{Tenant, Repo, Perm}
                    + KeyKind = "user" | "deploy"
  store.go          VerifyCredential returns (*Actor, tokenID string, *Scope, error)
                    + AddSSHKey, ListSSHKeysForUser, ListSSHKeysForRepo, RevokeSSHKey,
                      TouchSSHKeyUsage
  conformance/      + scoped-credential and ssh_keys test cases (#14-#22)

internal/auth/sqlitestore/              MODIFIED
  schema.go         + migration 0002_ssh_keys.sql
  store.go          + ssh_keys table CRUD, fingerprint-indexed lookup

cmd/bucketvcs/
  ssh.go            NEW: bucketvcs ssh fingerprint
  user.go           MODIFIED: bucketvcs user key {add,list,revoke}
  repo.go           MODIFIED: bucketvcs repo deploy-key {add,list,revoke}
  serve.go          MODIFIED: --ssh-addr, --ssh-host-key, --ssh-grace
```

### 4.2 Invariants

1. `internal/auth` and `internal/sshd` have no overlap. SSH transport doesn't import auth-storage; auth doesn't know SSH exists beyond the `SSHKeyFingerprint` credential type.
2. `internal/gitproto/{uploadpack,receivepack}` has no HTTP, no SSH, no SQL imports. Pure protocol drivers over `io.Reader` / `io.Writer`.
3. The gateway's HTTP adapters and the sshd's session handler both call the same `gitproto` engine. Push serialization (`mirror.Manager`) lives one level below the engine and is shared automatically.
4. `auth.Decide` does not change. `Store.VerifyCredential` adds a return value; the M4 conformance suite is extended, not rewritten.
5. SSH listener and HTTP listener are sibling goroutines under one `bucketvcs serve`. Either can be disabled by omitting its `--addr` flag.
6. No private keys are stored anywhere in the system. Spec §30.5: "bucketvcs MUST NOT store user SSH private keys." Public keys are stored in plaintext (they are public); fingerprints are the lookup key.
7. SSH path normalization MUST share a single function with HTTP route parsing. Asymmetry between transports is a class of bug we eliminate by construction.

## 5. Data model

### 5.1 SQLite migration `0002_ssh_keys.sql`

```sql
CREATE TABLE ssh_keys (
    id              TEXT PRIMARY KEY,         -- bvsk_<24 base32>
    fingerprint     TEXT NOT NULL UNIQUE,     -- "SHA256:base64nopad" (OpenSSH form)
    public_key      BLOB NOT NULL,            -- raw wire-format public key bytes
    key_type        TEXT NOT NULL,            -- "ssh-ed25519" | "ssh-rsa" | "ecdsa-..."
    label           TEXT,                     -- operator-supplied display label
    created_at      INTEGER NOT NULL,
    last_used_at    INTEGER,
    revoked_at      INTEGER,

    -- exactly one of (user_id) or (scope_tenant + scope_repo) is set
    user_id         TEXT REFERENCES users(id) ON DELETE CASCADE,
    scope_tenant    TEXT,
    scope_repo      TEXT,
    scope_perm      TEXT CHECK (scope_perm IN ('read','write')),

    CHECK (
        (user_id IS NOT NULL AND scope_tenant IS NULL
                              AND scope_repo IS NULL
                              AND scope_perm IS NULL)
        OR
        (user_id IS NULL      AND scope_tenant IS NOT NULL
                              AND scope_repo IS NOT NULL
                              AND scope_perm IS NOT NULL)
    ),
    FOREIGN KEY (scope_tenant, scope_repo) REFERENCES repos(tenant, name) ON DELETE CASCADE
);

CREATE UNIQUE INDEX ssh_keys_fingerprint_idx ON ssh_keys(fingerprint);
CREATE INDEX        ssh_keys_user_idx        ON ssh_keys(user_id);
CREATE INDEX        ssh_keys_scope_idx       ON ssh_keys(scope_tenant, scope_repo);
```

Notes:

- `fingerprint` is the OpenSSH-style `"SHA256:" + base64(sha256(wire_pubkey))` with no padding — matches what `ssh-keygen -lf` and OpenSSH log messages print, so operators can compare visually.
- `public_key` is stored even though we look up by fingerprint, so the operator can re-list/export keys without inverting the hash. Public keys are public; spec §30.5 explicitly allows plaintext storage of SSH public keys.
- The CHECK constraint enforces "user key XOR deploy key" at the database level rather than relying on application correctness.
- Cascade on user delete and on repo delete keeps the table consistent without application-level cleanup.
- Key id format `bvsk_<24 base32>` mirrors `bvts_` token ids, with a distinct prefix so log greps and accidental leaks are unambiguous.

### 5.2 `auth.Scope`

```go
type Scope struct {
    Tenant string
    Repo   string
    Perm   Perm   // PermRead or PermWrite (PermAdmin not allowed for deploy keys)
}
```

### 5.3 Updated `Store` interface

```go
type Store interface {
    VerifyCredential(ctx context.Context, c Credential) (*Actor, string, *Scope, error)
        // Returns (actor, credentialID, scope, err).
        //   credentialID = tokens.id     for BasicPassword
        //   credentialID = ssh_keys.id   for SSHKeyFingerprint
        // For BasicPassword and user SSHKeyFingerprint: Scope is nil.
        // For deploy-key SSHKeyFingerprint: Actor is synthetic, Scope is set.
        // Errors unchanged from M4: ErrInvalidCredential, ErrTokenExpired,
        // ErrTokenRevoked, ErrUserDisabled.

    LookupRepoPerm(ctx context.Context, actor *Actor, tenant, repo string) (Perm, error)
    GetRepoFlags(ctx context.Context, tenant, repo string) (RepoFlags, error)
    TouchTokenUsage(ctx context.Context, tokenID string) error

    // NEW for M6:
    AddSSHKey(ctx context.Context, k SSHKey) error
        // SSHKey carries (Fingerprint, PublicKey, KeyType, Label,
        // either UserID or ScopeTenant+ScopeRepo+ScopePerm).
    ListSSHKeysForUser(ctx context.Context, userID string) ([]SSHKey, error)
    ListSSHKeysForRepo(ctx context.Context, tenant, repo string) ([]SSHKey, error)
    RevokeSSHKey(ctx context.Context, keyID string) error
    TouchSSHKeyUsage(ctx context.Context, keyID string) error

    Close() error
}
```

For deploy keys, the synthetic actor returned by `VerifyCredential` is `&Actor{UserID: "deploy:" + keyID, Name: "deploy-key:" + label}`. Nothing reads the synthetic actor beyond audit/log lines.

### 5.4 Decision-table additions

`Decide(actor, perm, action, flags)` is unchanged. The middleware sequence gains one branch:

| step | actor | scope | action |
|------|-------|-------|--------|
| 1    | nil   | nil   | (anonymous; M4 path)        |
| 2    | user  | nil   | LookupRepoPerm; M4 path     |
| 3    | synth | set   | scope.Tenant/Repo == request → use scope.Perm; else deny |

A scope mismatch (deploy key for `acme/web` cloning `acme/other`) denies before reaching `Decide`. This avoids leaking "the key exists but is for a different repo" across the auth boundary; the client sees the same denial it would for a missing key.

## 6. SSH session lifecycle

### 6.1 Listener startup

```
Server.Listen(addr):
    1. Load or generate host key (see §6.5).
    2. Build &ssh.ServerConfig{
           PublicKeyCallback: server.publicKeyCallback,
           NoClientAuth:      false,
           MaxAuthTries:      6,
           AuthLogCallback:   server.logAuthAttempt,
       }
    3. net.Listen("tcp", addr); return.

Server.Serve(ctx):
    Loop: accept, go handleConn(conn).
    On ctx.Done: close listener, drain in-flight sessions to grace deadline,
    then close them.
```

### 6.2 Per-connection flow

```
handleConn(rawConn):
    sshConn, chans, reqs := ssh.NewServerConn(rawConn, config)
    if err: log, close, return.
    go ssh.DiscardRequests(reqs)            // ignore global keepalive/etc
    actor, scope := decodePerms(sshConn.Permissions)
    for newCh := range chans:
        if newCh.ChannelType() != "session":
            newCh.Reject(UnknownChannelType, "")
            continue
        ch, chReqs, _ := newCh.Accept()
        go handleSession(ctx, ch, chReqs, actor, scope, keyID)
    sshConn.Close()
```

### 6.3 Session handler

```
handleSession(ctx, ch, reqs, actor, scope, keyID):
    var protoEnv string
    var execCmd string
    for req := range reqs:
        switch req.Type {
        case "env":
            name, value := parseEnv(req.Payload)
            if name == "GIT_PROTOCOL":
                protoEnv = value
            req.Reply(true, nil)             // accept silently; ignore others
        case "exec":
            execCmd = parseExec(req.Payload)
            req.Reply(true, nil)
            goto run
        case "shell", "subsystem", "pty-req":
            req.Reply(false, nil)            // never grant a shell
            sendExitStatus(ch, 1); ch.Close(); return
        default:
            req.Reply(false, nil)
        }
    return                                   // channel closed without exec
    run:
    cmd, err := command.Parse(execCmd)
    if err != nil:
        fmt.Fprintln(ch.Stderr(), "bucketvcs:", err.Error())
        sendExitStatus(ch, 128); ch.Close(); return

    tenant, repo, op := cmd.Tenant, cmd.Repo, cmd.Op   // upload | receive

    flags, err := store.GetRepoFlags(ctx, tenant, repo)
    if errors.Is(err, ErrNoSuchRepo):
        deny(ch, "repository not found"); return

    var perm Perm
    if scope != nil:
        if scope.Tenant != tenant || scope.Repo != repo:
            deny(ch, "key not authorized for this repository"); return
        perm = scope.Perm
    else:
        perm, _ = store.LookupRepoPerm(ctx, actor, tenant, repo)

    if !auth.Decide(actor, perm, op.RequiredAction(), flags):
        deny(ch, "insufficient permissions"); return

    engineReq := &gitproto.EngineRequest{
        Ctx:     withProtoEnv(ctx, protoEnv),
        Tenant:  tenant,
        Repo:    repo,
        Actor:   actor,
        Stdin:   ch,                         // ssh.Channel implements io.Reader
        Stdout:  ch,                         // io.Writer
        Stderr:  ch.Stderr(),
        Backend: server.repoBackend,
    }
    var serveErr error
    switch op {
    case OpUpload:  serveErr = uploadpack.Serve(engineReq)
    case OpReceive: serveErr = receivepack.Serve(engineReq)
    }
    sendExitStatus(ch, serveErr)
    go store.TouchSSHKeyUsage(ctx, keyID)    // fire-and-forget
    ch.Close()
```

### 6.4 Exec-command parsing

Accepted forms (OpenSSH passes the literal client argument to `exec`):

```
git-upload-pack 'tenant/repo.git'
git-receive-pack 'tenant/repo.git'
git-upload-pack "tenant/repo.git"
git-upload-pack /tenant/repo.git           (rare; some clients drop quotes — accept)
```

Parsing rules:

1. Split on first whitespace into `verb` and `arg`.
2. `verb` MUST be exactly `git-upload-pack` or `git-receive-pack`. Anything else (including `git-upload-archive`) → reject with "command not allowed".
3. Strip a single matching pair of `'` or `"` around `arg`. Reject mixed/unbalanced quoting.
4. Strip a single leading `/`.
5. Reject `..`, `\`, NUL, control chars, percent-encoding (`%`), and any `/` other than the single one between tenant and repo.
6. Require `.git` suffix; strip it before returning `(tenant, repo)`.
7. Reuse `routes.normalizeTenantRepo` from M4 so the SSH and HTTP path-validation rules are bit-identical. The two parsers share golden-file fixtures so drift is impossible.

### 6.5 Host-key management

- Default path: `$XDG_STATE_HOME/bucketvcs/ssh_host_ed25519_key`, falling back to `$HOME/.local/state/bucketvcs/ssh_host_ed25519_key`.
- Override with `--ssh-host-key <path>`.
- On first start, if the file doesn't exist, generate a fresh ed25519 key, write it with mode 0600, log the SHA256 fingerprint at INFO, and proceed.
- On every start, log the SHA256 fingerprint at INFO so log scraping can detect host-key churn.
- If the file exists with mode looser than 0600, log a warning and continue (don't refuse — operators may have set 0640 deliberately for group access).
- `bucketvcs ssh fingerprint` reads the file directly and prints the SHA256 fingerprint without starting the server.

### 6.6 PublicKeyCallback

```
publicKeyCallback(meta, key) (*ssh.Permissions, error):
    if meta.User() != "git":
        return nil, errors.New("only the 'git' user is supported")
    fp := fingerprint.SHA256(key)            // "SHA256:..." form
    actor, keyID, scope, err := store.VerifyCredential(ctx, SSHKeyFingerprint{Fingerprint: fp})
    if err != nil:
        return nil, err                      // x/crypto/ssh logs as auth failure
    perms := &ssh.Permissions{Extensions: map[string]string{
        "actor_id":   actor.UserID,
        "actor_name": actor.Name,
        "scope":      encodeScope(scope),    // empty if nil
        "key_id":     keyID,                 // ssh_keys.id from VerifyCredential
    }}
    return perms, nil
```

`Permissions.Extensions` carries only strings; the session handler decodes back to typed `actor` and `*Scope`. Avoiding pointer stash keeps lifetimes clean.

### 6.7 Graceful shutdown

`Server.Close()` closes the listener (no new connections), then waits up to `--ssh-grace` (default 10s) for in-flight sessions to drain. After the grace period, in-flight channels are closed; receive-pack sessions that have already committed the manifest CAS are unaffected — the client gets a normal Git transport error, the commit is durable.

`--ssh-grace 0` force-closes immediately. Useful for tests and for operators who'd rather have the client see a clean disconnect than wait.

## 7. CLI surface

### 7.1 `bucketvcs serve` flags (additive over M4)

```
--ssh-addr <host:port>      default: "" (SSH disabled)
                            common: "127.0.0.1:2222" or ":2222"
--ssh-host-key <path>       default: $XDG_STATE_HOME/bucketvcs/ssh_host_ed25519_key
                            (falls back to $HOME/.local/state/bucketvcs/...)
--ssh-grace <duration>      default: 10s
```

If `--ssh-addr` is empty, the SSH listener doesn't start; `bucketvcs serve` behaves as M4. If both `--addr` and `--ssh-addr` are empty, `serve` exits with an error ("at least one of --addr or --ssh-addr must be set").

### 7.2 `bucketvcs ssh fingerprint`

```
$ bucketvcs ssh fingerprint
SHA256:Yj+Q...nopad= bucketvcs host key (ssh-ed25519)
```

Reads the host key from the resolved path; does not require a running server. Used to publish the fingerprint to users for `~/.ssh/known_hosts` pinning.

### 7.3 `bucketvcs user key` subcommands

```
bucketvcs user key add <user> <pubkey-file> [--label <text>]
    Reads pubkey-file (single OpenSSH-format public key), validates form,
    computes fingerprint, inserts into ssh_keys with user_id=<user>.
    Refuses on duplicate fingerprint (any user).
    Prints: "added key bvsk_... (SHA256:... ssh-ed25519) for <user>".

bucketvcs user key add <user> --stdin [--label <text>]
    Same, reads pubkey from stdin.

bucketvcs user key list <user> [--json]
    id | label | type | fingerprint | created | last-used | revoked

bucketvcs user key revoke <key-id>
    Sets revoked_at. Accepts full id or unique prefix.
    Revoked keys fail auth immediately; rows persist for audit.
```

### 7.4 `bucketvcs repo deploy-key` subcommands

```
bucketvcs repo deploy-key add <tenant>/<repo> <pubkey-file> <read|write> [--label <text>]
    Inserts ssh_keys row with scope_tenant/scope_repo set, scope_perm in (read,write),
    user_id NULL. Refuses if repo not registered, refuses on duplicate fingerprint.

bucketvcs repo deploy-key add <tenant>/<repo> --stdin <read|write> [--label <text>]

bucketvcs repo deploy-key list <tenant>/<repo> [--json]
    id | label | type | fingerprint | perm | created | last-used | revoked

bucketvcs repo deploy-key revoke <key-id>
```

A single deploy key is bound to one (tenant, repo). A team that wants the same key on three repos registers it three times. Cross-repo deploy keys are a hosted-product concept; OSS doesn't need them.

### 7.5 `bucketvcs user delete <name>` (modified)

Cascades into `ssh_keys` via `ON DELETE CASCADE` (already in §5.1 schema). No CLI change; the M4 "refuses if last admin" behavior is preserved.

### 7.6 SSH username

The SSH `user` field in the connection MUST be `git`. Per spec §30.2 the actual identity comes from the public key, not the username. Other usernames are rejected by the `PublicKeyCallback` before key lookup with "only the 'git' user is supported". This keeps the auth log clean.

### 7.7 Bootstrap flow (M6 quickstart)

```
$ bucketvcs serve --addr :8080 --ssh-addr :2222 &
[log] generated host key, fingerprint SHA256:...
[log] http listening on :8080
[log] ssh listening on :2222

$ bucketvcs user add eran --admin
$ bucketvcs token create eran --label "first admin"
bvts_...                                       # one-shot, copy this

$ bucketvcs user key add eran ~/.ssh/id_ed25519.pub --label "laptop"
added key bvsk_... (SHA256:... ssh-ed25519) for eran

$ bucketvcs repo register acme/web
$ bucketvcs repo grant eran acme/web write

$ git clone ssh://git@localhost:2222/acme/web.git
```

## 8. Engine refactor

The largest correctness risk in M6 is the move from `internal/gateway/{upload,receive}_pack.go` into `internal/gitproto/{uploadpack,receivepack}`. The current handlers are method-on-`Server` taking `(w http.ResponseWriter, r *http.Request, tenant, repoID)`.

The refactored shape:

```go
package uploadpack

type EngineRequest struct {
    Ctx     context.Context
    Tenant  string
    Repo    string
    Actor   *auth.Actor
    Stdin   io.Reader     // request body for HTTP, ssh.Channel for SSH
    Stdout  io.Writer     // response writer for HTTP, ssh.Channel for SSH
    Stderr  io.Writer     // discarded for HTTP; ch.Stderr() for SSH
    Backend RepoBackend   // existing mirror.Manager / repo state seam
}

func Serve(req *EngineRequest) error
```

Same shape for `package receivepack`.

The HTTP handlers in `internal/gateway` become a few lines each: build the `EngineRequest`, set the response content-type and `Cache-Control` headers, call `Serve`, map the returned error to an HTTP status.

**Promotion gate:** the existing M3 byte-for-byte fixtures (ref advertisement, pack negotiation, report-status) move into `gitproto` engine tests. Those tests must reproduce every M3 fixture byte before any SSH wiring lands. If a single byte differs, M6 stops and the refactor returns to drawing board.

Push serialization (`mirror.Manager`) and manifest CAS live one level below the engine and are unchanged. Receive-pack over SSH and over HTTPS contend on the same per-repo serialization automatically because they call into the same backend.

## 9. Testing

### 9.1 Unit tests (new)

- `internal/sshd/command_test.go` — exhaustive parser table:
  - all four accepted exec command shapes round-trip to expected (tenant, repo, op).
  - rejects: `git-upload-archive`, `bash`, `git-shell`, empty, no-quotes-with-space-in-arg.
  - rejects: `..`, `\`, NUL byte, control chars, percent-encoding, double `/`, missing `.git`.
  - rejects: mixed/unbalanced quotes.
  - normalization output identical to `routes.normalizeTenantRepo` for the same inputs (golden file shared between the two parser tests so drift is impossible).
- `internal/sshd/fingerprint_test.go` — fixture pubkeys (ed25519, rsa-2048, ecdsa-p256) produce the exact `SHA256:...` strings that `ssh-keygen -lf` produces. Pinned vectors.
- `internal/sshd/hostkey_test.go` — generate-on-missing creates ed25519 with mode 0600; load-existing returns same key bytes; mode-loosening on existing file (e.g. 0644) produces a warning log but still loads.
- `internal/auth/sqlitestore/store_test.go` (extended) — every `ssh_keys` operation:
  - insert user key + scope key, lookup by fingerprint returns correct shape.
  - CHECK constraint rejects rows with both user_id and scope set, and rows with neither.
  - duplicate fingerprint rejected with a typed error (not a raw SQL error).
  - cascade on `users` delete clears that user's keys.
  - cascade on `repos` delete clears that repo's deploy keys.
  - revoked key returns `ErrTokenRevoked` from `VerifyCredential`.
  - lookup of a key whose user is disabled returns `ErrUserDisabled`.
- `internal/auth/permissions_test.go` (extended) — table-driven cases for `Decide` with deploy-key Scope: scope.Tenant/Repo mismatch never reaches `Decide` (caught earlier in middleware), but the synthetic actor + scope.Perm produce identical Decide outcomes to a real user with the equivalent grant. Symmetry test prevents drift.

### 9.2 Engine refactor tests (new)

- `internal/gitproto/uploadpack/engine_test.go` — drives the engine over an in-memory pipe pair (no HTTP, no SSH), asserts pkt-line advertisement bytes match the M3 golden fixtures.
- `internal/gitproto/receivepack/engine_test.go` — same shape, drives a fixture push, asserts CAS commit and report-status response bytes match M3 fixtures.

The existing M3 fixtures move from gateway tests into engine tests; gateway tests become thin wrapper tests asserting HTTP framing.

### 9.3 Integration tests (new)

`internal/sshd/session_test.go` — using `golang.org/x/crypto/ssh` client against the in-process server:

- successful exec of upload-pack returns ref advertisement bytes.
- successful exec of receive-pack accepts a push and reports status.
- shell request is rejected with non-zero exit.
- subsystem request is rejected.
- pty-req is rejected.
- non-`git` ssh username rejected before key lookup.
- revoked key fails auth without reaching the session handler.
- unknown command after exec returns exit-status 128 and a clear stderr message.
- `GIT_PROTOCOL=version=2` env propagates to the engine and v2 advertisement is served.
- graceful shutdown: in-flight session completes; new connections refused after `Close()`.

### 9.4 End-to-end against `git`

`internal/sshd/e2e_test.go`, modeled on M4's `e2e_auth_test.go`. Uses a real `git` binary configured with `GIT_SSH_COMMAND` pointing at a script that uses a generated client keypair against the test server.

1. `git clone ssh://git@127.0.0.1:<port>/acme/web.git` with valid user key → success.
2. Same with revoked key → permission denied at SSH layer; clone fails.
3. Same with key whose user is disabled → fails.
4. `git push` with read-only deploy key → permission denied with `auth.Decide` reason.
5. `git push` with write deploy key → success.
6. Deploy key for `acme/web` cloning `acme/other` → "key not authorized for this repository".
7. Public-read repo, no key offered → SSH auth fails (intentional asymmetry from §3 / §10).
8. Force-push (`--force-with-lease`) over SSH with write perm → behaves identically to HTTPS path.
9. Annotated tag push → same.
10. `git ls-remote ssh://...` → success path matches HTTPS ls-remote bytes after stripping transport framing.

### 9.5 Differential harness (new oracle)

New oracle: `clone-equivalence-ssh-vs-http`. For every existing M3 differential fixture, run a clone against the bucketvcs gateway over SSH and over HTTPS; require object closure to be identical. This is the contract test that "transport doesn't perturb pack bytes."

The existing M3 fixtures (16 × 4 oracles) are not multiplied; the new oracle is a single comparison axis added on top. The known-divergence list (§40.3 of the original spec) MUST be updated if any divergence is found; it MUST NOT be added to silently.

### 9.6 Conformance suite extension

`internal/auth/conformance/conformance.go` gains tests #14–#22:

```
14. AddSSHKey rejects duplicate fingerprint across users.
15. AddSSHKey rejects key with no user_id and no scope.
16. AddSSHKey rejects key with both user_id and scope.
17. VerifyCredential(SSHKeyFingerprint) returns the user actor for a user key.
18. VerifyCredential(SSHKeyFingerprint) returns synthetic actor + scope for a deploy key.
19. VerifyCredential rejects a revoked SSH key.
20. VerifyCredential rejects an SSH key whose user is disabled.
21. RevokeSSHKey is idempotent and accepts unique id prefixes.
22. Cascade: deleting a user removes their SSH keys; deleting a repo removes its deploy keys.
```

Any future `Store` (e.g. Postgres for the hosted product) must pass all 22 tests.

### 9.7 Stress smoke (`+build stress`)

- 200 parallel SSH clones against a single warm gateway with distinct user keys → all succeed, no auth-row update lost, no goroutine leak (assert via `runtime.NumGoroutine()` delta).
- 10,000 sequential public-key callbacks against a 1,000-key DB completes under 5s on a dev box. Catches accidental n² scans on the fingerprint index.

### 9.8 Review protocol

Per memory `m1_review_protocol.md`:

- Per-task: superpowers `code-reviewer` + per-task roborev.
- Branch-level after merge candidate: `roborev-refine` on max reasoning until passing or diminishing returns.
- The engine refactor (the M3-fixture-preserving move from `gateway/{upload,receive}_pack.go` into `gitproto/{uploadpack,receivepack}`) gets its own roborev pass before any SSH code lands. It's the change with the largest correctness blast radius.

## 10. Security considerations

- No private-key storage. Spec §30.5 honored; validated in the conformance suite.
- Public keys stored in plaintext; permitted by §30.5.
- Argon2id is not used for SSH keys — the lookup is by SHA256 fingerprint of a public-key blob, not by a password. Brute-force resistance is not the threat model here.
- Constant-time fingerprint comparison is unnecessary because fingerprints are not secrets; `ssh.MarshalAuthorizedKey` and the SHA256 hash can be computed by anyone with the public key.
- The `MaxAuthTries: 6` setting matches OpenSSH's default; a single connection can present up to six keys before disconnect. This is normal for `ssh-add` users with many loaded keys. Failed attempts are logged via `AuthLogCallback`. Per-IP rate limiting is deferred (§3).
- Bootstrap trust root remains filesystem access on the gateway host. Same as M4.
- Path traversal: the exec-command parser shares its normalization with the HTTP route parser, so any traversal-defense bug exists once and is caught once.
- Anonymous SSH access to public-read repos is intentionally not supported. The OpenSSH protocol has no "no key, succeed" mode; allowing one would require either a `git`-user-with-no-key handler (security-sensitive) or a separate transport. HTTPS retains anonymous public-read; SSH does not.
- Authenticated-but-unauthorized over SSH manifests as a clear stderr message and a non-zero exit status, mirroring M4's 403 vocabulary. Git client error messages will include the stderr line.
- Hidden-existence trade-off: spec §42.13 ("avoid leaking private repo object existence across tenants") is honored over SSH the same way M4 honors it over HTTPS — `ErrNoSuchRepo` and "insufficient permissions" produce the same client-visible message ("repository not found"). An anonymous SSH probe cannot enumerate private repos because no key means no auth at all.

## 11. Open questions (decided in this design)

1. *Should SSH cert auth land in M6?* No; deferred per §3. §44.15 answered "yes, enterprise scope" for OSS.
2. *Should anonymous SSH read of public-read repos work?* No; documented as intentional transport asymmetry in §3 and §10.
3. *Should `bucketvcs user add` accept an inline pubkey?* No; M4 deferred to M6 and we still defer to the explicit `user key add` verb. One thing per command.
4. *Should we support `--ssh-grace 0`?* Yes; allows fast tests and operators who'd rather have the client see a clean disconnect than wait.
5. *Should the same public key be both a user key and a deploy key?* No; the schema enforces unique fingerprint across the whole `ssh_keys` table. Operators who want a key in both roles register two distinct keys.

## 12. Acceptance criteria

M6 ships when:

1. All §9.1–§9.6 tests pass.
2. The §9.7 stress smoke meets the stated bounds.
3. The new SSH-vs-HTTP differential oracle reports identical object closure across all M3 fixtures.
4. M3's existing 61 differential pass + 3 documented skips are unchanged.
5. M4's HTTPS auth e2e (`internal/gateway/e2e_auth_test.go`) passes unchanged — proves the auth refactor (added `*Scope` return) is backward-compatible.
6. `git clone ssh://git@host:port/tenant/repo.git` succeeds for a user with read; fails with no key, with a revoked key, with a disabled-user key.
7. `git push` succeeds for a user with write; fails for read-only.
8. Deploy keys clone/push only their bound repo; cross-repo attempts fail.
9. `bucketvcs ssh fingerprint` prints the host-key fingerprint without a running server.
10. `bucketvcs serve` started with no `--addr` and no `--ssh-addr` exits with a clear error.
11. `staticcheck`, `go vet`, and `gofmt` are clean across the M6 diff.
12. `roborev-refine` on the merged branch reports passing or diminishing returns.

## 13. Out of scope for this design spec

Per the brainstorming process, this spec is a design contract. Detailed choices deferred to the implementation plan:

- Exact CLI flag-parsing wiring for the new subcommands.
- Exact log line schemas for SSH auth events.
- The internal shape of `gitproto.RepoBackend` (it must be the same seam M3 uses; the implementation plan can rename or split it as needed without changing this design's promises).
- Internal layout of `command.Parse` (single function vs split lex/normalize stages).
- Test fixture file naming.

## 14. References

- `docs/original-spec.md` §30.2 (SSH auth), §30.3 (transport-neutral authorization), §30.5 (no plaintext private keys, public keys allowed plaintext), §40.2 (Go protocol gateway), §41 (compatibility matrix), §42 (security), §44.15 (SSH certs as enterprise question).
- `docs/superpowers/specs/2026-05-03-bucketvcs-oss-decomposition-design.md` (M6 row).
- `docs/superpowers/specs/2026-05-06-m4-https-token-auth-design.md` (auth.Store + Decide invariants; conformance suite).
- `docs/superpowers/specs/2026-05-06-m3-git-protocol-gateway-design.md` (HTTP engine that this milestone refactors).
- Memory: `m4_progress.md`, `m5_progress.md`, `m1_review_protocol.md`.
