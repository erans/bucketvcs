# M2 — Git object engine

**Status:** design draft 2026-05-04; implementation plan to follow.
**Depends on:** M1 (`internal/repo` transaction kernel, merged at commit `65db4c3`, tag `m1-complete`), which depends on M0 (`internal/storage` ObjectStore + localfs adapter + conformance suite).
**Spec sections:** §14 (basic only), §15.1, §19.1, §20 (SHA-1), §21, §34, §40.3.
**Decomposition row:** M2 — "`bucketvcs import` and `bucketvcs export` round-trip a bare git repo with `git fsck` clean on both ends; first differential tests run against upstream git" (`docs/superpowers/specs/2026-05-03-bucketvcs-oss-decomposition-design.md`).

## 1. Purpose and boundary

M2 is the **Git object engine** layered on M1's transaction kernel. It owns four things:

1. **Pack handling on object storage.** A pure-Go random-access reader over Git's native `.pack`/`.idx` v2 format, designed to read from `storage.ObjectStore` (range GETs, not local files).
2. **Index objects M3 will consume.** An object-to-pack map (`.bvom`) and a commit graph (`.bvcg`, parent edges + ref tips), both content-addressed, both referenced from the root manifest body.
3. **Import/export.** `bucketvcs import` round-trips a bare Git repo into bucketvcs storage; `bucketvcs export` round-trips it back. `git fsck` clean on both ends.
4. **Differential-harness scaffolding.** Round-trip oracle (import → export → byte-compare via upstream `git`) plus pack-reader oracle (`bucketvcs cat-object` vs `git cat-file`) on a synthetic in-test fixture corpus.

M2 produces **inline refs** (§19.1) and **one canonical pack per import** (§15.1). M9 turns the latter into a real "small-append-style packs + maintenance" story; M2 is content with one well-formed pack at rest.

### 1.1 Track A vs Track B for M2

Per spec §40.3, Track A (upstream `git` as oracle/temporary helper) and Track B (pure-Go) are not an either/or; migration is gradual per code path. M2's choice:

- **Track A on the import/export side.** `bucketvcs import` shells out to `git pack-objects --all` for pack production, `git fsck --strict` for input validation, and other plumbing. `bucketvcs export` uses `git init`, `git index-pack`, `git update-ref`, `git fsck`. Rationale: M2's value is "data round-trips cleanly"; we get there fastest by letting upstream `git` do the validation and packing it already does correctly.
- **Track B on the read side (M3-consumed layer).** The pure-Go pack reader is the actually-novel contribution — nobody else has done pack indexing against bucket primitives. M3 will build fetch negotiation directly on top of this reader; it must exist as Go code in M2 so the differential harness can compare it against `git cat-file` from day one.

The differential harness gets full teeth on the read path (`bucketvcs cat-object` vs `git cat-file`).

### 1.2 What M2 explicitly does not own

- Git protocol / pkt-line / negotiation — M3.
- Push receive path — M3.
- Auth — M4.
- Cloud-backend storage — M5/M7. M2's `--store` only accepts `localfs:`.
- Reachability *compaction*, base+delta splits, partitioning, mini-bitmaps — M10.
- Background repack / multi-pack consolidation / bitmaps / multi-pack-index / generated-vs-canonical promotion — M9.
- Adoption of Git's commit-graph file format — M9, if benchmarks justify swapping out M2's `.bvcg`.
- Bundle URI / packfile URI — M11.
- Sharded refs (§19.2) and resharding (§19.3) — M12.
- LFS — M13.
- Hooks / policy / webhooks / audit — M14/M15.
- Garbage collection of failed-import orphans, old tx, superseded packs — M8.
- Repair tooling (`bucketvcs doctor`, manifest reconstruction) — M16.
- SHA-256 repository support — deferred per §20; `OID` in M2 is a fixed `[20]byte`.

## 2. Package layout

```
internal/pack/                   pure-Go pack reader (the M3-consumed layer)
  reader.go                      open .pack + .idx, random-access by OID
  index.go                       parse pack-*.idx v2 (fanout + sorted OID + offsets + CRC)
  object.go                      decode object header, type, inflate; resolve REF_DELTA / OFS_DELTA
  store_source.go                io.ReaderAt over storage.ObjectStore range GETs
  cache.go                       small bounded LRU for index pages + delta-base objects
  reader_test.go
  conformance_test.go            run reader against a corpus of git-produced packs

internal/objindex/               object-to-pack map (M2 reachability index, part 1)
  format.go                      file format + (un)marshal
  build.go                       build from one or more pack readers
  read.go                        random-access lookup OID -> (pack_id, offset)
  format_test.go

internal/commitgraph/            commit graph (M2 reachability index, part 2)
  format.go                      M2-local format: OID -> []parent_OID + ref tips list
  build.go                       walk commits in a pack via pack reader, write graph
  read.go                        random-access OID -> parents
  format_test.go

internal/importer/               import orchestrator
  importer.go                    Import(ctx, store, opts) (*Result, error)
  importer_test.go

internal/exporter/               export orchestrator
  exporter.go                    Export(ctx, store, opts) (*Result, error)
  exporter_test.go

internal/gitcli/                 thin wrappers around the upstream git binary (Track A side)
  gitcli.go                      InitBare, PackObjectsAll, IndexPack, UnpackObjects,
                                 UpdateRef, Fsck, CatFile, ShowRef, Version
  gitcli_test.go                 verifies version + that required subcommands exist
                                 (skipped if git not on PATH; CI requires git ≥ 2.40)

internal/diffharness/            differential harness library (used by tests)
  fixtures/                      synthetic-repo builders
  fixtures.go                    registry: name -> Builder
  oracle.go                      git-CLI oracle wrappers
  roundtrip.go                   ImportThenExportAndCompare(t, fixtureName)
  catobject.go                   bucketvcs cat-object oracle
  README.md                      how to add a fixture / a new oracle assertion

cmd/bucketvcs/                   (existing M1 binary; M2 adds three subcommands)
  import.go
  export.go
  catobject.go                   bucketvcs cat-object <oid>  (debug, oracle target)
```

### 2.1 M1 boundary respected

M2 imports `internal/repo`, `internal/repo/keys`, `internal/repo/manifest`, `internal/repo/tx`, and `internal/storage`. It does **not** create new packages under `internal/repo/`. Where the M1 progress note carved out future homes (`internal/repo/refs`, `internal/repo/reachability`, `internal/repo/packindex`), those names are paid for by `internal/objindex` and `internal/commitgraph` at the top level — flat is fine for M2; we promote into `internal/repo/...` only if M3+ shows that's where they belong.

### 2.2 Why `internal/gitcli/` is a real package

Three reasons it isn't inline `exec.Command` calls:

1. The differential harness needs the same git wrappers as the importer/exporter; one well-tested implementation beats two parallel ones.
2. Testing the wrappers in one place gives a stable mock surface for `import`/`export` unit tests.
3. When M-later swaps an individual call from Track A to Track B (e.g., replacing `git pack-objects` with a pure-Go pack writer), the change is one file's contract, not a grep-and-fix across the codebase.

## 3. Storage layout, content addressing, formats

### 3.1 Keys M2 adds

Under `/tenants/{tid}/repos/{rid}/`:

```
packs/canonical/{pack_id}.pack
packs/canonical/{pack_id}.idx
indexes/object-map/{hash}.bvom        (BVOM = bucketvcs object map)
indexes/commit-graph/{hash}.bvcg      (BVCG = bucketvcs commit graph)
```

All four are **immutable, content-addressed, write-once** via `PutIfAbsent`. `packs/canonical/` matches §21. `indexes/object-map/` and `indexes/commit-graph/` are new key prefixes; constructors are added to `internal/repo/keys` (M1's keys package — the only place §6 paths get built).

### 3.2 `pack_id`

The Git-native pack name: SHA-1 over the sorted object IDs the pack contains (the same hash that names `pack-{name}.pack` in a normal `objects/pack/` directory). This is what `git pack-objects --all` already prints to stdout.

- **Pros:** deterministic, matches every Git tool, free to compute.
- **Cons:** not based on pack *bytes* — two re-packs of the same object set can produce the same `pack_id` even if delta encoding differs. That's fine for M2 (we never re-pack here; M9 does).

### 3.3 `{hash}` for index objects

SHA-256 of the index file bytes. Different from `pack_id` because index files have no prior naming convention to inherit, and we want a single rule for "is this byte sequence already uploaded." Hex-encoded, 64 chars.

### 3.4 Object-to-pack map format (`.bvom`)

Single binary file, fixed-width records, sorted by OID for binary-search lookup:

```
header (32 bytes):
  magic     "BVOM"          (4)
  version   uint32 BE = 1   (4)
  count     uint64 BE       (8)        # number of records
  pack_tbl  uint64 BE       (8)        # byte offset of pack-id table
  reserved                  (8)

records (count × 32 bytes), sorted ascending by oid:
  oid          [20]byte
  pack_idx     uint16 BE              # index into pack-id table
  reserved     [2]byte
  offset       uint64 BE              # byte offset within the .pack

pack-id table (at pack_tbl):
  n_packs      uint16 BE
  for each pack:
    pack_id_hex [40]byte              # ASCII hex of SHA-1

trailer (32 bytes):
  sha256       [32]byte               # SHA-256 over everything before trailer
```

Lookups are O(log n) with two range GETs in the cold path: header (read first 32 bytes), then bisect the records section using range reads. The pack-id table is small (~40 bytes × n_packs) and gets coalesced into the last range read or fetched once and cached.

For M2 there's exactly one pack per repo, so the table has one entry. The format generalizes for M9.

### 3.5 Commit-graph format (`.bvcg`) — M2-local, not Git's

Git's `commit-graph` file format is rich (chunked CHRM/OIDF/OIDL/CDAT/EDGE/BIDX/BDAT, generation numbers, Bloom filters). M2 needs only "OID → parent OIDs + the set of ref tips." Writing a pure-Go reader for Git's full format is M9/M10 work; doing it in M2 burns budget on a feature M3 doesn't need yet.

```
header (32 bytes):
  magic     "BVCG"          (4)
  version   uint32 BE = 1   (4)
  n_commits uint64 BE       (8)
  n_tips    uint32 BE       (4)
  reserved                  (12)

ref tips (n_tips × 24 bytes):
  ref_name_offset uint32 BE          # offset into string table
  oid             [20]byte

commit records, sorted ascending by oid:
  for each commit:
    oid           [20]byte
    n_parents     uint8
    parent_oids   [n_parents][20]byte

string table:
  packed UTF-8 strings, NUL-terminated

trailer (32 bytes):
  sha256
```

This is enough for M3 to: enumerate refs from the manifest body, look up each tip's commit in the graph, walk parents without re-reading the pack. M9/M10 replace this with Git's commit-graph format if benchmarks justify it — M3 will read commit-graph through `internal/commitgraph`'s API, not the file directly.

### 3.6 Generation path at import time (Track A)

`internal/importer/importer.go` orchestrates (every shell-out below goes through `internal/gitcli`):

1. `gitcli.CloneBareMirror(src, tmpdir)` — always to a fresh `tmpdir`, never modifying the caller's source repo.
2. `gitcli.Fsck(tmpdir, strict=true)` — fail import if source is broken.
3. `gitcli.PackObjectsAll(tmpdir, outPrefix)` — produce a single `.pack` + `.idx` pair; the wrapper returns the resulting `pack_id` (Git's pack name).
4. Open the produced `.pack` + `.idx` with the M2 pure-Go reader; build `.bvom` from index entries; build `.bvcg` by walking commits via the reader.
5. Collect refs via `gitcli.ShowRef(tmpdir)` and the default-branch symref via `gitcli.SymbolicRef(tmpdir, "HEAD")`; canonicalize to `Refs map[string]string` + `DefaultBranch string`.
6. Upload with `PutIfAbsent`, in this order — content first, then indexes:
   - `packs/canonical/{id}.pack`
   - `packs/canonical/{id}.idx`
   - `indexes/object-map/{hash}.bvom`
   - `indexes/commit-graph/{hash}.bvcg`
7. Call `repo.Repo.Commit` on a freshly-`Create`'d repo with a callback that sets manifest body fields: `refs`, `default_branch`, `packs`, `indexes`.

The callback is the only place the new state becomes visible. If any upload in step 6 fails after partial writes, the orphans are picked up by M8 GC; nothing in the manifest references them yet so nothing is observable as committed.

Everything content-addressed and `PutIfAbsent`-keyed: re-running a failed import is safe and idempotent up to the final CAS.

## 4. Public API

### 4.1 Pack reader (`internal/pack`)

```go
type ObjectType uint8
const (
    TypeCommit ObjectType = 1
    TypeTree   ObjectType = 2
    TypeBlob   ObjectType = 3
    TypeTag    ObjectType = 4
)

type OID [20]byte                       // SHA-1; v1 of M2 is SHA-1 only (§20)

type Object struct {
    Type ObjectType
    Size int64                          // uncompressed payload size
    Data []byte                         // fully resolved (deltas applied)
}

// Reader gives random-access, range-GET-backed reads of one .pack/.idx pair.
type Reader struct { /* ... */ }

// Open loads only .idx and a small head of .pack to validate magic+version.
// All object reads are lazy range GETs against store.
func Open(ctx context.Context, store storage.ObjectStore,
    packKey, idxKey string) (*Reader, error)

func (r *Reader) Has(oid OID) bool
func (r *Reader) Get(ctx context.Context, oid OID) (*Object, error)
func (r *Reader) ForEach(fn func(oid OID, packOffset uint64) error) error
func (r *Reader) Close() error
```

`Reader` resolves `OFS_DELTA` and `REF_DELTA` chains internally. It carries the small caches from §2 (idx fanout page + bounded delta-base LRU). M3 will hold one `Reader` per pack per repo and call `Get` on the fetch-negotiation hot path.

### 4.2 Manifest body fields M2 adds

M1's `Commit` callback receives a body the milestone may mutate. M2 defines this Go-typed view of the body fields it owns:

```go
package manifest  // (in internal/repo/manifest, extending what M1 created)

type Body struct {
    DefaultBranch string                  // §7; M1 already wrote this for Create
    Refs          map[string]string       // §19.1 inline: ref_name -> hex-OID
    Packs         []PackEntry             // §21
    Indexes       Indexes                  // M2's reachability index pointers
    // Bundles, RefShards left zero-valued in M2; reserved for M11/M12.
}

type PackEntry struct {
    PackID      string  // hex SHA-1 (§3.2)
    PackKey     string  // packs/canonical/{pack_id}.pack
    IdxKey      string  // packs/canonical/{pack_id}.idx
    SizeBytes   int64
    ObjectCount int
}

type Indexes struct {
    ObjectMap   *IndexRef  // indexes/object-map/{hash}.bvom; nil only on empty repo
    CommitGraph *IndexRef  // indexes/commit-graph/{hash}.bvcg; nil only on empty repo
}

type IndexRef struct {
    Key  string // full §6 key
    Hash string // SHA-256 hex over file bytes
}
```

The on-the-wire JSON shape mirrors this 1:1 (JSON tags use `snake_case`). M2 commits a golden-file test (§7.4) that asserts every field round-trips, so M3 can rely on the exact wire shape.

### 4.3 Object-to-pack map and commit graph

```go
package objindex

func Build(packReader *pack.Reader, packID string) ([]byte, error)
type Map struct{ /* ... */ }
func Open(ctx context.Context, store storage.ObjectStore, key string) (*Map, error)
func (m *Map) Lookup(oid pack.OID) (packID string, offset uint64, ok bool)

package commitgraph

type Tip struct{ Ref string; OID pack.OID }
func Build(packReader *pack.Reader, tips []Tip) ([]byte, error)
type Graph struct{ /* ... */ }
func Open(ctx context.Context, store storage.ObjectStore, key string) (*Graph, error)
func (g *Graph) Parents(oid pack.OID) ([]pack.OID, bool)
func (g *Graph) Tips() []Tip
```

`Build` returns the file bytes so the caller can hash them (for content-addressing) and `PutIfAbsent` them. Decoupling build-bytes from upload keeps these packages independent of `internal/storage` for write paths and lets the importer test them with `bytes.Buffer`-only.

### 4.4 Importer / exporter

```go
package importer

type Options struct {
    SourceDir     string  // path to a bare git repo on local disk (or convertible)
    Tenant, Repo  string
    Actor         string
    DefaultBranch string  // optional; if empty, taken from source HEAD; else "refs/heads/main"
}

type Result struct {
    PackID            string
    ObjectMapHash     string  // hex SHA-256
    CommitGraphHash   string
    ManifestVersion   uint64
    RefCount          int
    ObjectCount       int
}

// Import is idempotent up to the final CAS: re-running after partial failure is safe
// because every uploaded object is content-addressed and PutIfAbsent.
func Import(ctx context.Context, store storage.ObjectStore, opts Options) (*Result, error)
```

```go
package exporter

type Options struct {
    Tenant, Repo string
    DestDir      string  // must not exist or must be empty
    RunFsck      bool    // default true; set false for tests that want intermediate state
}

type Result struct {
    ManifestVersion uint64
    ObjectCount     int
    FsckOK          bool
}

func Export(ctx context.Context, store storage.ObjectStore, opts Options) (*Result, error)
```

### 4.5 Git CLI wrappers (`internal/gitcli`)

Plain functions, one per upstream git invocation. Each takes `ctx` and a working directory; each returns parsed output (or `nil` for fire-and-forget) and a wrapped error including stderr. Tests use a `gitBin` package-level var (test-only setter) for hermetic mocks; production reads `GIT_BINARY` env var falling back to `PATH` lookup with a single resolution at process start.

```go
func InitBare(ctx context.Context, dir string) error
func CloneBareMirror(ctx context.Context, src, dst string) error
func Fsck(ctx context.Context, dir string, strict bool) error
func PackObjectsAll(ctx context.Context, dir, outPrefix string) (packID string, err error)
func IndexPack(ctx context.Context, dir, packPath string) error
func UnpackObjects(ctx context.Context, dir, packPath string) error
func UpdateRef(ctx context.Context, dir, ref, oid string) error
func SymbolicRef(ctx context.Context, dir, name string) (target string, err error)
func ShowRef(ctx context.Context, dir string) (map[string]string, error)
func RevListAllObjects(ctx context.Context, dir string) ([]string, error)
func CatFilePretty(ctx context.Context, dir, oid string) ([]byte, error)
func Version(ctx context.Context) (string, error)
```

We do not wrap `git commit-graph write` — M2's commit graph is our own format; the pack-reader walks commits to build it.

### 4.6 `bucketvcs cat-object`

```
bucketvcs cat-object --store=<url> [--type|--size|--pretty] <tenant> <repo> <oid>
```

Mirrors `git cat-file -t/-s/-p` semantics. It exists primarily as the differential-harness oracle target; secondary use is operator debugging. Lives next to `inspect-manifest` in `cmd/bucketvcs/`.

## 5. Differential harness

§40.3 says the harness is a core deliverable, scaffolded at M2 and CI-gating from M3. M2's job is to land the bones, the test-time discipline, and enough coverage that M3 can plug in protocol-level oracles without restructuring.

### 5.1 What's in the harness at M2 ship

Two oracle assertions, both running on every fixture:

**Round-trip oracle.** Given a fixture builder that produces a bare git repo at `srcDir`:
1. `Import(srcDir → bucketvcs)`.
2. `Export(bucketvcs → dstDir)`.
3. `git fsck --strict` clean on both `srcDir` and `dstDir`.
4. `show-ref` on both produces identical `(ref, oid)` sets.
5. The set of object IDs reachable from each ref is identical (computed via `git rev-list --objects --all` on both sides).
6. For every reachable OID, `git cat-file -p` returns identical bytes on both sides.

**Pack-reader oracle.** For the same fixture, after import:
1. For every reachable OID, `bucketvcs cat-object --pretty` returns bytes identical to `git cat-file -p` on the source.
2. For every reachable OID, `bucketvcs cat-object --type` and `--size` match `git cat-file -t / -s`.

The pack-reader oracle is the one that catches pure-Go pack-reader bugs at the layer M3 actually consumes. The round-trip oracle would let some classes of reader bugs hide (e.g., a delta-resolution bug that round-trips because the same `git` binary writes and reads).

### 5.2 Fixture corpus (synthetic, in-test)

Under `internal/diffharness/fixtures/`, each fixture is a function that uses `internal/gitcli` to script git commands against a fresh `dir`:

```go
type Fixture struct {
    Name     string
    Refs     map[string]string  // ref name -> hex OID, from gitcli.ShowRef
    AllOIDs  []string           // closure of reachable objects, from gitcli.RevListAllObjects
}

type Builder func(t *testing.T, dir string) Fixture
```

`Refs` and `AllOIDs` are populated by the builder *after* the synthetic git history is laid down, by calling `internal/gitcli` against `dir`. Cached on the struct so per-fixture assertions don't re-run `git rev-list` on every check.

```go
var Registry = map[string]Builder{
    "empty":            buildEmpty,
    "single_commit":    buildSingleCommit,
    "linear_3_commits": buildLinear3,
    "branch_and_merge": buildBranchAndMerge,
    "lightweight_tag":  buildLightweightTag,
    "annotated_tag":    buildAnnotatedTag,
    "symref_head":      buildSymrefHead,            // HEAD -> refs/heads/dev
    "two_branches":     buildTwoBranchesDivergent,
    "binary_blob":      buildBlobWithBinaryContent, // one ~1 MiB random blob
    "deep_tree":        buildDeepNestedTrees,       // exercises tree delta chains
}
```

Tests iterate the registry. Adding a fixture is one PR touching one file. M3 will add its own protocol-level fixtures (`fetch_clone_closure`, `push_acceptance`, etc.) as a parallel registry under the M3 package — they don't have to cohabit M2's.

Real-world corpora and opt-in network corpora are explicitly out of scope for M2; they land naturally as M3 push/fetch tests find tail-case bugs that synthetic corpora miss.

### 5.3 Hermeticity and CI

- `internal/gitcli.Version()` is called once at test setup; tests `t.Skip` if `git` is not on `PATH`. CI must have `git` ≥ 2.40 (the version where protocol v2 is stable enough to depend on for M3, and where commit-graph generation is well-behaved). The minimum is checked in `gitcli_test.go` and the version surfaces in test logs.
- Every fixture builder uses `t.TempDir()` for both source and dest. No network, no shared state.
- The harness uses `localfs` as the storage backend. Cloud-backend differential coverage is M5/M7's problem; M2's harness must not bake in localfs assumptions — `Import`/`Export` take `storage.ObjectStore`, and the harness gets one from a small `newTestStore(t)` helper that any future backend can swap.
- Each round-trip test runs in a few hundred milliseconds on a dev box. The full M2 differential suite should complete in under 30 seconds without `-tags stress`. If it grows past that, partition by build tag, not by skipping coverage.

### 5.4 Promotion-rule housekeeping

§40.3's promotion rule (100% pass + 4-week shadow before pure-Go path becomes default serving) doesn't activate until M3, but M2 establishes the artifact it promotes against:

- `docs/superpowers/diffharness/known-divergences.md` — empty file committed at M2 ship, with header explaining what goes here and what doesn't ("not a dumping ground for correctness bugs"). Format: one entry per divergence, classified per §40.3 (`bucketvcs bug` / `git quirk to emulate` / `intentional documented difference` / `unsupported optional capability` / `invalid test case`), date opened, link to issue.
- `internal/diffharness/divergences_test.go` parses that file and fails CI if any entry is missing classification, date, or link. Cheap, catches the dumping-ground failure mode early.

## 6. Failure modes, recovery, and orphan budget

M2 inherits M1's atomic-pair primitive but adds its own write fan-out (4 objects per import: pack, idx, object-map, commit-graph) before the final CAS. That widens the orphan surface. This section pins down what each failure looks like and what does (and doesn't) clean up.

### 6.1 Importer failure points

```
Import(...)
  step 1   gitcli.CloneBareMirror   (local fs)               -> rerunnable
  step 2   gitcli.Fsck --strict     (local fs)               -> rerunnable
  step 3   gitcli.PackObjectsAll    (local fs)               -> rerunnable
  step 4   build .bvom              (local fs)               -> rerunnable
  step 5   build .bvcg              (local fs)               -> rerunnable
  step 6a  PutIfAbsent packs/canonical/{pack_id}.pack
  step 6b  PutIfAbsent packs/canonical/{pack_id}.idx
  step 6c  PutIfAbsent indexes/object-map/{hash}.bvom
  step 6d  PutIfAbsent indexes/commit-graph/{hash}.bvcg
  step 7   repo.Repo.Commit(callback sets refs/packs/indexes/default_branch)
                                                            -> M1 atomic pair
```

Steps 1–5 are local; failures there abort cleanly with a nonzero exit code and no remote state. Steps 6a–6d are content-addressed `PutIfAbsent`, so a partial run leaves orphans whose keys we can re-derive deterministically from the same source repo on the next attempt. Step 7 is M1's atomic pair (tx record then root CAS); its failure modes are M1's.

**Crash between 6a and 6d.** Some uploaded blobs in `packs/canonical/` and/or `indexes/`, but no manifest reference. Re-running `bucketvcs import` against the same source repo recomputes identical keys and `PutIfAbsent` no-ops on the already-uploaded ones. Manual cleanup is unnecessary; M8 GC will sweep anything that's truly abandoned.

**Crash between 6 and 7.** All four blobs uploaded, no manifest. Same recovery: re-run import; the second attempt's step 6 is all no-ops; step 7 commits.

**Crash during step 7.** That's M1's failure surface and M1 already handles it: tx record may exist without root advance (orphan tx, M8 sweeps), or both written but the in-process error propagates — caller retries `Commit`, M1's CAS-loss path triggers, callback re-runs against the new view. M2's callback is idempotent (sets fields to deterministic values), so this is safe.

### 6.2 Importer is reject-on-conflict, not idempotent-on-content

If `Create` returns `ErrRepoExists`, `Import` exits with a typed error and exit code 2. We do **not** auto-merge into an existing repo. M2's contract is "create a fresh repo from a source bare repo"; merging two import sources is a different feature with different correctness properties (ref conflicts, ancestor resolution) and lives nowhere in the OSS roadmap before M3+. The CLI message says: "repo already exists; delete it or import to a different `<tenant>/<repo>`."

Re-running `bucketvcs import` against the same source after a partial-failure crash works because the `Repo` row is also conditional: M1's `Create` itself uses `PutIfAbsent` on `root.json`. If the prior attempt got far enough to commit, the next attempt fails fast with `ErrRepoExists` and the operator knows the import is already done.

### 6.3 Exporter failure points

Export is read-only against the bucket and write-only against `DestDir`. Failure modes:

- `repo.Open` returns `ErrRepoNotFound` or `ErrUnsupportedSchema`: surface verbatim, exit 2.
- A pack/idx/index object referenced by the manifest is missing from the bucket: hard error, exit 3, message names the missing key. (M16 is where repair tooling for this lives; M2 just diagnoses.)
- `git fsck` fails on the exported repo: exit 4, fsck stderr included. Strong signal of a bucket-side bug or import-side bug; the exit-code differentiation lets the differential harness treat it distinctly from "missing object."
- `DestDir` exists and is non-empty: exit 2 with a message; we don't overwrite. `--force` is **not** added in M2; if operators want overwrite, they `rm -rf` first. This is the "destructive default" boundary worth keeping firm.

### 6.4 Pack-reader failure modes

- Idx + pack disagree on object count, fanout, or trailer: `pack.Open` returns `ErrPackCorrupt`. Wraps the underlying mismatch in a stable error type so the harness can assert on classification.
- Object inflate fails or delta chain exceeds a configurable depth (default 50, semantically near Git's `core.deltaBaseCacheLimit`): typed `ErrDeltaChainTooDeep`. M9 will tune this; M2 just makes it observable and bounded.
- Range GET returns short read or 4xx: wrap in `ErrStorage`; caller (M3 in the future) decides retry policy. M2 itself doesn't retry — that's a layer above the reader.

### 6.5 What M2 explicitly does not clean up

- Failed-import orphans in `packs/canonical/` or `indexes/`: M8.
- Old packs/indexes after a future repack supersedes them: M9.
- Old manifest versions / orphan tx records: M8.
- Multipart-upload garbage: M0/M8 (the storage adapter handles its own partial-multipart recovery; M1's atomic pair never observes a partial).

The orphan budget for an M2-only deployment is bounded: at most 4 objects per failed import attempt, no compounding from normal operation. Documenting this so the M8 spec can size its sweep accordingly.

## 7. Testing strategy

The differential harness from §5 is one of three test layers.

### 7.1 Unit tests (per package)

- **`internal/pack`** — `reader_test.go` builds packs via `internal/gitcli.PackObjectsAll` against scripted repos, then asserts byte-identical `Get(oid)` results vs `git cat-file -p`. Edge cases: empty pack, single-object pack, REF_DELTA chain depth 1/5/50, OFS_DELTA backward reference, `OBJ_OFS_DELTA` to a base in the same pack at large negative offset, idx v2 with > 256 first-byte fanout entries (forces large-offset table — needs a fixture with > 4 GiB pack offsets simulated by a synthetic idx).
- **`internal/objindex`** — `format_test.go` round-trips Build → bytes → Open → Lookup for fixtures of size 0, 1, 2, 256 (fanout boundary), and ~10k entries; asserts trailer SHA-256 matches; asserts binary search on a random sample. Property test: shuffle input then build twice → bytes are byte-identical (sort stability).
- **`internal/commitgraph`** — same shape: Build → bytes → Open → Parents/Tips. Edge cases: orphan commit (no parents), merge commit (2 parents), octopus merge (≥ 3 parents), annotated tag pointing into the graph, multiple tips at the same OID.
- **`internal/gitcli`** — wrapper-by-wrapper unit test, all `t.Skip` if `git` not on PATH. Verifies the parsed return shape, the stderr-wrapping on failure, and that the `gitBin` test override works.
- **`internal/importer` / `internal/exporter`** — table-driven tests per fixture from §5.2's registry, asserting the `Result` shape and that the resulting manifest body has the expected `Refs`/`Packs`/`Indexes` shape (M2 wire-format contract for M3).

### 7.2 Differential harness (§5)

Runs `Registry`-driven round-trip + pack-reader oracles. Two top-level test files:

```
internal/diffharness/roundtrip_test.go        — runs every fixture through round-trip oracle
internal/diffharness/catobject_test.go        — runs every fixture through cat-object oracle
```

Both `t.Skip` if `git` is missing. Both run on every CI build, no build tags.

### 7.3 CLI tests

`cmd/bucketvcs/import_test.go`, `export_test.go`, `catobject_test.go` — black-box tests that invoke `run(...)` (the existing M1 test entry point) and assert: exit codes, stdout/stderr text, and bucket state via `repo.Open` afterward. Mirrors the pattern M1 established in `init_test.go` / `inspect_test.go`.

### 7.4 Manifest wire-format contract test

`internal/repo/manifest/m2body_test.go` — a **golden file** test:

```
testdata/golden/m2-body-minimal.json     # an empty repo (no packs, no indexes)
testdata/golden/m2-body-single-pack.json # one canonical pack, one .bvom, one .bvcg, two refs
```

Marshals a hand-built `Body` struct → bytes → asserts byte-identical to the golden file; then unmarshals → asserts struct equality. Updating the goldens requires `go test -update-golden` (a flag we add). Why this matters: M3 will read these byte sequences from buckets created by M2. Drift here is an on-the-wire break.

### 7.5 Concurrency / stress

M2 import is a one-shot operation, not a hot path. We don't need M1's `+build stress` 16×200 commit suite for M2 itself.

We do need: a `+build stress` test that imports a 1000-commit synthetic repo, validates timing stays roughly linear, and asserts that `len(.bvom) + len(.bvcg) < 128 MiB` for that fixture. The 128 MiB cap is loose on purpose — it's a smoke-test sanity bound, not a tuned budget. §14.2's 64 MiB delta-index ceiling is M10's, not M2's; this test exists to catch a 10x format regression, not to enforce production sizing. One test, runs in a couple of minutes, gated. Not a ship gate.

### 7.6 What's intentionally not tested at M2

- Concurrent imports against the same `<tenant>/<repo>`: M1's `Create` is `PutIfAbsent`, so the second wins or loses cleanly; we test that one race in `cmd/bucketvcs/import_test.go` and call it covered. We don't run the full M1 concurrency property suite over import paths.
- Cloud-backend differential coverage: M5/M7. M2's harness uses localfs; the harness is parameterized over `storage.ObjectStore` so M5/M7 can plug in.
- Performance benchmarks past the smoke test in §7.5: M9 owns repack benchmarks; M10 owns reachability benchmarks. M2 imports are not user-facing latency.

### 7.7 Coverage gates for M2 ship

- `go test ./...` clean.
- `go test -race ./...` clean.
- `go vet ./...` clean.
- `staticcheck ./...` clean (M1 ship gate; carry forward).
- All synthetic fixtures pass round-trip + cat-object oracle.
- `divergences_test.go` (§5.4) clean — no unaccompanied entries.
- M1's localfs concurrency property test still passes (regression sanity).

## 8. CLI surface

```
bucketvcs import --store=<url> [--default-branch=<ref>] [--actor=<id>] <src-bare-repo> <tenant> <repo>
bucketvcs export --store=<url> [--no-fsck] <tenant> <repo> <dst-dir>
bucketvcs cat-object --store=<url> [--type|--size|--pretty] <tenant> <repo> <oid>
```

All three follow the M1 conventions established in `cmd/bucketvcs/`:
- `--store` is required and uses the existing `parseStoreURL` (M1's `cmd/bucketvcs/store.go`); only `localfs:` is accepted in M2.
- Positional args follow `<tenant> <repo>` order (matches `init` and `inspect-manifest`).
- Exit codes: `0` success, `2` usage / not-found / already-exists, `3` missing referenced bucket object, `4` fsck failure on export, `1` for unclassified internal errors. The classification matters because the differential harness `Skip`s on certain codes and `Fail`s on others.

`bucketvcs import` writes one line per major step to stderr (`fsck source ok`, `pack built {pack_id} {n} objects`, `uploaded pack`, `uploaded indexes`, `commit {manifest_version}`) so an operator running it on a slow bucket sees progress without needing a verbose flag. The lines are stable text — not a structured logger — to keep them grep-able and to avoid pulling in the observability skeleton (§32) at M2; that lands later.

`bucketvcs export` writes to a single created directory and runs `git fsck` at the end unless `--no-fsck` is set. `--no-fsck` exists for the harness (test wants to inspect intermediate state) and for support workflows where the bucket is suspected broken; not the default.

### 8.1 Updated CLI roster after M2

| Command | Status |
|---|---|
| `bucketvcs init` | M1 ✓ |
| `bucketvcs inspect-manifest` | M1 ✓ |
| `bucketvcs import` | **M2** |
| `bucketvcs export` | **M2** |
| `bucketvcs cat-object` | **M2** (debug; not in §35 but used by harness) |
| `bucketvcs serve` | M3 |
| `bucketvcs doctor` | M16 |
| `bucketvcs conformance-test` | exists implicitly via M0 conformance package; no CLI yet |
| `bucketvcs gc` | M8 |

## 9. Out-of-scope deferrals (explicit)

| Deferred to | Item | Why not M2 |
|---|---|---|
| **M3** | Git protocol gateway, pkt-line, capability negotiation, fetch/clone/push HTTP handlers | M2's job ends at "data round-trips cleanly"; serving Git is its own milestone |
| **M3** | In-process per-repo push serialization | No push path in M2 |
| **M3** | Promotion-rule enforcement (100% diff pass + 4-week shadow before pure-Go default) | The pure-Go path doesn't *serve* anything in M2; nothing to promote |
| **M5/M7** | Cloud-backend coverage of `Import`/`Export` | `--store` only accepts `localfs:` in M2; cloud schemes return the same `reserved; cloud adapters land at M5/M7` error M1 wired up |
| **M8** | Garbage collection of orphan packs/indexes/tx | M2 generates orphans on failed imports (§6); cleanup is M8's contract |
| **M9** | Background repack, multi-pack consolidation, pack-count bounds (§15.3 thresholds) | M2 ships one canonical pack per repo by construction; M9 is when "many packs" becomes a real shape |
| **M9** | Bitmaps, `multi-pack-index`, generated-vs-canonical pack distinction | None needed for M2's read path; building them now would be wasted work that M9 may redesign |
| **M9** | Adopting Git's commit-graph file format | M2's `.bvcg` is sufficient for M3; format swap is M9 if benchmarks justify |
| **M10** | Base + delta reachability indexes, partitioning by generation/pack/namespace, mini-bitmaps | M2 ships only the "base" — no deltas, no partitions |
| **M10** | Compaction CAS protocol for reachability | No compaction targets exist in M2 |
| **M11** | Bundle URI, packfile URI, bundle freshness states | Pure acceleration; meaningless before M3 serves traffic |
| **M12** | Sharded refs (§19.2), ref resharding maintenance | M2 stays inline (§19.1); sharding lands when measured ref-count justifies |
| **M13** | LFS | Independent surface |
| **M14/M15** | Hooks, policy, webhooks, audit | Plug into M3's receive flow, additive |
| **M16** | `bucketvcs doctor`, manifest reconstruction from tx chain, orphan listing | Repair tooling consumes M2 outputs but isn't M2's contract |
| **future** | SHA-256 repository support (§20) | Spec says "tracked but MUST NOT block initial product viability"; M2 is SHA-1 only and the `OID` type is a fixed `[20]byte` rather than a polymorphic interface |

Two cross-cutting items M2 *does* extend (called out so they're not mistaken for deferrals):

- **Storage conformance suite (M0).** Unchanged in M2 — no new conformance requirements, but the M2 harness uses the same `storage.ObjectStore` factory pattern so future cloud adapters (M5/M7) automatically pick up M2 differential coverage when they pass the suite.
- **Differential harness (cross-cutting from M2 onward).** Expands at every milestone per §40.3.

## 10. Review protocol

Per `m1_review_protocol.md` (carried forward): each task ends with superpowers `code-reviewer` for spec/code-quality, then roborev-refine on max reasoning until pass or diminishing returns. The protocol caught ~25 substantive findings in M1; recommend continuing for M2.
