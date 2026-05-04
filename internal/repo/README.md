# `internal/repo`

The M1 thin transaction kernel: the only place in the codebase that
atomically advances a repo from one durable state to the next. Sits
between [`internal/storage`](../storage) (M0) and the future Git object
engine (M2).

## Status

M1 ships:

- `Repo` handle with `Open`, `Create`, `ReadRoot`, `Commit`
- [`internal/repo/keys`](keys) — constructors for the entire §6 path layout
- [`internal/repo/manifest`](manifest) — `RootHeader` struct + §43.7 schema gate +
  `ReadRoot` / `CASRoot` / `WrapHeaderInBody` helpers
- [`internal/repo/tx`](tx) — header/body split + `PutIfAbsent` writer
- [`internal/repo/repoerrs`](repoerrs) — leaf package holding the canonical
  sentinel errors and `CommitGaveUpError`; `internal/repo` re-exports them
- `bucketvcs init` and `bucketvcs inspect-manifest` CLI subcommands

## What this package owns

1. **The §6 key naming contract.** Every path inside
   `/tenants/{tenant_id}/repos/{repo_id}/` is constructed via `keys`.
   M2/M3/M8 do not invent paths.
2. **The §7 root-manifest CAS.** `Commit` reads `manifest/root.json`,
   invokes the caller's `buildBody` callback against a snapshotted view,
   splices in M1-owned header fields, and atomically swaps the root via
   `ObjectStore.PutIfVersionMatches`.
3. **The §8 transaction-record-then-CAS ordering.** Each `Commit`
   attempt mints a fresh ULID, writes the immutable tx record (via
   `tx.Write`'s `PutIfAbsent`), then attempts the CAS. On conflict,
   retry with a fresh tx_id; on exhaustion, return `*CommitGaveUpError`
   carrying the orphan IDs.

## What this package does NOT own

- Refs, pack content, reachability indexes, bundles — M2.
- Sharded refs (`manifest/ref-shards/`) — M12.
- Garbage collection of orphan tx records — M8.
- Git protocol surface — M3.
- Authentication / tenants — M4 / commercial scope.

## Concurrency-test conformance bar

`internal/repo/internal/repo_concurrency_test.go` ships the property
test as `RunPropertyManifestVersionMonotonic(t, factory)`, parameterized
over an `ObjectStore` factory. The localfs-backed call
`TestCommit_PropertyManifestVersionMonotonic` is the M1 ship gate per
the design doc §8.3.

**Cloud adapters at M5 (R2 or S3) and M7 (the others) MUST run this
same suite against their backend before claiming conformance.** They
provide their own factory and call `RunPropertyManifestVersionMonotonic`.

A heavier stress test (`TestCommit_Stress`) is gated behind the
`stress` build tag:

```bash
go test -race -tags stress -count=1 ./internal/repo/internal/... \
  -run TestCommit_Stress -timeout 15m
```

## Adding a Commit caller

```go
import "github.com/bucketvcs/bucketvcs/internal/repo"
import tx "github.com/bucketvcs/bucketvcs/internal/repo/tx"

txID, err := r.Commit(ctx, tx.Body{Type: "push", Actor: actor},
    func(prev *repo.RootView) ([]byte, error) {
        // Caller mutates the body — refs, packs, etc. — and returns
        // the new body bytes. M1 splices in header fields; the caller
        // must NOT include header keys in the returned bytes.
        return mutate(prev.Body)
    },
)
```

The callback may be invoked multiple times (once per CAS attempt).
Make the callback idempotent against `prev`; do not let it depend on
state captured before the first call.

The `prev *RootView` passed to the callback contains an M1-owned
snapshot of the header and version. Mutating `prev.Header` or
`prev.Version` from inside the callback is harmless — `Commit` uses
its own snapshot for the CAS.

## Schema gate

`schema_version 1` is the only value M1 reads or writes. Manifests with
`schema_version > 1` or `min_reader_version > 0.1.0` are rejected with
`repoerrs.ErrUnsupportedSchema` (re-exported as
`repo.ErrUnsupportedSchema`). When a real schema bump lands at M2+,
extend `manifest.CurrentSchemaVersion` and update the gate.

The gate runs inside `manifest.ReadRoot` BEFORE the full `RootHeader`
is unmarshalled — only `schema_version` and `min_reader_version` are
parsed first. This way a future-schema manifest with an incompatible
field-type change in another header field still fails with
`ErrUnsupportedSchema` rather than a generic parse error.
