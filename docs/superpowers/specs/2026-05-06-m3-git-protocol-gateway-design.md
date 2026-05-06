# M3 — Git Protocol Gateway Design

**Status:** Draft
**Date:** 2026-05-06
**Predecessors:** M0 (storage foundation), M1 (repo state engine), M2 (Git object engine)
**Position in roadmap:** First milestone exposing bucketvcs over the wire. After M3, `git clone` / `git fetch` / `git push` work end-to-end against a localfs-backed repo via HTTP.

## 0. Executive summary

M3 ships an HTTP smart-Git gateway that speaks Git protocol v2 (upload-pack) and protocol v0 (receive-pack) over `/{tenant}/{repo}.git/...` URLs. The gateway is **single-binary, single-gateway, in-process push-serialized**. It runs `git pack-objects` and `git index-pack` against a **per-repo on-disk bare-repo mirror** that is kept in sync with the bucket's authoritative manifest. M3 covers spec §2, §13 (HTTP only), §16.1, §17, §18 (basic), §41.

Four architectural calls anchor the design:

1. **Track B (pure-Go protocol, shell-out for pack assembly).** Gateway owns pkt-line framing, capability negotiation, ref advertisement, side-band, and v2 command dispatch in Go. Pack production (fetch response) and pack ingestion (push validation) shell out to `git pack-objects` / `git index-pack` against a local bare repo. Continues M2's "pure-Go on the read path, shell-out on the conversion path" precedent.
2. **B2 — per-repo on-disk mirror kept in sync.** Gateway maintains a working bare repo per repo on local disk under `<mirror-root>/<tenant>/<repo>/bare/`. Lazy-materialized via M2 exporter on first request; advanced incrementally on push (`cp` validated pack into `objects/pack/`, then `git update-ref`); rebuilt from scratch on stale-detection at startup or after a missed bucket update.
3. **Protocol v2 only** (with v0 receive-pack). Modern git 2.18+ supports v2; older clients see clean errors. v1 fallback is additive in the future if needed but explicitly out of scope.
4. **A2 — optional shared bearer token with configurable scope.** `--auth-token <secret> --auth-scope write-only|all`. No token configured → fully anonymous. Per-user identity / OIDC / mTLS / SSH keys defer to M5+.

The differential harness from M2 extends with two new oracles (clone-equivalence, push-equivalence) parameterized over the existing fixture corpus plus 5 new fixtures. M3 ships when 16 fixtures × 4 oracles = 64 oracle assertions pass and the static analysis / build gates from M2 stay green.

## 1. Goals

- **G1:** A `git clone http://localhost:PORT/<tenant>/<repo>.git` against a bucketvcs-backed repo produces a bare clone byte-identical to a clone of the upstream original.
- **G2:** A `git push http://localhost:PORT/<tenant>/<repo>.git --mirror` from a bare repo, followed by `bucketvcs export`, produces a bare repo byte-identical to the input.
- **G3:** Incremental fetch returns only objects reachable from `wants` minus `haves`.
- **G4:** All push paths atomically commit through the M1 transaction kernel (`tx/{tx_id}.json` create-if-absent + manifest CAS). A push either lands fully or leaves no committed effect.
- **G5:** Concurrent pushes to the same repo serialize correctly (in-process); concurrent fetches and concurrent pushes to *different* repos run in parallel.
- **G6:** Single-binary deploy: `bucketvcs serve --addr :8080 --store localfs:/path` runs a working gateway with no external dependencies beyond the `git` binary on `$PATH`.

## 2. Non-goals (out of scope, deferred)

| Item | Deferred to | Rationale |
|---|---|---|
| SSH transport | M5+ | HTTPS covers OSS demo; SSH is a separate transport layer (sshd integration, key auth, command parsing). |
| Protocol v1 fallback | future, optional | v1 stateful semantics fight cloud-storage-native model; v2-only covers post-2018 clients. |
| Partial-clone (`filter` capability) | M11 | Requires reachability-with-filter walks beyond M2's commit graph. |
| `bundle-uri`, `packfile-uri` | M11+ | Bundle freshness/policy is its own design (spec §16.3). |
| Per-user identity, ACLs, OIDC, mTLS, SSH keys | M5+ | §30 native git auth/authz is its own milestone. |
| Hooks (pre/update/post-receive) | M14 | §23 policy-engine territory. |
| Webhooks, audit events | M15 | §24, §31 async delivery + structured event design. |
| Multi-region, leases, distributed serialization | M12 | §18's distributed mechanisms; §26. |
| Reachability delta indexes, compaction | M10 | §14.1–§14.2 standalone design. |
| Pack repacking, bitmaps, multi-pack-index, background maintenance | M9 | §15.3. |
| Auto-create-on-push | M14+ | Touches policy/auth. M3 requires explicit `bucketvcs init`. |
| Native TLS termination | never (operator concern) | Reverse proxy is the documented safe-deploy pattern. |
| LFS support | M13 | §22. |

## 3. Architecture overview

```
                         HTTP request
                              │
                              ▼
                  ┌─────────────────────┐
                  │   internal/gateway  │     auth, routing,
                  │     (HTTP layer)    │     pkt-line framing,
                  │                     │     v2 dispatch
                  └──────────┬──────────┘
                             │
                ┌────────────┴────────────┐
                ▼                         ▼
      ┌─────────────────┐       ┌─────────────────┐
      │ internal/pktline│       │ internal/v2proto│   ls-refs, fetch,
      │  (frame I/O)    │       │ (commands+caps) │   ref-advert (v2)
      └─────────────────┘       └────────┬────────┘
                                         │
                                         ▼
                         ┌─────────────────────────────┐
                         │     internal/mirror         │   per-repo
                         │  (on-disk bare-repo cache,  │   on-disk
                         │   manifest-version sync,    │   working
                         │   per-repo mutex)           │   copy
                         └──────────┬──────────────────┘
                                    │
                          shells out to git
                                    │
                  ┌─────────────────┴──────────────────┐
                  ▼                                    ▼
         git pack-objects                        git index-pack
         (fetch response)                        (push validation)
                                    │
                                    ▼
                      ┌──────────────────────────┐
                      │ M2's importer write path │   build .bvom/.bvcg
                      │   (called for push)      │   from new pack
                      └──────────┬───────────────┘
                                 │
                                 ▼
                      ┌──────────────────────────┐
                      │    internal/repo         │   atomic Commit
                      │ (M1 transaction kernel)  │   via root manifest CAS
                      └──────────────────────────┘
                                 │
                                 ▼
                       storage.ObjectStore (M0)
```

**Authority model.** The bucket manifest (root manifest written via M1's CAS) is the durable source of truth. The on-disk mirror is a derived view, always reproducible from the manifest. The mirror's local state is never authoritative — it can be deleted at any time and the next request will rebuild it.

**Hot path** (fetch): HTTP → pkt-line → v2 dispatch → mirror read-lock acquired → stale check (one HEAD on the manifest) → `git pack-objects` against the mirror → side-band the resulting pack to the client.

**Hot path** (push): HTTP → pkt-line → receive-pack handler → mirror write-lock acquired → stale check → write inbound pack to temp → `git index-pack --strict` validate → `git fsck --connectivity-only` validate → build new `.bvom`/`.bvcg` via M2 importer write path → upload → `repo.Commit` (M1 CAS) → `IngestPack` into mirror → side-band status to client.

## 4. Package layout

**New packages** (M3 adds):

- `internal/pktline` — pkt-line frame reader/writer. ~150 LOC.
- `internal/v2proto` — protocol v2 command dispatch (`ls-refs`, `fetch`), capability advertisement, side-band wrapper. ~600–800 LOC.
- `internal/mirror` — per-repo on-disk bare-repo cache: `Open`, `IngestPack`, lazy materialization, manifest-version sync, per-repo mutex. ~400 LOC.
- `internal/gateway` — HTTP handlers, auth middleware, routing, request lifecycle. ~300 LOC.
- `cmd/bucketvcs serve` — subcommand wrapping `gateway.NewServer(store, opts).ListenAndServe(addr)`. ~30 LOC.

**Existing packages M3 imports unchanged:**

- `internal/storage` (M0) — `ObjectStore` for all bucket I/O.
- `internal/repo` (M1) — `Open`, `ReadRoot`, `Commit`. M3 does not add new packages under `internal/repo/`.
- `internal/repo/manifest` (M2) — `Body{DefaultBranch, Refs, Packs, Indexes, Bundles}` wire format. Locked to golden file.
- `internal/pack`, `internal/objindex`, `internal/commitgraph` (M2) — used to validate-and-build `.bvom`/`.bvcg` for the push write path.
- `internal/importer` (M2) — pack→bvom→bvcg→upload→Commit pipeline reused for the push write path. M3 may extract a shared `BuildAndCommit` helper if duplication becomes painful; otherwise calls the existing private functions through a thin internal façade.
- `internal/exporter` (M2) — used by `mirror` for first-time materialization and stale-rebuild.
- `internal/gitcli` (M2) — wrappers for `pack-objects`, `index-pack`, `update-ref`, `rev-parse`, `fsck`. Extended in M3 with `RevList` (compute new objects given wants/haves) and `PackObjectsForFetch` (the gateway-side fetch wrapper that takes wants/haves on stdin).

## 5. Protocol v2 implementation (upload-pack)

### 5.1 pkt-line framing

`internal/pktline.Reader` and `Writer` over `io.Reader`/`io.Writer`:
- 4-byte hex length prefix where length includes the 4 bytes; max payload 65516 bytes.
- Special markers: `0000` (flush), `0001` (delim, v2 only), `0002` (response-end, v2 only).
- Side-band wraps each data frame's payload with a 1-byte channel prefix: 1 = data, 2 = progress, 3 = fatal.
- Reader refuses oversized frames (>65520 bytes including header) and negative-looking lengths.
- Writer's `WritePacket(b)` validates `len(b) <= 65516` and returns error otherwise.

### 5.2 v2 capability advertisement

`GET /{tenant}/{repo}.git/info/refs?service=git-upload-pack` with `Git-Protocol: version=2` header:

```
# service=git-upload-pack
flush
version 2
agent=bucketvcs/0.1
ls-refs=unborn
fetch=shallow
flush
```

(In all subsequent protocol-flow examples in this document the pkt-line length prefixes are elided for readability; precise lengths are computed by `internal/pktline.Writer`. `flush` indicates the special `0000` marker; `delim` is `0001`.)

`agent` is the bucketvcs build version. We skip `filter` (partial-clone, M11) and `bundle-uri` (M11). When the request is missing `Git-Protocol: version=2`, we still serve a v0-shaped ref-advertisement so older clients see a clean error rather than corrupted output, but the actual `git-upload-pack` POST returns a `protocol-error: protocol v2 required` message that recent git clients surface clearly.

### 5.3 `command=ls-refs`

Body:
```
command=ls-refs
delim
peel
symrefs
ref-prefix refs/heads/
flush
```

Server walks `manifest.Body.Refs` (and resolves `HEAD` symref-target via `manifest.Body.DefaultBranch`):
```
<oid> refs/heads/main symref-target:HEAD
<oid> refs/heads/feature
flush
```

No shell-out. Output is a pure function of the manifest snapshot. `ref-prefix` filters server-side; multiple prefixes union.

### 5.4 `command=fetch`

Body:
```
command=fetch
delim
ofs-delta
thin-pack
no-progress
include-tag
want <oid>
want <oid>
have <oid>
done
flush
```

Server flow:
1. Acquire mirror RLock.
2. Stale check: `repo.ReadRoot(ctx)` → compare manifest version to `<mirror>/manifest_version.txt`. If stale, drop RLock, acquire WLock, rebuild, downgrade back to RLock (or just keep WLock for the duration — simpler and rare).
3. Validate every `want` is reachable: `git rev-parse --verify <oid>^{commit}` (or `^{tag}` for annotated tags) against the mirror. Unknown OID → `ERR upload-pack: not our ref`.
4. Build the pack-objects invocation:
   ```
   git -C <mirror> pack-objects --revs --thin --stdout \
       --delta-base-offset --no-replace-objects [--shallow-file <tmp> if shallow]
   ```
5. Pipe `--<want>\n^<have>\n` to stdin (one per line, `done` not needed for plumbing).
6. Stream stdout to client via side-band band 1; stderr to band 2 (progress) unless `no-progress`.
7. Response shape:
   ```
   acknowledgments
   delim
   ACK <oid> common
   ready
   flush
   --- (then in same response or new POST per stateless RPC):
   packfile
   delim
   <side-band-1 frames with pack bytes>
   flush
   ```

Capabilities honored in M3: `ofs-delta`, `thin-pack`, `include-tag`, `no-progress`, `shallow`/`deepen` (depth-N), `done`. Capabilities advertised but trivially handled: `agent` (logged). Capabilities not advertised: `filter`, `bundle-uri`, `wait-for-done`, `sideband-all`.

### 5.5 Shallow handling

`fetch=shallow` advertised → client may send `deepen <depth>`, `deepen-since <time>`, `deepen-not <ref>`, `shallow <oid>`. Server constructs a per-request temporary `shallow` file containing the client-asserted shallow boundary OIDs and passes it to git via `git -c shallowFile=<tmp> -C <mirror> pack-objects ...` (or by writing the file to a per-request temp clone of the bare repo, depending on which proves cleaner in implementation):
- `deepen <depth>` → `git rev-list --max-count=<depth>` from the wants, with shallow boundary computed as the deepest commits.
- `shallow <oid>` from client → recorded in the temp shallow file.

This is the most fiddly v2 area. M3 covers depth-N shallow only; commit-time / ref-relative shallow advertised but tested only minimally and may produce conservative results (more objects than strictly necessary). Failures are caught by the differential harness `shallow_clone_depth_1` fixture.

## 6. Receive-pack v0 implementation (push)

v2 does not redefine receive-pack; pushes use the v0 framing.

### 6.1 Ref advertisement

`GET /{tenant}/{repo}.git/info/refs?service=git-receive-pack`:

```
# service=git-receive-pack
flush
<oid> refs/heads/main\0report-status delete-refs ofs-delta atomic side-band-64k agent=bucketvcs/0.1
<oid> refs/heads/feature
<oid> refs/tags/v1
flush
```

First ref line carries the NUL-terminated capability list. Capabilities advertised: `report-status`, `delete-refs`, `ofs-delta`, `atomic`, `side-band-64k`, `agent`. NOT advertised: `quiet`, `push-options` (M14), `push-cert` (M14).

### 6.2 Receive flow

`POST /{tenant}/{repo}.git/git-receive-pack`. Body:
```
<old-oid> <new-oid> <refname>\0<capability-list>
<old-oid> <new-oid> <refname>
... more ...
0000
PACK<pack bytes>
```

Server flow:
1. Auth (Section 10).
2. `repo.Open` → 404 if missing.
3. Parse pkt-line update commands until flush; capture `(old, new, ref)` tuples and the capability set.
4. Read remaining body as raw pack bytes into `<mirror>/incoming/<request-id>.pack`. Cap at 1 GiB body size.
5. Acquire `mirror.Lock()`.
6. Stale-sync the mirror (Section 7).
7. Validate `old-oid` for each command (`git rev-parse <ref>` against mirror; mismatch → `ng <ref> stale info`).
8. Validate refnames via `git check-ref-format`; reject `refs/replace/*` writes; reject zero `new-oid` outside explicit deletes.
9. Validate pack: `git index-pack --strict --fix-thin --keep <pack>` against mirror. Failure → `unpack invalid-pack`, abort.
10. Validate connectivity: `git rev-list --objects --not --all <new-oids>` against mirror should produce zero new objects beyond the pack contents.
11. Validate fast-forward unless force semantics (M3 accepts force; M14 will tighten).
12. Atomic-batch handling: under `atomic`, any failure aborts all updates with `ng <ref> atomic-batch-failed`.
13. Build new artifacts: `objindex.Build` over all packs (existing + new), `commitgraph.Build` over all commits. Upload pack/.bvom/.bvcg.
14. Construct new `manifest.Body`: apply ref updates to `Refs`, append new pack to `Packs`, set `Indexes.ObjectMap`/`Indexes.CommitGraph` to new keys, leave `Bundles` unchanged.
15. `repo.Commit`. CAS retry once on conflict; second failure → `ng <ref> stale-manifest`.
16. `mirror.IngestPack` — `cp` pack into `objects/pack/`, run `git update-ref` per command (or `update-ref -d` for deletes), bump `manifest_version.txt`.
17. Emit pkt-line status report wrapped in side-band band 1:
    ```
    unpack ok
    ok refs/heads/main
    ng refs/heads/feature non-fast-forward
    flush
    ```
18. Release mutex after response is fully written.

### 6.3 Force-push handling

`new-oid` is not a fast-forward of `old-oid` → still accepted in M3. Branch protection (`refs/heads/main` non-FF rejection, signed-commit requirements, etc.) is M14 policy-engine territory. The differential harness includes a `force_push_overwrite` fixture to lock the M3 behavior.

### 6.4 Delete handling

`new-oid = 000000…` and `delete-refs` advertised → ref removed from `manifest.Body.Refs` and from mirror via `git update-ref -d <ref>`. The pack body for delete-only pushes may be empty (no PACK header) — server tolerates this.

## 7. Mirror lifecycle (`internal/mirror`)

### 7.1 On-disk layout

```
<mirror-root>/                      # default $XDG_CACHE_HOME/bucketvcs/mirrors
└── <tenant>/<repo>/
    ├── bare/                       # the bare git repo (HEAD, refs/, objects/, etc.)
    ├── manifest_version.txt        # sentinel: bucket manifest version this mirror is synced to
    ├── incoming/                   # transient pack staging during push
    └── lock                        # advisory file lock (process-level flock)
```

`<mirror-root>` configurable via `--mirror-dir` flag or `BUCKETVCS_MIRROR_DIR` env. Default `$XDG_CACHE_HOME/bucketvcs/mirrors` falling back to `/tmp/bucketvcs-mirrors` if XDG unset. The `lock` file is acquired at server start and released at clean shutdown; a second `bucketvcs serve` against the same mirror dir refuses to start.

### 7.2 Per-repo mutex

`*Mirror` holds a `sync.RWMutex`. Fetches `RLock`, pushes `Lock`. Mutexes are stored in a `sync.Map[string]*Mirror`; first access does `LoadOrStore`. Mutex map entries persist for the gateway lifetime in M3 — eviction is M9. The mutex is held only for the duration of one HTTP request; never across an HTTP-client read or a long-running bucket operation that could be cancelled by client disconnect (`context.Context` cancellation propagates).

### 7.3 Lazy materialization

`mirror.Open(ctx, tenant, repo) (*Mirror, error)`:
1. `LoadOrStore` the per-repo mutex+state.
2. Hold WLock during init; downgrade to RLock for the caller after init completes (or have caller re-acquire).
3. If `bare/` exists and `manifest_version.txt` matches `repo.ReadRoot(ctx)` → ready.
4. If `bare/` exists but stale → wipe and rebuild via M2 exporter.
5. If absent → call M2 exporter into `bare/`, write `manifest_version.txt`.
6. Run `git fsck --no-dangling --strict` after materialization (M2 exporter already does this).

### 7.4 Stale detection

Before serving any request, the gateway calls `repo.ReadRoot(ctx)` (one HEAD on the manifest) and compares the manifest version to `<mirror>/manifest_version.txt`. Different → mirror is stale, rebuild before serving. In M3 single-gateway scope, the only path that advances the bucket version is the gateway's own pushes, so this check is hot-path-fast (no rebuild). The check exists primarily as a correctness safety net for restart and future multi-gateway deployments.

### 7.5 Push update path (called under WLock)

`mirror.IngestPack(ctx, packPath, updates) error`:
1. `cp <packPath>.pack` and `<packPath>.idx` into `bare/objects/pack/`.
2. For each `RefUpdate{Refname, OldOID, NewOID}`:
   - If `NewOID` is zero → `git update-ref -d <Refname> <OldOID>`.
   - Else if `OldOID` is zero → `git update-ref <Refname> <NewOID>` (no old check — create).
   - Else → `git update-ref <Refname> <NewOID> <OldOID>`.
3. After all updates succeed, write new `manifest_version.txt`.

Caller must hold `m.Lock()`. Step 3 is the commit point of the local mirror state — earlier steps are reversible by removing the new pack and reverting refs (which the gateway will do if `repo.Commit` fails before `IngestPack` is called, though by current flow we call `IngestPack` only after `repo.Commit` succeeds).

### 7.6 Crash recovery

A gateway crash mid-push leaves the mirror in one of three states, all self-healing:

- **New pack copied, refs not updated, sentinel not bumped.** Mirror reads correctly with extra dangling pack. Next request: bucket version matches old sentinel, no rebuild. Dangling objects are harmless until `git gc --auto` (we don't run gc in M3; this accumulates as a known minor liability).
- **Refs updated, manifest bumped, sentinel not bumped.** Next request: bucket version > sentinel → rebuild. Slow but correct.
- **Bucket-side `repo.Commit` succeeded but `IngestPack` not yet run.** Next request: same as above, rebuild includes the just-pushed objects.

Correctness anchors at the bucket manifest. The mirror is reproducible.

### 7.7 No eviction in M3

Mirrors persist on disk indefinitely. Operator can `rm -rf <mirror-root>` to reset. LRU eviction by repo activity is M9 territory.

## 8. Concurrency layered controls

**Layer 1 — Per-repo `sync.RWMutex` (in-process).** Pushes serialize per repo; fetches share. Spec §18 "in-process queue for single gateway owner" — exact match.

**Layer 2 — Bucket-side `repo.Commit` CAS (M1).** The durable correctness primitive. Layer 1 is a scheduling optimization; Layer 2 is the authority. Spec §18: "the durable commit point remains the root manifest CAS … the queue/lease is a scheduling optimization only."

**Layer 3 — File-system advisory `flock` on mirror dir.** Process-level. Catches accidental misconfiguration (two daemons sharing one mirror dir).

**Lease/expiry:** none in M3. Distributed lease/queue mechanisms are M12.

**Liveness:** `context.Context` propagated through every git child process and bucket call. Client disconnect cancels the context, releases the mutex within milliseconds.

## 9. Auth (`internal/gateway/auth`)

```go
type AuthMode int
const (
    AuthAnonymous AuthMode = iota
    AuthWriteOnly
    AuthAll
)
type AuthOptions struct {
    Mode  AuthMode
    Token string
}
```

**Wire:** HTTP Basic with username `bucketvcs` and password = configured token. `Authorization: Basic <b64>`. Constant-time compare. Missing/wrong → `401 WWW-Authenticate: Basic realm="bucketvcs"`.

**Read vs write classification:** `service=git-upload-pack` query param or path-suffix `/git-upload-pack` → read. `service=git-receive-pack` or `/git-receive-pack` → write.

**Token storage:** plaintext in CLI flag or `BUCKETVCS_AUTH_TOKEN` env. Single token, single-process. Tokens redacted from all logs (M2 redaction patterns extend).

**No identity tracking.** M3 logs auth_mode and authenticated bool only. Per-user identity is M5+.

**No native TLS, rate limiting, replay protection.** Reverse proxy in front (caddy/nginx/etc.) is the documented safe-deploy pattern.

## 10. URL routing

Pattern: `/{tenant}/{repo}.git/...`. `tenant` and `repo` MUST match `^[A-Za-z0-9._-]+$` (matches M0/M1 key-component constraints; rejects path traversal, control chars, and shell metacharacters). The trailing `.git` is required to match real git client behavior.

Recognized paths:
- `GET  /{tenant}/{repo}.git/info/refs?service=git-upload-pack`
- `GET  /{tenant}/{repo}.git/info/refs?service=git-receive-pack`
- `POST /{tenant}/{repo}.git/git-upload-pack`
- `POST /{tenant}/{repo}.git/git-receive-pack`

Anything else under `/{tenant}/{repo}.git/` returns 404. The root `/` and `/healthz` are served as plain HTTP — `/healthz` returns `200 OK` for liveness probes; `/` returns a one-line text banner. No HTML, no UI.

Repo-not-found → 404. Tenant-or-repo with invalid character → 400. Auto-create-on-push deferred.

## 11. Server binary (`cmd/bucketvcs serve`)

```
bucketvcs serve [flags]

Flags:
  --addr <host:port>          listen address (default :8080)
  --store <url>               object store URL (e.g. localfs:/path/to/dir)
  --mirror-dir <path>         mirror cache directory (default $XDG_CACHE_HOME/bucketvcs/mirrors)
  --auth-token <secret>       optional bearer token (also via BUCKETVCS_AUTH_TOKEN env)
  --auth-scope <s>            "write-only" (default if --auth-token set) or "all"
  --max-body-bytes <n>        per-request body limit (default 1073741824 = 1 GiB)
  --max-concurrent-fetches    bound on simultaneous fetches (default unlimited)
  --log-format <fmt>          "text" (default) or "json"
  --shutdown-timeout <dur>    graceful shutdown deadline (default 30s)
```

Server lifecycle:
1. Parse flags, validate.
2. Open `ObjectStore` from `--store` URL.
3. Acquire `<mirror-dir>/lock` (refuse to start if held).
4. Construct `gateway.NewServer(store, opts)`.
5. `http.ListenAndServe(addr, server)`.
6. On `SIGTERM`/`SIGINT`: stop accepting new connections, wait up to `--shutdown-timeout` for in-flight requests, then exit. Mirror state is durable across restarts.

No daemonization, no PID file, no built-in TLS — all delegated to systemd / supervisord / reverse proxy.

## 12. Differential harness extension

M2 ships 11 fixtures × 2 oracles (round-trip, cat-object). M3 adds two oracles parameterized over the registry, and 5 new fixtures.

### 12.1 Oracle 3 — clone-equivalence

For each fixture:
1. Build fixture as bare repo `Aupstream`.
2. `bucketvcs import` → bucket-resident repo `B`.
3. Start gateway in-process (`httptest.NewServer`).
4. `git clone http://gateway/<tenant>/<repo>.git Adownstream` (real git binary, real HTTP).
5. Compare `Aupstream` vs `Adownstream` via M2's recursive cat-object diff.

### 12.2 Oracle 4 — push-equivalence

For each fixture:
1. Start gateway in-process; `bucketvcs init` an empty repo.
2. `git push http://gateway/<tenant>/<repo>.git --mirror` from fixture's bare repo.
3. `bucketvcs export` to `Aexported`.
4. Compare fixture (input) vs `Aexported` recursively.

### 12.3 New fixtures

| Fixture | Purpose |
|---|---|
| `force_push_overwrite` | non-FF push acceptance |
| `delete_branch` | `delete-refs` capability |
| `atomic_multi_ref_push` | `atomic` capability |
| `incremental_fetch_after_push` | `have`/`want` negotiation post-push |
| `shallow_clone_depth_1` | `fetch=shallow` capability |

Combined: 16 fixtures × 4 oracles = 64 oracle assertions per CI run.

### 12.4 Promotion rule activation

Spec §40.3: pure-Go serving path becomes default after 100% differential pass + 4-week shadow. M3 ships with 100% pass; the 4-week shadow is operator/release discipline (CI keeps the harness green on every change). `docs/superpowers/diffharness/known-divergences.md` stays empty at M3 ship; any divergence must be fixed or land as a documented entry through M2's format-gate test.

### 12.5 Stress smoke (build-tag `+build stress`)

A 1000-commit synth repo gets fully pushed via gateway and asserted to complete < 60 s on a dev box. Not a ship gate; regression alarm matching M2's stress posture.

## 13. Failure modes and what the client sees

| Failure point | Client report |
|---|---|
| Auth missing/wrong | HTTP 401 |
| Repo not found | HTTP 404 |
| Tenant/repo invalid character | HTTP 400 |
| Body exceeds `--max-body-bytes` | HTTP 413 |
| Pack integrity (`git index-pack --strict` fails) | `unpack invalid-pack: <message>` |
| Connectivity (`git fsck --connectivity-only` fails) | `unpack missing-connectivity` |
| Stale `old-oid` | `ng <ref> stale info` |
| Refname format | `ng <ref> bad-ref-name` |
| `refs/replace/*` write | `ng <ref> replace-refs-not-allowed` |
| Atomic batch failure | `ng <ref> atomic-batch-failed` for all refs |
| Bucket upload error | `ng <ref> internal-storage-error` |
| `repo.Commit` CAS persistent fail (after retry) | `ng <ref> stale-manifest` |
| Gateway crash mid-flow | client sees connection drop; mirror self-heals on next request |

**Partial-failure stranded state.** A push that uploads pack/.bvom/.bvcg to bucket but fails at `repo.Commit` leaves orphaned objects in the bucket (same shape as M2's stranded importer state). Orphans are harmless (nothing references them) and will be swept by M8 GC. Atomic create-with-body primitive (collapse upload + manifest CAS into one logical step) is a candidate future API.

## 14. Testing strategy

### 14.1 Unit tests

- `internal/pktline`: round-trip framing, oversized-frame rejection, special markers, side-band channel handling.
- `internal/v2proto`: capability advertisement byte exact, `ls-refs` against synthetic `manifest.Body`, `fetch` argument parsing, error paths (unknown OID, malformed input).
- `internal/mirror`: lazy materialize, stale detection, IngestPack, crash-recovery scenarios (pre-built corrupted-mirror states).
- `internal/gateway/auth`: each AuthMode × (no-auth-header | wrong-token | right-token) × (read | write).
- `internal/gateway` routing: each path pattern × {happy, 404, 400}.

### 14.2 Integration tests (`httptest.NewServer`)

- Each protocol command flow end-to-end with real `git` binary as the client.
- Concurrent push serialization: two `git push` against same repo, assert both succeed, assert second sees first's commit as old-oid via stale-info if not staged.
- Auth modes: 9 combinations.
- Empty-pack delete-only push.
- Shallow clone depth 1.

### 14.3 Differential harness (Section 12)

64 oracle assertions per CI run. Hard ship gate.

### 14.4 Static analysis

`go vet`, `staticcheck`, `gofmt -l`. M2 ship-gate posture continues.

### 14.5 Stress smoke

`go test -tags stress ./internal/gateway/...`. Not a ship gate.

## 15. Public APIs M4+ will consume

- `gateway.NewServer(store storage.ObjectStore, opts gateway.Options) *gateway.Server` — entry point for embedding.
- `gateway.Options{ MirrorDir, AuthMode, AuthToken, MaxBodyBytes, ... }` — option struct, fields additive in future milestones.
- `mirror.Open(ctx, store, tenant, repo) (*mirror.Mirror, error)` — exposed for M9 background maintenance to drive repacks.
- `pktline.Reader`, `pktline.Writer` — exposed for SSH transport in M5 to reuse framing.
- `v2proto.Caps` — exposed so future capabilities can be added in their own milestones.

## 16. Architecture invariants M4+ MUST honor

- HTTP URL pattern is `/{tenant}/{repo}.git/...` with trailing `.git` required. Don't change without coordinating client compatibility.
- Mirror sentinel format (`manifest_version.txt`) is a single-line opaque string matching `manifest.Body`'s version representation. Format change requires migration logic in `mirror.Open`.
- The gateway never holds a per-repo mutex across an external HTTP-client byte read or a long bucket operation that could block on client cancellation. `context.Context` cancellation must always release the lock promptly.
- All `gitcli` invocations pass `--no-replace-objects` (M2 invariant continues).
- v2 capabilities advertised at ship are a stable contract for client compatibility. Adding capabilities is fine; removing or changing semantics requires coordinated rollout.
- `repo.Commit` is the only path that advances the manifest. The mirror is always derivable. Don't add gateway-local persistent state that isn't reproducible from the bucket.
- `manifest.Body` golden file unchanged in M3 — the body shape is identical to M2 ship.

## 17. Open issues / risks

**R1: Mirror dir disk pressure.** Repos pile up. M3 has no eviction. If an operator runs many repos through one gateway, mirror dir grows unbounded. Mitigations: documented `--mirror-dir` location, operator can `rm -rf` to clear. M9 will add LRU eviction.

**R2: First-fetch latency on cold mirror.** Materializing a 1 GiB repo via the M2 exporter takes time before the first byte goes to the client. M3 accepts this; B3-style request-time caching is a future optimization.

**R3: Shallow clone correctness.** Most fiddly v2 area; depth-N covered, time-based and ref-based shallow may produce conservatively-large packs. Differential harness has one shallow fixture; expand if real users report issues.

**R4: Force-push without policy.** M3 accepts force pushes silently. M14 will tighten with branch protection. Operators should put a reverse proxy or commit-message-policy in front if they need protection before M14.

**R5: Partial-failure stranded objects.** Same shape as M2's stranded importer state. M8 GC will sweep. Atomic create-with-body primitive is a candidate future API.

**R6: Single shared token has no rotation story.** M3's auth is "stop the gate"; a real production deploy needs rotation, which means M5 identity/auth.

**R7: `git pack-objects` resource consumption.** A malicious or unbounded `wants` list can spike CPU/RAM on the gateway. M3 mitigates with the 100k wants cap and `--max-concurrent-fetches`. Production-grade resource shaping is M5+.

## 18. Summary of architectural decisions

| Decision | Choice | Why |
|---|---|---|
| Protocol implementation strategy | Track B (pure-Go protocol, shell-out for pack assembly) | Continues M2 precedent; differential-harness verifiable |
| Mirror lifecycle | B2 (per-repo on-disk mirror kept in sync) | B1's cold-fetch cost unusable; B3's invalidation more complex than B2's incremental update for no gain in single-binary case |
| Protocol versions | v2-only (v0 receive-pack unchanged because v2 doesn't redefine push) | Halved code surface; v2 matches stateless cloud-storage-native model; v1 fallback is additive in future if needed |
| Auth | A2 (optional shared bearer token, configurable scope) | Usable single-binary out of box; defers user/role modeling to M5 cleanly |
| Push validation | `index-pack --strict` + `fsck --connectivity-only` | Matches git's own receive-pack default; cheap on local mirror |
| Push concurrency | In-process per-repo `sync.RWMutex` + bucket CAS | Spec §18 "in-process queue for single gateway owner" exact match |
| `.bvom`/`.bvcg` rebuild after push | Full rebuild over all packs | Simple and correct; incremental merge is M9 perf optimization |
