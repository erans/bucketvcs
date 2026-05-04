# M1 — Repository state engine

**Status:** design approved 2026-05-03; implementation plan to follow.
**Depends on:** M0 (`internal/storage` ObjectStore + localfs adapter + conformance suite, merged at commit `718c0f4`, tag `m0-complete`).
**Spec sections:** §6 durable repository model, §7 root manifest, §8 immutable transaction records, §43.7 manifest schema migration, §40.1 (`internal/repo`).
**Decomposition row:** M1 — "library that creates/reads/updates a repo's durable state, demonstrably correct under concurrent CAS contention" (`docs/superpowers/specs/2026-05-03-bucketvcs-oss-decomposition-design.md`).

## 1. Purpose and boundary

M1 is a **thin transaction kernel**: the only place in the codebase that knows how to atomically advance a repo from one durable state to the next. It sits between `internal/storage` (M0) and the future Git object engine (M2).

It owns three things:

- **The §6 key naming contract.** Every path inside `/tenants/{tenant_id}/repos/{repo_id}/` is constructed via `internal/repo/keys`. M2/M3/M8 do not invent paths.
- **The §7 root-manifest CAS.** Read root, build new manifest, swap atomically via `ObjectStore.PutIfVersionMatches`.
- **The §8 transaction-record-then-CAS ordering.** Every state change writes an immutable `tx/{tx_id}.json` first, then attempts the root swap.

It explicitly does **not** know about: refs (M2), pack content (M2), reachability (M2), GC (M8), Git protocol (M3), auth (M4). Those layers consume M1's primitives.

## 2. Package layout

```
cmd/bucketvcs/
  main.go              # cobra root, wires `init` + `inspect-manifest`
  init.go
  inspect.go

internal/repo/
  repo.go              # Repo handle: Open, Create, Commit, ReadRoot
  errors.go            # typed sentinel errors
  keys/
    keys.go            # constructors for every §6 path
    keys_test.go
  manifest/
    header.go          # RootHeader struct + JSON marshal
    schema.go          # schema gate (§43.7 fail-closed)
    cas.go             # readRoot / casRoot helpers (operate on bytes)
    manifest_test.go
  tx/
    record.go          # TxHeader + TxBody types; canonical JSON marshal
    write.go           # writeTxRecord (PutIfAbsent)
    record_test.go
  internal/
    repo_concurrency_test.go    # property + scenario + stress on localfs
```

`internal/repo/refs`, `internal/repo/reachability`, `internal/repo/packindex`, `internal/repo/gc` from §40.1 are **not created** in M1. They land at M2 / M8.

## 3. Public API

### 3.1 Repo handle

```go
type Repo struct {
    store    storage.ObjectStore
    tenantID string
    repoID   string
    keys     *keys.Repo  // pre-bound to the (tenant, repo) prefix
}

type CreateOptions struct {
    DefaultBranch string  // default "refs/heads/main"
    ObjectFormat  string  // "sha1" only in M1; "sha256" reserved
    Actor         string  // recorded in the create tx record
}

// Create writes the initial tx record + root manifest.
// Returns ErrRepoExists if root.json already present.
func Create(ctx context.Context, store storage.ObjectStore,
    tenantID, repoID string, opts CreateOptions) (*Repo, error)

// Open returns a handle for an existing repo.
// ErrRepoNotFound if root absent.
// ErrUnsupportedSchema if header fails the §43.7 gate.
func Open(ctx context.Context, store storage.ObjectStore,
    tenantID, repoID string) (*Repo, error)

// ReadRoot returns the current root manifest header + body bytes
// + opaque version token.
func (r *Repo) ReadRoot(ctx context.Context) (*RootView, error)

type RootView struct {
    Header    manifest.RootHeader
    Body      json.RawMessage         // refs, packs, indexes, bundles, ...
    Version   storage.ObjectVersion
    SizeBytes int64
}

// Commit performs the §8 atomic-pair: write tx record, then CAS root.
// On CAS conflict, Commit re-reads root, mints a fresh tx_id, and
// re-invokes buildBody with the new prev (bounded retries).
// Returns the *winning* tx_id.
func (r *Repo) Commit(
    ctx context.Context,
    txBody tx.Body,
    buildBody func(prev *RootView) (newBody []byte, err error),
    opts ...CommitOption,
) (txID string, err error)

type CommitPolicy struct {
    MaxRetries  int           // default 8
    BackoffBase time.Duration // default 5ms (jittered)
}
type CommitOption func(*CommitPolicy)
func WithCommitPolicy(p CommitPolicy) CommitOption
```

### 3.2 Manifest header

```go
// internal/repo/manifest/header.go
type RootHeader struct {
    SchemaVersion    int       `json:"schema_version"`
    MinReaderVersion string    `json:"min_reader_version"`
    RepoID           string    `json:"repo_id"`
    RepoFormat       Format    `json:"repo_format"`
    ManifestVersion  uint64    `json:"manifest_version"`
    LatestTx         string    `json:"latest_tx"`
    CreatedAt        time.Time `json:"created_at"`
    UpdatedAt        time.Time `json:"updated_at"`
}

type Format struct {
    ObjectFormat  string   `json:"object_format"`            // "sha1"
    Compatibility []string `json:"compatibility,omitempty"`  // ["sha1"]
}
```

`RootHeader` is the *only* part of the manifest M1 owns. Everything else (refs, packs, indexes, bundles, default_branch, tx-related body fields) is M2's concern. Per §7 the example, `created_at`/`updated_at` are top-level (header-owned): `created_at` is set once by `Create`; `updated_at` is rewritten by every successful `Commit`.

### 3.3 Tx record body

```go
// internal/repo/tx/record.go
type Body struct {
    Type       string          `json:"type"`        // "create" | "push" | "gc" | future
    Actor      string          `json:"actor"`
    RefUpdates json.RawMessage `json:"ref_updates,omitempty"`
    NewPacks   json.RawMessage `json:"new_packs,omitempty"`
    Validation json.RawMessage `json:"validation,omitempty"`
    Extra      json.RawMessage `json:"-"` // merged into top-level JSON
}
```

M1 wraps the body with a header (`schema_version`, `tx_id`, `repo_id`, `base_manifest_version`, `base_manifest_object_version`, `started_at`) at write time.

### 3.4 Errors

```go
var (
    ErrRepoExists        = errors.New("repo: root manifest already exists")
    ErrRepoNotFound      = errors.New("repo: root manifest not found")
    ErrUnsupportedSchema = errors.New("repo: schema or min_reader_version exceeds supported")
    ErrCallbackFailed    = errors.New("repo: buildBody callback returned error")
    ErrInvalidTenantID   = errors.New("repo: tenant_id invalid")
    ErrInvalidRepoID     = errors.New("repo: repo_id invalid")
)

type CommitGaveUpError struct {
    Attempts    int
    OrphanTxIDs []string
    LastErr     error
}
func (e *CommitGaveUpError) Error() string
func (e *CommitGaveUpError) Unwrap() error
```

Storage-layer errors (`storage.ErrNotFound`, `storage.ErrVersionMismatch`, etc.) are wrapped with `fmt.Errorf("repo: %w", err)` when they leak through, so callers can still `errors.Is(err, storage.ErrNotFound)`.

## 4. Commit data flow

```
caller calls repo.Commit(ctx, txBody, buildBody [, policy])
  │
  ├── Read root.json (bytes, version, parsed RootHeader).
  │   schema gate: refuse if SchemaVersion > 1 or MinReaderVersion > supported.
  │   on missing root: ErrRepoNotFound.
  │
  ├── Retry loop (default MaxRetries = 8):
  │     1. txID := newULID()
  │     2. prev := readRoot()                     # re-read each attempt
  │     3. schemaGate(prev.Header)
  │     4. newBody := buildBody(prev)             # caller supplies
  │     5. txRecord := txHeader{
  │            schema_version: 1,
  │            tx_id: txID,
  │            repo_id: r.repoID,
  │            base_manifest_version:        prev.Header.ManifestVersion,
  │            base_manifest_object_version: prev.Version.Token,
  │            started_at: now(),
  │        } merged with caller's tx.Body
  │     6. store.PutIfAbsent(tx/{txID}.json, txRecord)   # always succeeds (fresh ULID)
  │     7. nextHeader := prev.Header with:
  │            ManifestVersion = prev.Header.ManifestVersion + 1
  │            LatestTx        = txID
  │            UpdatedAt       = now()
  │     8. nextBytes := wrapHeaderInBody(nextHeader, newBody)
  │     9. _, err := store.PutIfVersionMatches(root, nextBytes, prev.Version)
  │    10. on err == nil:                  return txID, nil
  │        on ErrVersionMismatch:          continue          # orphan tx_id stays on disk
  │        on other error:                 return "", err    # propagate (orphan stays)
  │
  └── Retry budget exhausted:
        return "", &CommitGaveUpError{
            Attempts: N, OrphanTxIDs: [...], LastErr: storage.ErrVersionMismatch
        }
```

### 4.1 Invariants enforced

- Tx record is always written **before** the CAS attempt (§8).
- `manifest_version` strictly monotonically increases by 1 per successful commit.
- `latest_tx` always points to a tx record that exists on disk (because `PutIfAbsent` succeeded before CAS).
- Each tx record's `base_manifest_version` and `base_manifest_object_version` accurately reflect the root state read **immediately before** that record's CAS attempt — never stale due to retries (because each attempt mints a new `tx_id`).
- Tx records are immutable after write. M1 never touches a tx record after the `PutIfAbsent` returns.

### 4.2 Orphan tx records

A "lost" CAS attempt leaves its tx record on disk, unreferenced by any committed root. M1 makes **no attempt** to clean these up. Rationale:

1. §8 declares tx records immutable — M1 must not delete them.
2. M8 mark/sweep handles orphans per §43.6, which has the retention-window context M1 lacks.
3. Orphans carry diagnostic value: a contention storm is visible as a cluster of orphans with the same `base_manifest_version`.

Under realistic load, retries are rare and orphan accumulation is bounded. Pathological contention is the M8 GC's job to clean up on its sweep schedule.

### 4.3 Create carve-out from §8 ordering

`Create` is the only operation that violates "tx record before CAS." It checks for existing root via `PutIfAbsent` on `manifest/root.json` *first*, and only writes the create-tx record if the root write succeeded. The reason: there is no prior root to CAS against, and writing a tx record for a `Create` that turns out to be a duplicate would generate a useless orphan on every accidental re-`init`. The carve-out is documented in `repo.go` and tested.

## 5. Schema versioning (§43.7)

M1 ships **schema_version 1 only** plus the §43.7 fail-closed gate.

- Writers emit `schema_version: 1` and `min_reader_version: <M1 build version>` (currently `"0.1.0"`).
- Readers parse the header before any other field and refuse with `ErrUnsupportedSchema` if `SchemaVersion > 1` or `semver.Compare(MinReaderVersion, SupportedReaderVersion) > 0`.
- The gate is **asymmetric** (matches M0 precedent at `localfs/c639aa8`): only future versions fail closed; v1 is the only valid current version, and there is no v0.
- No migration code is shipped. The "lazy migration on write" hook point is documented as: when a future M2+ schema bump lands, the `Commit` flow will re-emit at the writer's current version on every successful CAS — which is already what happens, since `nextHeader.SchemaVersion` is set to the writer's constant on every commit. No separate migration code path is needed.

A test fixture writes a synthetic `schema_version: 999` manifest and confirms `Open` rejects it with `ErrUnsupportedSchema`.

## 6. Keys package

`internal/repo/keys` exports a constructor for every §6 path. M1 only writes through `RootManifestKey` and `TxRecordKey`; the rest exist so M2/M8/M13/M14 cannot drift on naming.

```go
type Repo struct {
    tenantID, repoID string
    prefix           string  // "tenants/{tid}/repos/{rid}/"
}

func NewRepo(tenantID, repoID string) (*Repo, error)  // validates IDs

// Used by M1
func (r *Repo) RootManifestKey() string                          // manifest/root.json
func (r *Repo) TxRecordKey(txID string) string                   // tx/{tx_id}.json

// Used by M2+
func (r *Repo) RefShardKey(shardHash string) string              // manifest/ref-shards/{hash}.json (M12)
func (r *Repo) CanonicalPackKey(packHash string) string          // packs/canonical/{hash}.pack
func (r *Repo) PackIdxKey(packHash, area string) string          // packs/{area}/{hash}.idx
func (r *Repo) PackBitmapKey(packHash, area string) string       // packs/{area}/{hash}.bitmap
func (r *Repo) GeneratedPackKey(packHash string) string          // packs/generated/{hash}.pack
func (r *Repo) CommitGraphKey(graphHash string) string           // indexes/commit-graphs/{hash}.graph
func (r *Repo) ReachabilityKey(indexHash string) string          // indexes/reachability/{hash}.json
func (r *Repo) BundleKey(bundleID string) string                 // bundles/{id}.bundle
func (r *Repo) BundleManifestKey(bundleID string) string         // bundles/{id}.json
func (r *Repo) LFSObjectKey(sha256 string) string                // lfs/objects/{sha256}
func (r *Repo) HookKey(hookID, name string) string               // hooks/{hook_id}/{name}
func (r *Repo) GCMarkKey(markID string) string                   // gc/marks/{mark_id}.json
func (r *Repo) GCSweepKey(sweepID string) string                 // gc/sweeps/{sweep_id}.json

// Validation helpers
func IsValid(key string) bool
func ParseKey(key string) (Parsed, error)
```

ID validation: tenant_id and repo_id must match `^[A-Za-z0-9_-]{1,128}$` and not contain path-traversal sequences. Hashes are validated for hex/charset by the constructors that take them.

## 7. CLI

Two subcommands under `cmd/bucketvcs`. Both take `--store=localfs:<path>` (only `localfs` URL scheme supported in M1; URL parsing extensible for cloud adapters at M5).

### 7.1 `bucketvcs init <tenant> <repo>`

```
bucketvcs init acme my-repo --store=localfs:/var/lib/bucketvcs
```

- Calls `repo.Create(ctx, store, "acme", "my-repo", CreateOptions{...})`.
- Flags: `--store` (required), `--actor` (default `$USER` or `unknown`), `--default-branch` (default `refs/heads/main`).
- Exit: 0 on success; 1 + stderr on `ErrRepoExists` or store error.

### 7.2 `bucketvcs inspect-manifest <tenant> <repo>`

```
bucketvcs inspect-manifest acme my-repo --store=localfs:/var/lib/bucketvcs
```

Default human format prints schema_version, repo_id, manifest_version, latest_tx, ref/pack/index/bundle counts, created_at, updated_at. Body parsing is intentionally lenient — counts are derived from JSON array/object length only, since M1 doesn't know the shape of body sub-objects.

`--json` emits the raw root manifest body for tooling.

Exit: 0 on success; 2 on `ErrRepoNotFound`; 3 on `ErrUnsupportedSchema`; 1 on other errors.

## 8. Testing strategy

### 8.1 Unit tests

Co-located with each package:

- `keys/keys_test.go` — every constructor; round-trip `ParseKey`/`IsValid`; reject paths that escape the repo prefix.
- `manifest/header_test.go` — JSON round-trip; schema gate accepts v1, rejects v999, rejects min_reader_version > supported.
- `manifest/cas_test.go` — `readRoot` returns `ErrRepoNotFound` on missing root; `casRoot` maps `storage.ErrVersionMismatch` correctly.
- `tx/record_test.go` — header/body merge produces correct top-level JSON; tx_id ULID format validation; `PutIfAbsent` semantics surface as `storage.ErrAlreadyExists` (impossible in production with fresh ULIDs, but tested with a forced collision).

### 8.2 Integration tests

`internal/repo/repo_test.go`:

- `Create` writes both tx record and root manifest; `Open` returns the repo afterward.
- `Create` on existing repo returns `ErrRepoExists`; no orphan tx record produced (per the §4.3 carve-out).
- `Open` on missing repo returns `ErrRepoNotFound`.
- `Open` on synthetic future-schema fixture returns `ErrUnsupportedSchema`.
- `Commit` happy path: tx record exists at predicted key; root.json reflects bumped manifest_version + new latest_tx + caller's body changes.
- `Commit` callback error: tx record *not* written (callback runs before `PutIfAbsent`); err wraps `ErrCallbackFailed`.
- `inspect-manifest` CLI on a freshly-created repo prints expected fields.

### 8.3 Concurrency suite — the M1 ship gate

`internal/repo/internal/repo_concurrency_test.go`:

**Property test** (`TestCommit_PropertyManifestVersionMonotonic`):
N=8 goroutines × M=200 commits, all on the same repo backed by localfs. Each commit sets a unique key in the body's `extra`. Assertions after join:
- `manifest_version == 1 + N×M` in final root.
- `latest_tx` references a real on-disk tx record.
- For every committed tx_id, the tx record's filename equals its `tx_id` field.
- Orphan count = total tx records on disk − committed count, equals sum of observed retries (recorded via instrumentation hook).
- No torn header bytes — every tx record and every root.json snapshot during the run parses cleanly.

**Scenario tests** (each `TestCommit_Scenario_*`):
- `TwoWritersOneWins` — race two CAS attempts; assert exactly one wins on first attempt, loser retries and wins on second; both tx_ids exist; only winner's referenced.
- `CtxCancelMidCommit` — cancel ctx after callback returns but before `PutIfVersionMatches`; assert tx record exists, root unchanged, error wraps `context.Canceled`.
- `CallbackErrorAborts` — callback returns sentinel error; assert no tx record written, root unchanged, error wraps `ErrCallbackFailed`.
- `RetryBudgetExhausted` — stub builder that always loses CAS by N+1 writes between attempts; assert `*CommitGaveUpError` with N orphan tx_ids matching disk.
- `ReadDuringWrite` — reader goroutine spins on `ReadRoot` for 2s while writers commit; reader sees only valid root snapshots.

**Stress** (`TestCommit_Stress`, behind `// +build stress`):
100 goroutines × 1000 commits, `go test -race -count=10`. Pass criterion: zero data races, zero failures, runtime < 60s on a laptop.

`internal/repo/README.md` records: cloud adapters at M5 / M7 MUST run this same suite against their backend before claiming conformance.

## 9. Boundaries

### 9.1 Explicit non-goals for M1

- Any interpretation of refs, pack entries, indexes, or bundles. M1 reads/writes the body as opaque JSON.
- Fast-forward / force / delete ref-update semantics — M2 (§19.1).
- Pack content I/O, pack indexes, bitmaps, commit-graphs, reachability — M2.
- Sharded-refs mode (`manifest/ref-shards/`) — M12. The `RefShardKey` constructor exists but is never called by M1.
- GC mark/sweep, orphan tx record cleanup — M8.
- Multipart-aware writes (root manifest and tx records are small).
- Migration code for v1→v2.
- HTTP/SSH/protocol surface — M3+.
- Auth / tenant management — M4+, commercial scope.
- Differential harness against upstream Git — M2.

### 9.2 Contracts M2 inherits

- The §6 key naming layout via `internal/repo/keys`. M2 must not invent its own key constructors for pack/index/bundle paths.
- The `Repo.Commit(buildBody)` callback as the only way to advance repo state. Direct `store.PutIfVersionMatches` on root.json is forbidden outside M1.
- The header/body split: M2 owns body schema (`refs`, `packs`, `indexes`, `bundles`, `default_branch`); M1 owns header (including `created_at`/`updated_at`).
- Tx record body schema: M2 fills in `type`, `ref_updates`, `new_packs`, `validation` per §8.

### 9.3 Carry-forward open questions

- Sharded-refs threshold and switchover protocol — M12.
- Bulk schema migration tool — deferred until first real v2 schema lands.
- Per-repo Commit serialization stronger than in-process — M3 (§18) for HTTP, M12 for distributed.
- Module path `github.com/bucketvcs/bucketvcs` is a placeholder pending governance gate G1.
