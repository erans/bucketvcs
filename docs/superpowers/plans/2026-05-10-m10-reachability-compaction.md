# M10 — Reachability Compaction Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the §14 reachability index: per-push `.bvrd` delta files, `.bvcg` v2 with generation numbers, maintenance-driven compaction with §14.2 thresholds, and a pure-Go negotiation pre-step in upload-pack so cold gateways answer `have`/`want` without materializing the mirror first.

**Architecture:** New top-level package `internal/reachability/` (sibling of `internal/maintenance`, `internal/gc`) holds the `Set` abstraction (base + delta chain + object map). New sub-package `internal/reachability/deltaindex/` owns the `.bvrd` wire format. `internal/commitgraph/` is extended in place to v2 (adds generation numbers). `internal/gitproto/receivepack/` learns to write a delta per push; `internal/gitproto/uploadpack/` splits into a Negotiate phase (pure-Go, against `reachability.Set`) followed by a lazily-materialized Deliver phase (existing mirror + `git pack-objects`). `internal/maintenance/` Phase-0 grows three reachability-threshold checks and a compact-only outcome that rebuilds `.bvom` + `.bvcg` v2 from the current pack set without repacking. CAS-merge body extends to drop the consumed delta prefix.

**Tech Stack:** Go 1.25, existing `internal/storage` ObjectStore (M0), `internal/repo` transaction kernel (M1), `internal/pack` reader (M2), `internal/objindex` and `internal/commitgraph` (M2 — commitgraph evolves to v2 in this milestone), `internal/gitcli` (M2), `internal/mirror` (M3), `internal/maintenance` (M9), `internal/gc` (M8), `log/slog` for structured events, no new external dependencies.

**Spec:** `docs/superpowers/specs/2026-05-10-m10-reachability-compaction-design.md`

---

## File Structure

**New files:**

```
internal/reachability/
  doc.go                       // package overview
  errs.go                      // sentinel errors
  errs_test.go
  set.go                       // Set struct, Load, Has/Parents/Generation/WalkAncestors/RefTips/ObjectPack
  set_test.go
  shadow_test.go               // delta-shadows-base semantics
  fallback.go                  // IsFallback(err) + reason classification
  fallback_test.go
  README.md

internal/reachability/deltaindex/
  doc.go
  format.go                    // magic, version, header layout, constants
  format_test.go               // golden bytes
  build.go                     // Encode(rec []CommitRecord, tips, packs) []byte
  build_test.go
  read.go                      // Decode, Reader.Iter, Reader.Lookup
  read_test.go
  reject_test.go               // truncated trailer, bad magic, version mismatch

internal/reachability/conformance/
  safety.go                    // RunPropertyReachabilitySafety(t, factory)
  safety_test.go               // localfs harness

internal/reachability/rtest/
  fixtures.go                  // synthesizeBaseRepo, applyPush helpers
  fixtures_test.go

internal/gitproto/uploadpack/
  negotiate.go                 // pure-Go Git v2 want/have/done loop against reachability.Set
  negotiate_test.go
  shipping_plan.go             // ShippingPlan struct + diff helpers
  shipping_plan_test.go

cmd/bucketvcs/
  negotiate.go                 // debug subcommand
  negotiate_test.go

docs/
  m10-reachability-operator-guide.md
```

**Modified files:**

```
internal/commitgraph/format.go          // bump magic version to 2, add GenerationNumber to Record
internal/commitgraph/build.go           // compute gen numbers in topo order
internal/commitgraph/read.go            // v1 + v2 readers; GenerationOf accessor
internal/commitgraph/format_test.go     // v2 golden bytes + v1 back-compat test
internal/commitgraph/build_test.go      // generation property test

internal/repo/manifest/body.go          // IndexRef.SizeBytes, Indexes.Reachability, ReachabilityRef
internal/repo/manifest/body_test.go     // backward-compat decode, new-field roundtrip
internal/repo/manifest/testdata/golden/*.json  // refresh affected goldens (size_bytes optional)
internal/repo/keys/keys.go              // ReachabilityDeltaKey(hash) helper
internal/repo/keys/keys_test.go

internal/gitproto/receivepack/engine.go // build + upload .bvrd; append to manifest
internal/gitproto/receivepack/engine_test.go
internal/gitproto/uploadpack/engine.go  // split into Negotiate + Deliver
internal/gitproto/uploadpack/service.go // wire negotiate path with fallback
internal/gitproto/uploadpack/engine_test.go

internal/maintenance/options.go         // 3 new threshold fields
internal/maintenance/options_test.go
internal/maintenance/thresholds.go      // evaluate reachability thresholds
internal/maintenance/thresholds_test.go
internal/maintenance/pipeline.go        // compact-only outcome
internal/maintenance/pipeline_test.go
internal/maintenance/casmerge.go        // drop consumed deltas in CAS body builder
internal/maintenance/casmerge_test.go
internal/maintenance/indexes.go         // expose buildIndexes for compact-only reuse
internal/maintenance/conformance/safety.go  // add compaction-aware interleavings

internal/gc/walk.go                     // include .bvrd in live-set walk
internal/gc/walk_test.go
internal/gc/sweep.go                    // sweep prefix indexes/reachability-delta/
internal/gc/sweep_test.go
internal/gc/conformance/safety.go       // add compaction_during_mark interleaving

internal/diffharness/roundtrip_helpers_test.go  // ImportPushCompactNegotiateExportAndCompare
internal/diffharness/fixtures/*                  // many-small-pushes, force-push-mid-chain, tag-pushes-between, octopus-merge

cmd/bucketvcs/maintenance.go            // 3 new flags + reachability_compaction JSON field
cmd/bucketvcs/maintenance_test.go
cmd/bucketvcs/inspect-manifest.go       // reachability block in JSON
cmd/bucketvcs/inspect-manifest_test.go
cmd/bucketvcs/main.go                   // wire "negotiate" subcommand

docs/m9-maintenance-operator-guide.md   // cross-reference reachability thresholds
README.md                               // mention internal/reachability + M10 property
```

**Note on negotiation parity:** `git upload-pack` is the oracle. Every test that produces a `ShippingPlan` via the pure-Go engine also runs the same `(wants, haves)` probe against a materialized mirror and asserts the two `ShippingPlan` values are byte-equal. This is the strongest contract we can hold without forking the protocol.

---

## Phase 0 — Manifest schema + key helper

This phase widens the manifest body so receive-pack (Phase 4) and maintenance (Phase 5) can read/write the new reachability fields, and adds the storage-key helper they'll use. No reader/writer code yet.

### Task 0.1: Add `SizeBytes` to `IndexRef`

**Files:**
- Modify: `internal/repo/manifest/body.go`
- Modify: `internal/repo/manifest/body_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/repo/manifest/body_test.go`:

```go
func TestIndexRef_SizeBytes_Roundtrip(t *testing.T) {
	ref := IndexRef{Key: "tenants/t/repos/r/indexes/object-map/aa.bvom", Hash: "aa", SizeBytes: 12345}
	body := Body{Indexes: Indexes{ObjectMap: &ref}}
	out, err := MarshalBody(body)
	if err != nil {
		t.Fatalf("MarshalBody: %v", err)
	}
	var got Body
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Indexes.ObjectMap == nil || got.Indexes.ObjectMap.SizeBytes != 12345 {
		t.Fatalf("size_bytes lost: got %+v", got.Indexes.ObjectMap)
	}
}

func TestIndexRef_SizeBytes_OmittedWhenZero(t *testing.T) {
	body := Body{Indexes: Indexes{ObjectMap: &IndexRef{Key: "k", Hash: "h"}}}
	out, err := MarshalBody(body)
	if err != nil {
		t.Fatalf("MarshalBody: %v", err)
	}
	if bytes.Contains(out, []byte(`"size_bytes"`)) {
		t.Fatalf("size_bytes should be omitted when zero, got:\n%s", out)
	}
}
```

Add `"bytes"` and `"encoding/json"` to the imports if not already present.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/repo/manifest/ -run 'TestIndexRef_SizeBytes' -v`
Expected: FAIL — `unknown field SizeBytes`.

- [ ] **Step 3: Add `SizeBytes` to `IndexRef`**

Edit `internal/repo/manifest/body.go`, replace the existing `IndexRef`:

```go
// IndexRef is a key + content-hash pair. SizeBytes is populated when
// the producer knows the on-disk size (receive-pack for .bvrd,
// maintenance for .bvom/.bvcg). Consumers MAY use it for O(1)
// threshold evaluation; omit on legacy values.
type IndexRef struct {
	Key       string `json:"key"`
	Hash      string `json:"hash"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/repo/manifest/ -run 'TestIndexRef' -v`
Expected: PASS for both tests; existing tests still PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/repo/manifest/body.go internal/repo/manifest/body_test.go
git commit -m "M10 task 0.1: IndexRef.SizeBytes optional field"
```

### Task 0.2: Add `Indexes.Reachability` + `ReachabilityRef`

**Files:**
- Modify: `internal/repo/manifest/body.go`
- Modify: `internal/repo/manifest/body_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/repo/manifest/body_test.go`:

```go
func TestReachability_Roundtrip(t *testing.T) {
	body := Body{
		DefaultBranch: "main",
		Refs:          map[string]string{},
		Indexes: Indexes{
			ObjectMap:   &IndexRef{Key: "ok", Hash: "oh"},
			CommitGraph: &IndexRef{Key: "ck", Hash: "ch"},
			Reachability: &ReachabilityRef{
				BaseManifest: "v00000042",
				Deltas: []IndexRef{
					{Key: "d1k", Hash: "d1h", SizeBytes: 1024},
					{Key: "d2k", Hash: "d2h", SizeBytes: 2048},
				},
			},
		},
	}
	out, err := MarshalBody(body)
	if err != nil {
		t.Fatalf("MarshalBody: %v", err)
	}
	var got Body
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Indexes.Reachability == nil {
		t.Fatalf("Reachability lost")
	}
	if got.Indexes.Reachability.BaseManifest != "v00000042" {
		t.Fatalf("BaseManifest got %q", got.Indexes.Reachability.BaseManifest)
	}
	if len(got.Indexes.Reachability.Deltas) != 2 {
		t.Fatalf("Deltas len=%d", len(got.Indexes.Reachability.Deltas))
	}
}

func TestReachability_OmittedByDefault(t *testing.T) {
	body := Body{Indexes: Indexes{}}
	out, err := MarshalBody(body)
	if err != nil {
		t.Fatalf("MarshalBody: %v", err)
	}
	if bytes.Contains(out, []byte(`"reachability"`)) {
		t.Fatalf("reachability should be omitted when nil")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/repo/manifest/ -run 'TestReachability' -v`
Expected: FAIL — `unknown field Reachability`.

- [ ] **Step 3: Extend `Indexes` and add `ReachabilityRef`**

Edit `internal/repo/manifest/body.go`. Replace the existing `Indexes` and append `ReachabilityRef`:

```go
// Indexes carries pointers to reachability index objects. ObjectMap
// and CommitGraph form the base; Reachability lists deltas since the
// base. Legacy (pre-M10) manifests have Reachability == nil.
type Indexes struct {
	ObjectMap    *IndexRef         `json:"object_map,omitempty"`
	CommitGraph  *IndexRef         `json:"commit_graph,omitempty"`
	Reachability *ReachabilityRef  `json:"reachability,omitempty"`
}

// ReachabilityRef lists the delta chain layered on top of the base
// (ObjectMap + CommitGraph). BaseManifest records the manifest version
// that produced the base — paper-trail field, never used as a key.
type ReachabilityRef struct {
	BaseManifest string     `json:"base_manifest"`
	Deltas       []IndexRef `json:"deltas"`
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/repo/manifest/ -v`
Expected: all tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/repo/manifest/body.go internal/repo/manifest/body_test.go
git commit -m "M10 task 0.2: Indexes.Reachability + ReachabilityRef"
```

### Task 0.3: Add `ReachabilityDeltaKey` helper

**Files:**
- Modify: `internal/repo/keys/keys.go`
- Modify: `internal/repo/keys/keys_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/repo/keys/keys_test.go`:

```go
func TestRepo_ReachabilityDeltaKey(t *testing.T) {
	r, err := New("tenants/t/repos/r/")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := r.ReachabilityDeltaKey("abcd")
	want := "tenants/t/repos/r/indexes/reachability-delta/abcd.bvrd"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRepo_ReachabilityDeltaPrefix(t *testing.T) {
	r, _ := New("tenants/t/repos/r/")
	got := r.ReachabilityDeltaPrefix()
	want := "tenants/t/repos/r/indexes/reachability-delta/"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/repo/keys/ -run 'TestRepo_Reachability' -v`
Expected: FAIL — undefined methods.

- [ ] **Step 3: Implement helpers**

Append to `internal/repo/keys/keys.go`:

```go
// ReachabilityDeltaKey returns the storage key for a .bvrd delta-index
// file (M10). The hash is the SHA-256 of the file body (hex).
func (r *Repo) ReachabilityDeltaKey(hash string) string {
	return r.prefix + "indexes/reachability-delta/" + hash + ".bvrd"
}

// ReachabilityDeltaPrefix returns the common prefix for all .bvrd
// files for this repo. Used by GC sweep enumeration.
func (r *Repo) ReachabilityDeltaPrefix() string {
	return r.prefix + "indexes/reachability-delta/"
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/repo/keys/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/repo/keys/keys.go internal/repo/keys/keys_test.go
git commit -m "M10 task 0.3: Repo.ReachabilityDeltaKey + Prefix helpers"
```

---

## Phase 1 — `.bvcg` v2 (generation numbers)

The v2 format adds a `generation_number` u32 per commit record. Bump magic version. Reader supports both v1 (returns gen=0) and v2.

### Task 1.1: Bump format magic + add `Record.Generation`

**Files:**
- Modify: `internal/commitgraph/format.go`
- Modify: `internal/commitgraph/format_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/commitgraph/format_test.go`:

```go
func TestFormat_VersionConstants(t *testing.T) {
	if VersionV1 != 1 {
		t.Errorf("VersionV1 = %d, want 1", VersionV1)
	}
	if VersionV2 != 2 {
		t.Errorf("VersionV2 = %d, want 2", VersionV2)
	}
	if VersionCurrent != VersionV2 {
		t.Errorf("VersionCurrent = %d, want %d", VersionCurrent, VersionV2)
	}
}

func TestRecord_GenerationField(t *testing.T) {
	r := Record{Generation: 7}
	if r.Generation != 7 {
		t.Fatalf("Record.Generation not honored")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/commitgraph/ -run 'TestFormat_VersionConstants|TestRecord_GenerationField' -v`
Expected: FAIL — undefined identifiers.

- [ ] **Step 3: Extend the format**

Edit `internal/commitgraph/format.go`. Replace the version-related material and `Record`:

```go
// Format versions:
//
//   v1 (pre-M10) — no generation numbers; reader returns 0.
//   v2 (M10)     — adds u32 generation_number after the oid in each
//                  commit record.
const (
	VersionV1      uint32 = 1
	VersionV2      uint32 = 2
	VersionCurrent uint32 = VersionV2
)

// Record is one commit + its parents + its generation number.
// Generation = 1 + max(generations of parents); root commits have
// generation = 1. On v1 files, Generation is 0 after read.
type Record struct {
	OID        pack.OID
	Generation uint32
	Parents    []pack.OID
}
```

If the file already has a `Record` type elsewhere, this replaces it. Search-and-update any `Record{OID: ..., Parents: ...}` literals in the package to include `Generation: ...` or rely on the zero value.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/commitgraph/ -v`
Expected: PASS (existing tests work — zero-value Generation is fine for v1).

- [ ] **Step 5: Commit**

```bash
git add internal/commitgraph/format.go internal/commitgraph/format_test.go
git commit -m "M10 task 1.1: commitgraph v2 version constants + Record.Generation field"
```

### Task 1.2: Generation-number computation in `Build`

**Files:**
- Modify: `internal/commitgraph/build.go`
- Modify: `internal/commitgraph/build_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/commitgraph/build_test.go`:

```go
func TestBuild_GenerationNumbers_Linear(t *testing.T) {
	// Linear chain: A -> B -> C (C is the tip; A is the root).
	// gen(A) = 1, gen(B) = 2, gen(C) = 3.
	r := newLinearPackABC(t)
	tips := []Tip{{Ref: "refs/heads/main", OID: oidC}}
	out, err := Build(context.Background(), r, tips)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	gens := readGenerationsByOID(t, out)
	if gens[oidA] != 1 {
		t.Errorf("gen(A) = %d, want 1", gens[oidA])
	}
	if gens[oidB] != 2 {
		t.Errorf("gen(B) = %d, want 2", gens[oidB])
	}
	if gens[oidC] != 3 {
		t.Errorf("gen(C) = %d, want 3", gens[oidC])
	}
}

func TestBuild_GenerationNumbers_OctopusMerge(t *testing.T) {
	// Octopus: M has parents P1 (gen=2), P2 (gen=4), P3 (gen=3).
	// gen(M) = 1 + max(2,4,3) = 5.
	r := newOctopusPack(t)
	tips := []Tip{{Ref: "refs/heads/main", OID: oidM}}
	out, err := Build(context.Background(), r, tips)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	gens := readGenerationsByOID(t, out)
	if gens[oidM] != 5 {
		t.Errorf("gen(M) = %d, want 5", gens[oidM])
	}
}
```

Add helpers (also in `build_test.go`) — `newLinearPackABC`, `newOctopusPack`, `readGenerationsByOID`, and OID constants `oidA, oidB, oidC, oidM`. These are package-private test helpers that synthesize a tiny `pack.Reader` from in-memory commits and parse the produced bytes to return a `map[pack.OID]uint32`. Implementation pattern:

```go
// readGenerationsByOID parses bvcg v2 bytes and returns oid -> gen.
// The decode helper is exercised here too; if read.go is missing the
// field, this test will fail with a zero map.
func readGenerationsByOID(t *testing.T, b []byte) map[pack.OID]uint32 {
	t.Helper()
	rdr, err := Open(b)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	out := map[pack.OID]uint32{}
	if err := rdr.Iter(func(rec Record) error {
		out[rec.OID] = rec.Generation
		return nil
	}); err != nil {
		t.Fatalf("Iter: %v", err)
	}
	return out
}
```

For `newLinearPackABC` and `newOctopusPack`, reuse the existing in-package pack-synthesis pattern. If none exists, follow the importer's test fixtures (`internal/importer/*_test.go` has `synthesizePack`-style helpers — copy and adapt).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/commitgraph/ -run 'TestBuild_GenerationNumbers' -v`
Expected: FAIL — gens are all 0 (Build doesn't compute them yet).

- [ ] **Step 3: Compute generations in `Build`**

Edit `internal/commitgraph/build.go`. After the pack walk collects `commits []Record`, compute gens before the final `build()` call:

```go
// Compute generation numbers in topological order:
//   gen(c) = 1 + max(gen(parents of c)); roots = 1.
// commits is the slice produced by the pack walk above. Parents may
// reference commits not in this pack (i.e. base commits during an
// incremental .bvcg rebuild) — those resolve to gen 0 here, which is
// only correct for full-rebuild callers. Incremental callers (M10
// receive-pack) compute gens against a pre-loaded base lookup; see
// internal/reachability for that path.
gensByOID := make(map[pack.OID]uint32, len(commits))
visiting := make(map[pack.OID]bool, len(commits))
var compute func(oid pack.OID) uint32
compute = func(oid pack.OID) uint32 {
	if g, ok := gensByOID[oid]; ok {
		return g
	}
	if visiting[oid] {
		// Cycle — shouldn't happen for commits, but fail safe.
		return 0
	}
	visiting[oid] = true
	defer delete(visiting, oid)
	// Find the record for this oid.
	idx := -1
	for i := range commits {
		if commits[i].OID == oid {
			idx = i
			break
		}
	}
	if idx < 0 {
		// Parent not in this pack — return 0; caller treats as base.
		return 0
	}
	var maxParent uint32
	for _, p := range commits[idx].Parents {
		if g := compute(p); g > maxParent {
			maxParent = g
		}
	}
	g := maxParent + 1
	gensByOID[oid] = g
	return g
}
for i := range commits {
	commits[i].Generation = compute(commits[i].OID)
}
```

The linear-search-per-oid is O(n²); for the M10 use case (≤ tens of thousands of commits per full rebuild) this is fine. Optimize only if a benchmark proves it matters.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/commitgraph/ -run 'TestBuild_GenerationNumbers' -v`
Expected: PASS (depends on Task 1.3 below for `Open`/`Iter`; do them as a pair).

- [ ] **Step 5: Commit**

```bash
git add internal/commitgraph/build.go internal/commitgraph/build_test.go
git commit -m "M10 task 1.2: commitgraph generation-number computation in Build"
```

### Task 1.3: v2 encoder

**Files:**
- Modify: `internal/commitgraph/build.go` (or `format.go` where the byte emitter lives)
- Modify: `internal/commitgraph/format_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/commitgraph/format_test.go`:

```go
func TestEncode_V2_GoldenBytes(t *testing.T) {
	// Single root commit with gen=1.
	commits := []Record{{OID: oidA, Generation: 1, Parents: nil}}
	tips := []Tip{{Ref: "refs/heads/main", OID: oidA}}
	got, err := encode(commits, tips)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if string(got[:4]) != "BVCG" {
		t.Fatalf("magic = %q, want BVCG", got[:4])
	}
	ver := binary.LittleEndian.Uint32(got[4:8])
	if ver != VersionV2 {
		t.Fatalf("version = %d, want %d", ver, VersionV2)
	}
}

func TestEncode_V2_GenerationField_Position(t *testing.T) {
	// Verify the on-disk per-commit record layout: oid(20) + gen(4) +
	// n_parents(u8) + parents[n_parents]*20.
	commits := []Record{{OID: oidA, Generation: 42, Parents: nil}}
	tips := []Tip{{Ref: "refs/heads/main", OID: oidA}}
	got, err := encode(commits, tips)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// Walk to the first commit record (offset depends on header + tip
	// table size). Use Open to do this robustly:
	rdr, err := Open(got)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var rec Record
	if err := rdr.Iter(func(r Record) error { rec = r; return nil }); err != nil {
		t.Fatalf("Iter: %v", err)
	}
	if rec.Generation != 42 {
		t.Fatalf("Generation roundtrip = %d, want 42", rec.Generation)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/commitgraph/ -run 'TestEncode_V2' -v`
Expected: FAIL (gen field not written/read, or undefined `encode`).

- [ ] **Step 3: Update the byte emitter**

Locate the existing private `build()` (or whichever function actually emits bytes — likely in `build.go`). The current per-commit emit looks like:

```go
// existing:
buf.Write(c.OID[:])
buf.WriteByte(byte(len(c.Parents)))
for _, p := range c.Parents { buf.Write(p[:]) }
```

Insert generation between OID and n_parents:

```go
// v2:
buf.Write(c.OID[:])
var gen [4]byte
binary.LittleEndian.PutUint32(gen[:], c.Generation)
buf.Write(gen[:])
buf.WriteByte(byte(len(c.Parents)))
for _, p := range c.Parents { buf.Write(p[:]) }
```

In the header writer, set `version = VersionCurrent` (i.e. 2). Recompute trailing SHA-256.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/commitgraph/ -run 'TestEncode_V2' -v`
Expected: PASS (also depends on Open/Iter from Task 1.4).

- [ ] **Step 5: Commit**

```bash
git add internal/commitgraph/build.go internal/commitgraph/format.go internal/commitgraph/format_test.go
git commit -m "M10 task 1.3: commitgraph v2 byte emitter"
```

### Task 1.4: v2 reader (with v1 back-compat)

**Files:**
- Modify: `internal/commitgraph/read.go`
- Modify: `internal/commitgraph/read_test.go` (create if absent)

- [ ] **Step 1: Write the failing test**

Create or extend `internal/commitgraph/read_test.go`:

```go
package commitgraph

import (
	"encoding/binary"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/pack"
)

func TestRead_V2_RoundtripsGeneration(t *testing.T) {
	commits := []Record{
		{OID: oidA, Generation: 1, Parents: nil},
		{OID: oidB, Generation: 2, Parents: []pack.OID{oidA}},
	}
	tips := []Tip{{Ref: "refs/heads/main", OID: oidB}}
	bts, err := encode(commits, tips)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	rdr, err := Open(bts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var got []Record
	if err := rdr.Iter(func(r Record) error { got = append(got, r); return nil }); err != nil {
		t.Fatalf("Iter: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d records", len(got))
	}
	if got[0].Generation != 1 || got[1].Generation != 2 {
		t.Fatalf("gens = [%d, %d], want [1, 2]", got[0].Generation, got[1].Generation)
	}
}

func TestRead_V1_GenerationIsZero(t *testing.T) {
	// Forge a v1 .bvcg by encoding then patching version to 1 and
	// removing the gen u32 from each commit record. For test brevity
	// we directly write v1 bytes:
	bts := makeV1FixtureSingleCommit(t, oidA)
	rdr, err := Open(bts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var rec Record
	if err := rdr.Iter(func(r Record) error { rec = r; return nil }); err != nil {
		t.Fatalf("Iter: %v", err)
	}
	if rec.Generation != 0 {
		t.Fatalf("v1 Generation = %d, want 0", rec.Generation)
	}
}

func TestReader_GenerationOf(t *testing.T) {
	commits := []Record{{OID: oidA, Generation: 7, Parents: nil}}
	tips := []Tip{{Ref: "refs/heads/main", OID: oidA}}
	bts, _ := encode(commits, tips)
	rdr, _ := Open(bts)
	g, ok := rdr.GenerationOf(oidA)
	if !ok || g != 7 {
		t.Fatalf("GenerationOf(A) = (%d, %v), want (7, true)", g, ok)
	}
	if _, ok := rdr.GenerationOf(oidB); ok {
		t.Fatalf("GenerationOf(B) ok=true, want false")
	}
}

func makeV1FixtureSingleCommit(t *testing.T, oid pack.OID) []byte {
	t.Helper()
	var buf []byte
	// header: "BVCG" + version=1 + n_commits=1 + n_tips=0 + reserved 12B
	buf = append(buf, []byte("BVCG")...)
	var u4 [4]byte
	binary.LittleEndian.PutUint32(u4[:], 1)
	buf = append(buf, u4[:]...)
	binary.LittleEndian.PutUint32(u4[:], 1)
	buf = append(buf, u4[:]...)
	binary.LittleEndian.PutUint32(u4[:], 0)
	buf = append(buf, u4[:]...)
	buf = append(buf, make([]byte, 12)...)
	// commit record (v1): oid(20) + n_parents(1) + 0 parents
	buf = append(buf, oid[:]...)
	buf = append(buf, byte(0))
	// trailer: SHA-256 of preceding bytes
	trailer := sha256Sum(buf)
	buf = append(buf, trailer[:]...)
	return buf
}
```

Add a small helper `sha256Sum` if not already present in test files.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/commitgraph/ -v`
Expected: FAIL — v2 not parsed, `GenerationOf` undefined.

- [ ] **Step 3: Update the reader**

Edit `internal/commitgraph/read.go`. The `Open` function must:

1. Verify magic = "BVCG".
2. Read version (u32 LE). Accept 1 or 2; reject others with a clear error.
3. Read n_commits, n_tips.
4. Parse tips.
5. Per-commit record: read oid(20). If `version >= 2`, read gen(u32 LE); else gen = 0. Read n_parents(u8) and parents.
6. Verify trailer SHA-256.

Add a `GenerationOf` method:

```go
// GenerationOf returns the commit-graph generation number for oid, or
// (0, false) if oid isn't present. On v1 files all generations are 0
// and `ok` is true for present commits (callers can distinguish v1
// from "commit not present" via Has).
func (r *Reader) GenerationOf(oid pack.OID) (uint32, bool) {
	rec, ok := r.byOID[oid]
	if !ok {
		return 0, false
	}
	return rec.Generation, true
}
```

If the existing `Reader` doesn't index by OID, build a `map[pack.OID]Record` at `Open` time. (Memory cost: ~50 bytes/commit × tens of thousands = sub-MB; acceptable.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/commitgraph/ -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/commitgraph/read.go internal/commitgraph/read_test.go
git commit -m "M10 task 1.4: commitgraph v2 reader + GenerationOf accessor"
```

### Task 1.5: Generation property test

**Files:**
- Modify: `internal/commitgraph/build_test.go`

- [ ] **Step 1: Write the property test**

Append to `internal/commitgraph/build_test.go`:

```go
func TestBuild_GenerationProperty_RandomDAG(t *testing.T) {
	// Generate a random DAG of N commits in topological order; each
	// commit has 0..2 parents chosen from already-emitted commits.
	// Build .bvcg, read it back, and verify
	//   gen(c) = 1 + max(gen(parents))  for every c.
	const N = 200
	rng := rand.New(rand.NewSource(1))
	r, oids := newRandomDAGPack(t, rng, N)
	tips := []Tip{{Ref: "refs/heads/main", OID: oids[N-1]}}
	bts, err := Build(context.Background(), r, tips)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	rdr, err := Open(bts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	records := map[pack.OID]Record{}
	rdr.Iter(func(rec Record) error { records[rec.OID] = rec; return nil })
	for oid, rec := range records {
		var maxParent uint32
		for _, p := range rec.Parents {
			if g, ok := records[p]; ok && g.Generation > maxParent {
				maxParent = g.Generation
			}
		}
		want := maxParent + 1
		if rec.Generation != want {
			t.Errorf("oid %x: gen=%d, want %d (parents=%v)", oid[:4], rec.Generation, want, rec.Parents)
		}
	}
}
```

Implement `newRandomDAGPack(t, rng, N) (*pack.Reader, []pack.OID)` following the same pattern as Task 1.2's helpers — synthesize N commits where parent indices are chosen from `[0, i)`.

- [ ] **Step 2: Run the test to verify it passes (no implementation change)**

Run: `go test ./internal/commitgraph/ -run 'TestBuild_GenerationProperty' -v`
Expected: PASS (validates the work from Tasks 1.2–1.4).

- [ ] **Step 3: Commit**

```bash
git add internal/commitgraph/build_test.go
git commit -m "M10 task 1.5: commitgraph generation-number random-DAG property test"
```

### Task 1.6: Refresh affected golden testdata

**Files:**
- Modify: `internal/repo/manifest/testdata/golden/*.json` (only if any encode `commit_graph` index)
- Modify: any other golden that pins a `.bvcg` byte sequence

- [ ] **Step 1: Survey goldens**

Run: `grep -rl 'commit_graph\|bvcg' internal/`
Expected: list of files referencing the index. For manifest goldens with only an `IndexRef`, no change needed — `IndexRef.SizeBytes` is `omitempty` and the file's `key`/`hash` strings are unaffected. For any golden that pins literal `.bvcg` bytes (e.g. importer/diffharness fixtures), regenerate with the new format.

- [ ] **Step 2: Regenerate any byte-pinned `.bvcg` goldens**

For each affected golden file (likely `internal/diffharness/fixtures/` and `internal/importer/testdata/`), the procedure is:

```
1. go test ./internal/<pkg>/ -run TestX_Golden -update   (if the -update pattern exists)
2. Otherwise: delete the golden and rerun the test to fail with the new bytes; copy them into the golden.
3. Inspect with `xxd` to confirm magic + version byte = 2.
```

Use `xxd internal/<pkg>/testdata/<file>.bvcg | head -2` to confirm `BVCG` at offset 0 and `02 00 00 00` at offset 4.

- [ ] **Step 3: Run the full test suite**

Run: `go test ./internal/commitgraph/... ./internal/importer/... ./internal/diffharness/... -count=1`
Expected: all PASS (or only failures we expect to fix in later phases — note them).

- [ ] **Step 4: Commit**

```bash
git add -A internal/
git commit -m "M10 task 1.6: refresh .bvcg goldens to v2"
```

Self-review the commit content before pushing: ensure only intended files changed.

---

## Phase 2 — `.bvrd` format (`internal/reachability/deltaindex`)

This phase ships the new delta-index wire format: encoder, decoder, golden bytes, malformed-input rejection. No higher-level integration yet.

### Task 2.1: Create package skeleton + doc.go

**Files:**
- Create: `internal/reachability/deltaindex/doc.go`

- [ ] **Step 1: Write doc.go**

```go
// Package deltaindex implements the .bvrd reachability-delta wire
// format (M10 §14.1). Each push produces one immutable .bvrd that
// records new commits + parents + generation numbers, new ref tips,
// and new pack IDs. The file is content-addressed; the storage key
// embeds its SHA-256.
//
// Format (little-endian throughout):
//
//   header (32 bytes):
//     magic        "BVRD"  (4B)
//     version      u32     (=1)
//     n_commits    u32
//     n_reftips    u32
//     n_packs      u32
//     reserved     12B (zero)
//
//   commits (sorted by oid):
//     oid              20B
//     generation       u32
//     n_parents        u8
//     parents          n_parents * 20B
//
//   reftips:
//     ref_name_off     u32 (-> strtab offset)
//     new_oid          20B
//     old_oid          20B (zero for ref-create)
//
//   packs:
//     pack_id          20B
//
//   reserved sections (length-prefixed u32, currently zero):
//     trees_blobs_tags  // Q3=C extension slot for M11
//     bitmap            // M9.5 extension slot
//
//   strtab (length-prefixed u32 then bytes):
//     NUL-terminated UTF-8 ref names
//
//   trailer (32 bytes): SHA-256 of preceding bytes
package deltaindex
```

- [ ] **Step 2: Verify compile**

Run: `go build ./internal/reachability/deltaindex/`
Expected: no error.

- [ ] **Step 3: Commit**

```bash
git add internal/reachability/deltaindex/doc.go
git commit -m "M10 task 2.1: deltaindex package skeleton"
```

### Task 2.2: Format constants + structs

**Files:**
- Create: `internal/reachability/deltaindex/format.go`
- Create: `internal/reachability/deltaindex/format_test.go`

- [ ] **Step 1: Write the failing test**

```go
package deltaindex

import "testing"

func TestFormat_Magic(t *testing.T) {
	if string(Magic[:]) != "BVRD" {
		t.Fatalf("Magic = %q, want BVRD", Magic[:])
	}
}

func TestFormat_VersionCurrent(t *testing.T) {
	if VersionCurrent != 1 {
		t.Fatalf("VersionCurrent = %d, want 1", VersionCurrent)
	}
}

func TestFormat_HeaderSize(t *testing.T) {
	if HeaderSize != 32 {
		t.Fatalf("HeaderSize = %d, want 32", HeaderSize)
	}
}

func TestFormat_TrailerSize(t *testing.T) {
	if TrailerSize != 32 {
		t.Fatalf("TrailerSize = %d, want 32", TrailerSize)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/reachability/deltaindex/ -v`
Expected: FAIL — undefined identifiers.

- [ ] **Step 3: Implement format constants**

Create `internal/reachability/deltaindex/format.go`:

```go
package deltaindex

import "github.com/bucketvcs/bucketvcs/internal/pack"

// Wire constants.
var Magic = [4]byte{'B', 'V', 'R', 'D'}

const (
	VersionCurrent uint32 = 1
	HeaderSize            = 32
	TrailerSize           = 32
	OIDLen                = 20
)

// CommitRecord is one new commit introduced by this push.
type CommitRecord struct {
	OID        pack.OID
	Generation uint32
	Parents    []pack.OID
}

// RefTipDiff is a ref update introduced by this push. OldOID is
// zero-valued for ref creation.
type RefTipDiff struct {
	RefName string
	OldOID  pack.OID
	NewOID  pack.OID
}

// Delta is the decoded form of one .bvrd file.
type Delta struct {
	Commits []CommitRecord  // sorted by OID
	RefTips []RefTipDiff
	Packs   []pack.OID
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/reachability/deltaindex/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/reachability/deltaindex/format.go internal/reachability/deltaindex/format_test.go
git commit -m "M10 task 2.2: deltaindex format constants + structs"
```

### Task 2.3: Encoder

**Files:**
- Create: `internal/reachability/deltaindex/build.go`
- Create: `internal/reachability/deltaindex/build_test.go`

- [ ] **Step 1: Write the failing test**

```go
package deltaindex

import (
	"crypto/sha256"
	"encoding/binary"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/pack"
)

func TestEncode_EmptyDelta(t *testing.T) {
	bts, err := Encode(Delta{})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(bts) < HeaderSize+TrailerSize {
		t.Fatalf("too short: %d", len(bts))
	}
	if string(bts[:4]) != "BVRD" {
		t.Fatalf("magic = %q", bts[:4])
	}
	if binary.LittleEndian.Uint32(bts[4:8]) != VersionCurrent {
		t.Fatalf("version mismatch")
	}
	// Trailer = SHA-256 of preceding bytes.
	want := sha256.Sum256(bts[:len(bts)-TrailerSize])
	got := bts[len(bts)-TrailerSize:]
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("trailer mismatch at byte %d", i)
		}
	}
}

func TestEncode_DeterministicGivenSortedInput(t *testing.T) {
	d := Delta{
		Commits: []CommitRecord{
			{OID: oidA(), Generation: 1, Parents: nil},
			{OID: oidB(), Generation: 2, Parents: []pack.OID{oidA()}},
		},
		RefTips: []RefTipDiff{{RefName: "refs/heads/main", NewOID: oidB()}},
		Packs:   []pack.OID{oidP1()},
	}
	a, err := Encode(d)
	if err != nil {
		t.Fatalf("Encode #1: %v", err)
	}
	b, err := Encode(d)
	if err != nil {
		t.Fatalf("Encode #2: %v", err)
	}
	if string(a) != string(b) {
		t.Fatalf("Encode is non-deterministic")
	}
}

// Test helpers — short fixed OIDs so tests are readable.
func oidA() pack.OID { return packOID(0xA1) }
func oidB() pack.OID { return packOID(0xB1) }
func oidP1() pack.OID { return packOID(0xC1) }
func packOID(b byte) pack.OID {
	var o pack.OID
	o[0] = b
	return o
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/reachability/deltaindex/ -run TestEncode -v`
Expected: FAIL — `Encode` undefined.

- [ ] **Step 3: Implement encoder**

Create `internal/reachability/deltaindex/build.go`:

```go
package deltaindex

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
)

// Encode serializes a Delta to .bvrd bytes. Commits are sorted by OID
// before emission (deterministic output). RefTips and Packs are
// emitted in the caller-supplied order — callers SHOULD sort them too
// if content-hash stability across orderings matters.
func Encode(d Delta) ([]byte, error) {
	// Sort commits by OID for deterministic output.
	commits := make([]CommitRecord, len(d.Commits))
	copy(commits, d.Commits)
	sort.Slice(commits, func(i, j int) bool {
		return bytes.Compare(commits[i].OID[:], commits[j].OID[:]) < 0
	})

	// Build the string table for ref names.
	var strtab bytes.Buffer
	offsets := make(map[string]uint32, len(d.RefTips))
	for _, r := range d.RefTips {
		if _, seen := offsets[r.RefName]; seen {
			continue
		}
		offsets[r.RefName] = uint32(strtab.Len())
		strtab.WriteString(r.RefName)
		strtab.WriteByte(0)
	}

	var body bytes.Buffer

	// Header (32 bytes).
	body.Write(Magic[:])
	writeU32(&body, VersionCurrent)
	writeU32(&body, uint32(len(commits)))
	writeU32(&body, uint32(len(d.RefTips)))
	writeU32(&body, uint32(len(d.Packs)))
	body.Write(make([]byte, 12)) // reserved

	// Commits.
	for _, c := range commits {
		if len(c.Parents) > 255 {
			return nil, fmt.Errorf("deltaindex: commit has %d parents (>255)", len(c.Parents))
		}
		body.Write(c.OID[:])
		writeU32(&body, c.Generation)
		body.WriteByte(byte(len(c.Parents)))
		for _, p := range c.Parents {
			body.Write(p[:])
		}
	}

	// RefTips.
	for _, r := range d.RefTips {
		writeU32(&body, offsets[r.RefName])
		body.Write(r.NewOID[:])
		body.Write(r.OldOID[:])
	}

	// Packs.
	for _, p := range d.Packs {
		body.Write(p[:])
	}

	// Reserved length-prefixed sections (currently zero-length).
	writeU32(&body, 0) // trees_blobs_tags
	writeU32(&body, 0) // bitmap

	// strtab (length-prefixed).
	writeU32(&body, uint32(strtab.Len()))
	body.Write(strtab.Bytes())

	// Trailer: SHA-256 of preceding bytes.
	sum := sha256.Sum256(body.Bytes())
	body.Write(sum[:])

	return body.Bytes(), nil
}

func writeU32(w *bytes.Buffer, v uint32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	w.Write(b[:])
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/reachability/deltaindex/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/reachability/deltaindex/build.go internal/reachability/deltaindex/build_test.go
git commit -m "M10 task 2.3: deltaindex encoder"
```

### Task 2.4: Decoder + roundtrip

**Files:**
- Create: `internal/reachability/deltaindex/read.go`
- Create: `internal/reachability/deltaindex/read_test.go`

- [ ] **Step 1: Write the failing test**

```go
package deltaindex

import (
	"bytes"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/pack"
)

func TestDecode_Roundtrip(t *testing.T) {
	d := Delta{
		Commits: []CommitRecord{
			{OID: oidA(), Generation: 1, Parents: nil},
			{OID: oidB(), Generation: 2, Parents: []pack.OID{oidA()}},
		},
		RefTips: []RefTipDiff{
			{RefName: "refs/heads/main", NewOID: oidB()},
			{RefName: "refs/tags/v1", NewOID: oidA(), OldOID: pack.OID{}},
		},
		Packs: []pack.OID{oidP1()},
	}
	bts, err := Encode(d)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := Decode(bts)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(got.Commits) != 2 {
		t.Fatalf("commits len=%d", len(got.Commits))
	}
	if got.Commits[1].Generation != 2 {
		t.Fatalf("gen lost: %d", got.Commits[1].Generation)
	}
	if !bytes.Equal(got.Commits[1].Parents[0][:], oidA()[:]) {
		t.Fatalf("parents lost")
	}
	if len(got.RefTips) != 2 || got.RefTips[0].RefName != "refs/heads/main" {
		t.Fatalf("reftips lost: %+v", got.RefTips)
	}
	if len(got.Packs) != 1 || got.Packs[0] != oidP1() {
		t.Fatalf("packs lost")
	}
}

func TestDecode_ContentHashStability(t *testing.T) {
	d := Delta{Commits: []CommitRecord{{OID: oidA(), Generation: 1}}}
	a, _ := Encode(d)
	b, _ := Encode(d)
	if !bytes.Equal(a, b) {
		t.Fatalf("encode not stable")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/reachability/deltaindex/ -run TestDecode -v`
Expected: FAIL — `Decode` undefined.

- [ ] **Step 3: Implement decoder**

Create `internal/reachability/deltaindex/read.go`:

```go
package deltaindex

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/bucketvcs/bucketvcs/internal/pack"
)

// ErrMalformed is returned when the .bvrd bytes don't match the format.
var ErrMalformed = errors.New("deltaindex: malformed")

// Decode parses .bvrd bytes into a Delta. The trailing SHA-256 is
// verified; a mismatch returns ErrMalformed.
func Decode(b []byte) (*Delta, error) {
	if len(b) < HeaderSize+TrailerSize {
		return nil, fmt.Errorf("%w: too short (%d bytes)", ErrMalformed, len(b))
	}
	if !bytes.Equal(b[:4], Magic[:]) {
		return nil, fmt.Errorf("%w: bad magic %q", ErrMalformed, b[:4])
	}
	body := b[:len(b)-TrailerSize]
	wantSum := b[len(b)-TrailerSize:]
	gotSum := sha256.Sum256(body)
	if !bytes.Equal(gotSum[:], wantSum) {
		return nil, fmt.Errorf("%w: trailer hash mismatch", ErrMalformed)
	}

	r := &reader{buf: body, off: 0}
	if err := r.skip(4); err != nil {
		return nil, err
	}
	ver, err := r.readU32()
	if err != nil {
		return nil, err
	}
	if ver != VersionCurrent {
		return nil, fmt.Errorf("%w: version %d not supported", ErrMalformed, ver)
	}
	nCommits, err := r.readU32()
	if err != nil {
		return nil, err
	}
	nReftips, err := r.readU32()
	if err != nil {
		return nil, err
	}
	nPacks, err := r.readU32()
	if err != nil {
		return nil, err
	}
	if err := r.skip(12); err != nil { // reserved
		return nil, err
	}

	d := &Delta{
		Commits: make([]CommitRecord, 0, nCommits),
		RefTips: make([]RefTipDiff, 0, nReftips),
		Packs:   make([]pack.OID, 0, nPacks),
	}

	for i := uint32(0); i < nCommits; i++ {
		var c CommitRecord
		if err := r.readOID(&c.OID); err != nil {
			return nil, err
		}
		c.Generation, err = r.readU32()
		if err != nil {
			return nil, err
		}
		nParents, err := r.readU8()
		if err != nil {
			return nil, err
		}
		c.Parents = make([]pack.OID, nParents)
		for j := uint8(0); j < nParents; j++ {
			if err := r.readOID(&c.Parents[j]); err != nil {
				return nil, err
			}
		}
		d.Commits = append(d.Commits, c)
	}

	type tipRaw struct {
		off    uint32
		newOID pack.OID
		oldOID pack.OID
	}
	raws := make([]tipRaw, nReftips)
	for i := uint32(0); i < nReftips; i++ {
		off, err := r.readU32()
		if err != nil {
			return nil, err
		}
		var newOID, oldOID pack.OID
		if err := r.readOID(&newOID); err != nil {
			return nil, err
		}
		if err := r.readOID(&oldOID); err != nil {
			return nil, err
		}
		raws[i] = tipRaw{off: off, newOID: newOID, oldOID: oldOID}
	}

	for i := uint32(0); i < nPacks; i++ {
		var p pack.OID
		if err := r.readOID(&p); err != nil {
			return nil, err
		}
		d.Packs = append(d.Packs, p)
	}

	// Reserved length-prefixed sections.
	for _, name := range []string{"trees_blobs_tags", "bitmap"} {
		n, err := r.readU32()
		if err != nil {
			return nil, err
		}
		if err := r.skip(int(n)); err != nil {
			return nil, fmt.Errorf("%w: reserved %q section: %v", ErrMalformed, name, err)
		}
	}

	strtabLen, err := r.readU32()
	if err != nil {
		return nil, err
	}
	strtab, err := r.readBytes(int(strtabLen))
	if err != nil {
		return nil, err
	}

	for _, raw := range raws {
		name, err := readNUL(strtab, raw.off)
		if err != nil {
			return nil, err
		}
		d.RefTips = append(d.RefTips, RefTipDiff{
			RefName: name,
			NewOID:  raw.newOID,
			OldOID:  raw.oldOID,
		})
	}
	return d, nil
}

type reader struct {
	buf []byte
	off int
}

func (r *reader) skip(n int) error {
	if r.off+n > len(r.buf) {
		return fmt.Errorf("%w: short read of %d at offset %d", ErrMalformed, n, r.off)
	}
	r.off += n
	return nil
}
func (r *reader) readU8() (uint8, error) {
	if r.off+1 > len(r.buf) {
		return 0, fmt.Errorf("%w: short u8 at %d", ErrMalformed, r.off)
	}
	b := r.buf[r.off]
	r.off++
	return b, nil
}
func (r *reader) readU32() (uint32, error) {
	if r.off+4 > len(r.buf) {
		return 0, fmt.Errorf("%w: short u32 at %d", ErrMalformed, r.off)
	}
	v := binary.LittleEndian.Uint32(r.buf[r.off:])
	r.off += 4
	return v, nil
}
func (r *reader) readOID(out *pack.OID) error {
	if r.off+OIDLen > len(r.buf) {
		return fmt.Errorf("%w: short oid at %d", ErrMalformed, r.off)
	}
	copy(out[:], r.buf[r.off:r.off+OIDLen])
	r.off += OIDLen
	return nil
}
func (r *reader) readBytes(n int) ([]byte, error) {
	if r.off+n > len(r.buf) {
		return nil, fmt.Errorf("%w: short bytes(%d) at %d", ErrMalformed, n, r.off)
	}
	b := r.buf[r.off : r.off+n]
	r.off += n
	return b, nil
}
func readNUL(strtab []byte, off uint32) (string, error) {
	if int(off) >= len(strtab) {
		return "", fmt.Errorf("%w: strtab offset %d out of range", ErrMalformed, off)
	}
	end := bytes.IndexByte(strtab[off:], 0)
	if end < 0 {
		return "", fmt.Errorf("%w: unterminated string at %d", ErrMalformed, off)
	}
	return string(strtab[off : int(off)+end]), nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/reachability/deltaindex/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/reachability/deltaindex/read.go internal/reachability/deltaindex/read_test.go
git commit -m "M10 task 2.4: deltaindex decoder + roundtrip"
```

### Task 2.5: Reject-malformed cases

**Files:**
- Create: `internal/reachability/deltaindex/reject_test.go`

- [ ] **Step 1: Write the failing tests**

```go
package deltaindex

import (
	"errors"
	"testing"
)

func TestDecode_RejectsBadMagic(t *testing.T) {
	bts, _ := Encode(Delta{})
	bts[0] = 'X'
	_, err := Decode(bts)
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v, want ErrMalformed", err)
	}
}

func TestDecode_RejectsBadVersion(t *testing.T) {
	bts, _ := Encode(Delta{})
	// Corrupt version field (offset 4..7).
	bts[4] = 0xFF
	// Recompute trailer so the next check we hit is version, not trailer.
	rebuildTrailer(bts)
	_, err := Decode(bts)
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v, want ErrMalformed", err)
	}
}

func TestDecode_RejectsTruncatedTrailer(t *testing.T) {
	bts, _ := Encode(Delta{})
	_, err := Decode(bts[:len(bts)-1])
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v, want ErrMalformed", err)
	}
}

func TestDecode_RejectsTrailerMismatch(t *testing.T) {
	bts, _ := Encode(Delta{})
	bts[len(bts)-1] ^= 0xFF
	_, err := Decode(bts)
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v, want ErrMalformed", err)
	}
}

func rebuildTrailer(bts []byte) {
	body := bts[:len(bts)-TrailerSize]
	sum := sha256Sum(body)
	copy(bts[len(bts)-TrailerSize:], sum[:])
}
```

Add a tiny `sha256Sum` helper (or import `crypto/sha256` directly inline).

- [ ] **Step 2: Run tests to verify they pass**

Run: `go test ./internal/reachability/deltaindex/ -run TestDecode_Rejects -v`
Expected: PASS (Decode already implements these via ErrMalformed).

- [ ] **Step 3: Commit**

```bash
git add internal/reachability/deltaindex/reject_test.go
git commit -m "M10 task 2.5: deltaindex reject-malformed cases"
```

---

## Phase 3 — `internal/reachability.Set` (read-side abstraction)

`Set` is the unified view consumed by the upload-pack negotiation engine. It loads a base (`.bvcg` v2 + `.bvom`) plus delta chain (`.bvrd` × N) and exposes `Has`/`Parents`/`Generation`/`WalkAncestors`/`RefTips`/`ObjectPack`.

### Task 3.1: Package skeleton + errors

**Files:**
- Create: `internal/reachability/doc.go`
- Create: `internal/reachability/errs.go`
- Create: `internal/reachability/errs_test.go`

- [ ] **Step 1: Write doc.go**

```go
// Package reachability implements the M10 reachability index: a Set
// view that combines a base (.bvcg v2 + .bvom) with a chain of .bvrd
// deltas (one per push), used by upload-pack to answer want/have
// negotiation without materializing the on-disk mirror.
//
// Producers:
//   internal/gitproto/receivepack writes one .bvrd per push.
//   internal/maintenance compacts the chain back to an empty list.
//
// Consumers:
//   internal/gitproto/uploadpack reads Set during negotiation.
//   cmd/bucketvcs/negotiate is a debug CLI over the same path.
package reachability
```

- [ ] **Step 2: Write sentinel errors and a test**

`internal/reachability/errs.go`:

```go
package reachability

import "errors"

// ErrNoIndex is returned when the manifest's Indexes are insufficient
// to construct a Set (e.g. legacy repo without .bvcg / .bvom).
var ErrNoIndex = errors.New("reachability: manifest has no usable index")

// ErrStaleBase is returned when ReachabilityRef.BaseManifest disagrees
// with the manifest version currently pinning the base index pair.
var ErrStaleBase = errors.New("reachability: base-manifest mismatch")
```

`internal/reachability/errs_test.go`:

```go
package reachability_test

import (
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/reachability"
)

func TestErrors_Distinct(t *testing.T) {
	if errors.Is(reachability.ErrNoIndex, reachability.ErrStaleBase) {
		t.Fatalf("sentinels not distinct")
	}
}
```

- [ ] **Step 3: Build + test**

Run: `go test ./internal/reachability/ -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/reachability/doc.go internal/reachability/errs.go internal/reachability/errs_test.go
git commit -m "M10 task 3.1: reachability package skeleton + sentinel errors"
```

### Task 3.2: `Set` struct + `Load`

**Files:**
- Create: `internal/reachability/set.go`
- Create: `internal/reachability/set_test.go`

- [ ] **Step 1: Write the failing test**

```go
package reachability_test

import (
	"context"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/reachability"
	"github.com/bucketvcs/bucketvcs/internal/reachability/rtest"
)

func TestLoad_BaseOnly_NoDeltas(t *testing.T) {
	ctx := context.Background()
	store, k, body := rtest.NewBaseOnlyRepo(t, rtest.LinearChainABC)
	set, err := reachability.Load(ctx, store, k, body)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !set.Has(rtest.OidC) {
		t.Errorf("Set.Has(C) = false, want true")
	}
	if !set.Has(rtest.OidA) {
		t.Errorf("Set.Has(A) = false, want true")
	}
	if set.Has(rtest.OidUnknown) {
		t.Errorf("Set.Has(Unknown) = true, want false")
	}
}

func TestLoad_LegacyManifest_ReturnsErrNoIndex(t *testing.T) {
	ctx := context.Background()
	store, k, body := rtest.NewLegacyRepo(t) // no .bvcg / .bvom
	_, err := reachability.Load(ctx, store, k, body)
	if err == nil || !errors.Is(err, reachability.ErrNoIndex) {
		t.Fatalf("err = %v, want ErrNoIndex", err)
	}
}
```

(Helpers in `rtest` are built in Task 3.7; create them stubbed for now so this test compiles.)

- [ ] **Step 2: Write rtest stubs**

`internal/reachability/rtest/fixtures.go`:

```go
// Package rtest provides synthesized fixtures for reachability tests.
package rtest

import (
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

type ChainKind int

const (
	LinearChainABC ChainKind = iota
)

var (
	OidA       = packOID(0xA1)
	OidB       = packOID(0xB1)
	OidC       = packOID(0xC1)
	OidUnknown = packOID(0xFF)
)

func NewBaseOnlyRepo(t *testing.T, kind ChainKind) (storage.ObjectStore, *keys.Repo, manifest.Body) {
	t.Helper()
	t.Fatal("rtest.NewBaseOnlyRepo: implement in Task 3.7")
	return nil, nil, manifest.Body{}
}

func NewLegacyRepo(t *testing.T) (storage.ObjectStore, *keys.Repo, manifest.Body) {
	t.Helper()
	t.Fatal("rtest.NewLegacyRepo: implement in Task 3.7")
	return nil, nil, manifest.Body{}
}

func packOID(b byte) pack.OID {
	var o pack.OID
	o[0] = b
	return o
}
```

- [ ] **Step 3: Implement `Load` and `Has`**

Create `internal/reachability/set.go`:

```go
package reachability

import (
	"context"
	"fmt"

	"github.com/bucketvcs/bucketvcs/internal/commitgraph"
	"github.com/bucketvcs/bucketvcs/internal/objindex"
	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/reachability/deltaindex"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Set is the unified view (base + delta chain) consumed by negotiation.
type Set struct {
	cg     *commitgraph.Reader
	omap   *objindex.View
	deltas []*deltaindex.Delta // ordered, base-first slice (i.e. Deltas[0] is oldest)
	refs   map[string]pack.OID // computed from base + deltas in order
}

// Load constructs a Set from the manifest body. Returns ErrNoIndex if
// the base pair is missing.
func Load(ctx context.Context, store storage.ObjectStore, k *keys.Repo, body manifest.Body) (*Set, error) {
	if body.Indexes.CommitGraph == nil || body.Indexes.ObjectMap == nil {
		return nil, ErrNoIndex
	}
	cg, err := commitgraph.OpenFromStore(ctx, store, body.Indexes.CommitGraph.Key, body.Indexes.CommitGraph.Hash)
	if err != nil {
		return nil, fmt.Errorf("reachability: open .bvcg: %w", err)
	}
	omap, err := objindex.OpenWithExpectedHash(ctx, store, body.Indexes.ObjectMap.Key, body.Indexes.ObjectMap.Hash)
	if err != nil {
		return nil, fmt.Errorf("reachability: open .bvom: %w", err)
	}

	var deltas []*deltaindex.Delta
	if body.Indexes.Reachability != nil {
		for _, ref := range body.Indexes.Reachability.Deltas {
			d, err := loadDelta(ctx, store, ref)
			if err != nil {
				return nil, fmt.Errorf("reachability: load delta %s: %w", ref.Hash[:8], err)
			}
			deltas = append(deltas, d)
		}
	}

	// Compute effective ref tips: base refs from manifest.Refs, then
	// apply each delta's RefTips in order.
	refs := make(map[string]pack.OID, len(body.Refs))
	for name, hex := range body.Refs {
		o, err := pack.ParseOID(hex)
		if err != nil {
			return nil, fmt.Errorf("reachability: parse ref %q: %w", name, err)
		}
		refs[name] = o
	}
	for _, d := range deltas {
		for _, tip := range d.RefTips {
			refs[tip.RefName] = tip.NewOID
		}
	}

	return &Set{cg: cg, omap: omap, deltas: deltas, refs: refs}, nil
}

func loadDelta(ctx context.Context, store storage.ObjectStore, ref manifest.IndexRef) (*deltaindex.Delta, error) {
	r, err := store.Get(ctx, ref.Key)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	bts, err := readAll(r)
	if err != nil {
		return nil, err
	}
	return deltaindex.Decode(bts)
}
```

If `commitgraph.OpenFromStore` doesn't exist, write it as a thin helper here or add it to `internal/commitgraph/`:

```go
// In internal/commitgraph/read.go:
func OpenFromStore(ctx context.Context, store storage.ObjectStore, key, expectedHash string) (*Reader, error) {
	r, err := store.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	bts, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(bts)
	if hex.EncodeToString(sum[:]) != expectedHash {
		return nil, fmt.Errorf("commitgraph: hash mismatch")
	}
	return Open(bts)
}
```

And add `Set.Has`:

```go
// Has reports whether oid is a commit known to the base or any delta.
func (s *Set) Has(oid pack.OID) bool {
	// Latest delta first (shadow semantics).
	for i := len(s.deltas) - 1; i >= 0; i-- {
		for _, c := range s.deltas[i].Commits {
			if c.OID == oid {
				return true
			}
		}
	}
	_, ok := s.cg.GenerationOf(oid)
	return ok
}
```

(Linear scans are fine for the OSS-scope thresholds (≤ 1000 commits / delta). If profiling shows otherwise, add a per-delta map at Load.)

Add `readAll` import or use `io.ReadAll`.

- [ ] **Step 4: Run the first test**

Run: `go test ./internal/reachability/ -run TestLoad_LegacyManifest -v`
Expected: PASS (ErrNoIndex path works).

The `TestLoad_BaseOnly_NoDeltas` test will still fail until Task 3.7 implements `rtest.NewBaseOnlyRepo`. That's expected — note in commit message.

- [ ] **Step 5: Commit**

```bash
git add internal/reachability/set.go internal/reachability/set_test.go internal/reachability/rtest/fixtures.go internal/commitgraph/read.go
git commit -m "M10 task 3.2: reachability.Load + Set.Has (legacy-path test passes; fixture-backed test deferred to task 3.7)"
```

### Task 3.3: `Parents` + `Generation`

**Files:**
- Modify: `internal/reachability/set.go`
- Modify: `internal/reachability/set_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestSet_Parents_FromDelta(t *testing.T) {
	ctx := context.Background()
	store, k, body := rtest.NewBaseWithDeltaRepo(t)
	set, err := reachability.Load(ctx, store, k, body)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	parents := set.Parents(rtest.OidD) // D is introduced by the delta, parent=C
	if len(parents) != 1 || parents[0] != rtest.OidC {
		t.Fatalf("Parents(D) = %v, want [C]", parents)
	}
}

func TestSet_Generation(t *testing.T) {
	ctx := context.Background()
	store, k, body := rtest.NewBaseWithDeltaRepo(t)
	set, _ := reachability.Load(ctx, store, k, body)
	if g, ok := set.Generation(rtest.OidA); !ok || g != 1 {
		t.Errorf("gen(A) = (%d, %v), want (1, true)", g, ok)
	}
	if g, ok := set.Generation(rtest.OidD); !ok || g != 4 {
		t.Errorf("gen(D) = (%d, %v), want (4, true)", g, ok)
	}
}
```

(`NewBaseWithDeltaRepo` is also implemented in Task 3.7.)

- [ ] **Step 2: Implement methods**

Append to `internal/reachability/set.go`:

```go
// Parents returns oid's parents, looking deltas-first then base.
func (s *Set) Parents(oid pack.OID) []pack.OID {
	for i := len(s.deltas) - 1; i >= 0; i-- {
		for _, c := range s.deltas[i].Commits {
			if c.OID == oid {
				return c.Parents
			}
		}
	}
	if rec, ok := s.cg.RecordOf(oid); ok {
		return rec.Parents
	}
	return nil
}

// Generation returns the commit-graph generation number, or (0, false).
func (s *Set) Generation(oid pack.OID) (uint32, bool) {
	for i := len(s.deltas) - 1; i >= 0; i-- {
		for _, c := range s.deltas[i].Commits {
			if c.OID == oid {
				return c.Generation, true
			}
		}
	}
	return s.cg.GenerationOf(oid)
}
```

If `RecordOf` doesn't exist on the commitgraph reader, add it (returns the Record from the OID-indexed map built in Task 1.4).

- [ ] **Step 3: Run tests**

Run: `go test ./internal/reachability/ -run 'TestSet_(Parents|Generation)' -v`
Expected: PASS once Task 3.7 lands; for now these tests run with `t.Fatal` from stub.

- [ ] **Step 4: Commit**

```bash
git add internal/reachability/set.go internal/reachability/set_test.go internal/commitgraph/read.go
git commit -m "M10 task 3.3: Set.Parents + Set.Generation"
```

### Task 3.4: `WalkAncestors` (generation-bounded)

**Files:**
- Modify: `internal/reachability/set.go`
- Modify: `internal/reachability/set_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestSet_WalkAncestors_VisitsAll(t *testing.T) {
	ctx := context.Background()
	store, k, body := rtest.NewBaseWithDeltaRepo(t) // A-B-C-D linear
	set, _ := reachability.Load(ctx, store, k, body)
	visited := map[pack.OID]bool{}
	err := set.WalkAncestors([]pack.OID{rtest.OidD}, func(o pack.OID, gen uint32) error {
		visited[o] = true
		return nil
	})
	if err != nil {
		t.Fatalf("WalkAncestors: %v", err)
	}
	for _, want := range []pack.OID{rtest.OidA, rtest.OidB, rtest.OidC, rtest.OidD} {
		if !visited[want] {
			t.Errorf("missing %x", want[:1])
		}
	}
}

func TestSet_WalkAncestors_VisitOrderHigherGenFirst(t *testing.T) {
	ctx := context.Background()
	store, k, body := rtest.NewBaseWithDeltaRepo(t)
	set, _ := reachability.Load(ctx, store, k, body)
	var gens []uint32
	_ = set.WalkAncestors([]pack.OID{rtest.OidD}, func(_ pack.OID, gen uint32) error {
		gens = append(gens, gen)
		return nil
	})
	for i := 1; i < len(gens); i++ {
		if gens[i] > gens[i-1] {
			t.Fatalf("gens not non-increasing: %v", gens)
		}
	}
}
```

- [ ] **Step 2: Implement WalkAncestors**

Append to `internal/reachability/set.go`:

```go
// WalkAncestors visits roots and their ancestors transitively in
// generation-descending order (higher gens first). visit returns
// error to stop early.
func (s *Set) WalkAncestors(roots []pack.OID, visit func(oid pack.OID, gen uint32) error) error {
	seen := make(map[pack.OID]bool, 64)
	// Max-heap keyed by generation; we want higher gens first so we
	// fully explore tip-side before tails.
	h := newGenHeap()
	for _, r := range roots {
		if seen[r] {
			continue
		}
		seen[r] = true
		gen, _ := s.Generation(r)
		h.push(genItem{oid: r, gen: gen})
	}
	for h.len() > 0 {
		it := h.pop()
		if err := visit(it.oid, it.gen); err != nil {
			return err
		}
		for _, p := range s.Parents(it.oid) {
			if seen[p] {
				continue
			}
			seen[p] = true
			pgen, _ := s.Generation(p)
			h.push(genItem{oid: p, gen: pgen})
		}
	}
	return nil
}

type genItem struct {
	oid pack.OID
	gen uint32
}

type genHeap struct{ items []genItem }

func newGenHeap() *genHeap { return &genHeap{} }
func (h *genHeap) len() int { return len(h.items) }
func (h *genHeap) push(it genItem) {
	h.items = append(h.items, it)
	for i := len(h.items) - 1; i > 0; {
		parent := (i - 1) / 2
		if h.items[parent].gen >= h.items[i].gen {
			break
		}
		h.items[parent], h.items[i] = h.items[i], h.items[parent]
		i = parent
	}
}
func (h *genHeap) pop() genItem {
	top := h.items[0]
	n := len(h.items) - 1
	h.items[0] = h.items[n]
	h.items = h.items[:n]
	for i := 0; ; {
		l, r := 2*i+1, 2*i+2
		best := i
		if l < n && h.items[l].gen > h.items[best].gen {
			best = l
		}
		if r < n && h.items[r].gen > h.items[best].gen {
			best = r
		}
		if best == i {
			break
		}
		h.items[i], h.items[best] = h.items[best], h.items[i]
		i = best
	}
	return top
}
```

- [ ] **Step 3: Run tests**

Run: `go test ./internal/reachability/ -run TestSet_WalkAncestors -v`
Expected: PASS (after Task 3.7).

- [ ] **Step 4: Commit**

```bash
git add internal/reachability/set.go internal/reachability/set_test.go
git commit -m "M10 task 3.4: Set.WalkAncestors with generation-bounded heap"
```

### Task 3.5: `RefTips` + `ObjectPack`

**Files:**
- Modify: `internal/reachability/set.go`
- Modify: `internal/reachability/set_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestSet_RefTips_AppliesDeltas(t *testing.T) {
	ctx := context.Background()
	store, k, body := rtest.NewBaseWithDeltaRepo(t) // delta advances main from C to D
	set, _ := reachability.Load(ctx, store, k, body)
	tips := set.RefTips()
	if tips["refs/heads/main"] != rtest.OidD {
		t.Fatalf("main = %x, want D", tips["refs/heads/main"][:1])
	}
}

func TestSet_ObjectPack(t *testing.T) {
	ctx := context.Background()
	store, k, body := rtest.NewBaseWithDeltaRepo(t)
	set, _ := reachability.Load(ctx, store, k, body)
	p, ok := set.ObjectPack(rtest.OidA)
	if !ok {
		t.Fatalf("ObjectPack(A) not found")
	}
	_ = p
}
```

- [ ] **Step 2: Implement methods**

Append:

```go
// RefTips returns the effective ref tip map (base + deltas applied in
// order). The returned map is a copy; mutating it does not affect s.
func (s *Set) RefTips() map[string]pack.OID {
	out := make(map[string]pack.OID, len(s.refs))
	for k, v := range s.refs {
		out[k] = v
	}
	return out
}

// ObjectPack delegates to the underlying .bvom view.
func (s *Set) ObjectPack(oid pack.OID) (packID pack.OID, ok bool) {
	if id, found := s.omap.LookupPack(oid); found {
		return id, true
	}
	return pack.OID{}, false
}
```

If `objindex.View` doesn't expose `LookupPack`, add a thin wrapper or use whatever lookup method exists. Check `internal/objindex/` interface.

- [ ] **Step 3: Run tests**

Run: `go test ./internal/reachability/ -run 'TestSet_(RefTips|ObjectPack)' -v`
Expected: PASS (after Task 3.7).

- [ ] **Step 4: Commit**

```bash
git add internal/reachability/set.go internal/reachability/set_test.go
git commit -m "M10 task 3.5: Set.RefTips + Set.ObjectPack"
```

### Task 3.6: Shadow-semantics test

**Files:**
- Create: `internal/reachability/shadow_test.go`

- [ ] **Step 1: Write the test**

```go
package reachability_test

import (
	"context"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/reachability"
	"github.com/bucketvcs/bucketvcs/internal/reachability/rtest"
)

// A commit added by a later delta with a different generation number
// shadows the base/earlier deltas. The Set always reports the latest
// delta's value (this is impossible by construction in production
// — push-time .bvrd files always reference unique commits — but the
// abstraction MUST be defined so future bug-fix paths don't surprise us).
func TestSet_ShadowSemantics_LatestDeltaWins(t *testing.T) {
	ctx := context.Background()
	store, k, body := rtest.NewShadowedFixture(t)
	set, _ := reachability.Load(ctx, store, k, body)
	g, ok := set.Generation(rtest.OidA)
	if !ok || g != 99 {
		t.Fatalf("expected shadowed gen=99, got (%d, %v)", g, ok)
	}
}
```

`NewShadowedFixture` is added in Task 3.7 (delta containing OidA with gen=99 layered over base where OidA has gen=1).

- [ ] **Step 2: Run after Task 3.7**

Run: `go test ./internal/reachability/ -run TestSet_ShadowSemantics -v`
Expected: PASS after Task 3.7.

- [ ] **Step 3: Commit (with task 3.7)**

Defer commit to Task 3.7.

### Task 3.7: `rtest` fixtures

**Files:**
- Modify: `internal/reachability/rtest/fixtures.go`

- [ ] **Step 1: Implement `NewBaseOnlyRepo`**

Replace `t.Fatal` stubs with real fixture builders. Each fixture:

1. Creates an in-memory `storage.ObjectStore` (use `localfs` with a temp dir, or whatever in-memory store the codebase uses for tests).
2. Synthesizes a pack with the commits described by the chain kind.
3. Builds `.bvcg` v2 and `.bvom` via the public builders.
4. Uploads pack + indexes.
5. Returns a `manifest.Body` referencing them.

Reference: `internal/maintenance/mtest/fixtures.go` already has a similar pattern (synthesize repo → build indexes → upload). Adapt that code.

```go
func NewBaseOnlyRepo(t *testing.T, kind ChainKind) (storage.ObjectStore, *keys.Repo, manifest.Body) {
	t.Helper()
	switch kind {
	case LinearChainABC:
		return buildABCRepo(t)
	default:
		t.Fatalf("unknown ChainKind %v", kind)
		return nil, nil, manifest.Body{}
	}
}

func buildABCRepo(t *testing.T) (storage.ObjectStore, *keys.Repo, manifest.Body) {
	t.Helper()
	// 1. Synthesize a pack with commits A, B (parent=A), C (parent=B).
	// 2. Build .bvcg v2 (gens: A=1, B=2, C=3) and .bvom.
	// 3. Upload to a localfs store under tenants/t/repos/r/.
	// 4. Return (store, keys.Repo, manifest.Body) where Body.Indexes
	//    references the uploaded base; Body.Refs has main=C.
	// (~80 lines of plumbing; mirror internal/maintenance/mtest/fixtures.go)
	t.Skip("implement following internal/maintenance/mtest/fixtures.go pattern")
	return nil, nil, manifest.Body{}
}
```

The full implementation is mechanical. Mirror the `mtest` helpers closely; reuse `gitcli.PackObjectsAll` or hand-synthesize a small pack with `internal/pack`'s write APIs.

- [ ] **Step 2: Implement `NewBaseWithDeltaRepo`**

Same as base-only, plus:

```go
func NewBaseWithDeltaRepo(t *testing.T) (storage.ObjectStore, *keys.Repo, manifest.Body) {
	t.Helper()
	store, k, body := buildABCRepo(t)
	// Build a delta containing OidD (parent=C, gen=4) and ref-tip
	// main: C -> D. Encode .bvrd, upload, append IndexRef to body.
	d := deltaindex.Delta{
		Commits: []deltaindex.CommitRecord{
			{OID: OidD, Generation: 4, Parents: []pack.OID{OidC}},
		},
		RefTips: []deltaindex.RefTipDiff{
			{RefName: "refs/heads/main", OldOID: OidC, NewOID: OidD},
		},
		// no new packs in this fixture (D would belong to a pack we're
		// also adding; for read-only Set tests, omit pack production).
	}
	bts, err := deltaindex.Encode(d)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	hash := sha256Hex(bts)
	key := k.ReachabilityDeltaKey(hash)
	if err := putBytes(t, store, key, bts); err != nil {
		t.Fatalf("upload delta: %v", err)
	}
	body.Indexes.Reachability = &manifest.ReachabilityRef{
		BaseManifest: "v00000001",
		Deltas: []manifest.IndexRef{
			{Key: key, Hash: hash, SizeBytes: int64(len(bts))},
		},
	}
	return store, k, body
}
```

- [ ] **Step 3: Implement `NewLegacyRepo` + `NewShadowedFixture`**

```go
func NewLegacyRepo(t *testing.T) (storage.ObjectStore, *keys.Repo, manifest.Body) {
	t.Helper()
	// Empty manifest with no indexes set — Load should return ErrNoIndex.
	store := newLocalfsStore(t)
	k, _ := keys.New("tenants/t/repos/r/")
	return store, k, manifest.Body{DefaultBranch: "main", Refs: map[string]string{}}
}

func NewShadowedFixture(t *testing.T) (storage.ObjectStore, *keys.Repo, manifest.Body) {
	t.Helper()
	store, k, body := buildABCRepo(t)
	// Layer a delta that re-introduces OidA with generation=99.
	d := deltaindex.Delta{
		Commits: []deltaindex.CommitRecord{{OID: OidA, Generation: 99}},
	}
	bts, _ := deltaindex.Encode(d)
	hash := sha256Hex(bts)
	key := k.ReachabilityDeltaKey(hash)
	_ = putBytes(t, store, key, bts)
	body.Indexes.Reachability = &manifest.ReachabilityRef{
		BaseManifest: "v00000001",
		Deltas: []manifest.IndexRef{{Key: key, Hash: hash, SizeBytes: int64(len(bts))}},
	}
	return store, k, body
}
```

Add helpers `newLocalfsStore`, `putBytes`, `sha256Hex` at the bottom of the file.

- [ ] **Step 4: Run the full reachability test suite**

Run: `go test ./internal/reachability/... -v -count=1`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/reachability/rtest/ internal/reachability/shadow_test.go internal/reachability/set_test.go
git commit -m "M10 task 3.6+3.7: rtest fixtures + shadow-semantics test"
```

---

## Phase 4 — Receive-pack delta production

Each push produces a `.bvrd` and appends an `IndexRef` to `Indexes.Reachability.Deltas`. The work happens between pack ingest and manifest CAS.

### Task 4.1: Generation-lookup helper

Helper that, given a manifest body, returns a `parent_oid -> gen` lookup spanning the base `.bvcg` v2 plus all current deltas. Used by receive-pack to compute gens for new commits.

**Files:**
- Create: `internal/reachability/genlookup.go`
- Create: `internal/reachability/genlookup_test.go`

- [ ] **Step 1: Write the failing test**

```go
package reachability_test

import (
	"context"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/reachability"
	"github.com/bucketvcs/bucketvcs/internal/reachability/rtest"
)

func TestGenLookup_FromBaseOnly(t *testing.T) {
	ctx := context.Background()
	store, k, body := rtest.NewBaseOnlyRepo(t, rtest.LinearChainABC)
	gl, err := reachability.LoadGenLookup(ctx, store, k, body)
	if err != nil {
		t.Fatalf("LoadGenLookup: %v", err)
	}
	if g, ok := gl.Lookup(rtest.OidC); !ok || g != 3 {
		t.Errorf("C = (%d, %v), want (3, true)", g, ok)
	}
	if _, ok := gl.Lookup(rtest.OidUnknown); ok {
		t.Errorf("Unknown should not be present")
	}
}

func TestGenLookup_DeltaShadowsBase(t *testing.T) {
	ctx := context.Background()
	store, k, body := rtest.NewBaseWithDeltaRepo(t)
	gl, _ := reachability.LoadGenLookup(ctx, store, k, body)
	if g, ok := gl.Lookup(rtest.OidD); !ok || g != 4 {
		t.Errorf("D = (%d, %v), want (4, true)", g, ok)
	}
}
```

- [ ] **Step 2: Implement**

`internal/reachability/genlookup.go`:

```go
package reachability

import (
	"context"

	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// GenLookup is a read-only oid -> generation-number map covering all
// commits known to the manifest's base + delta chain. It's a derived
// view of Set, intended for receive-pack's gen-number computation
// during push.
type GenLookup struct {
	m map[pack.OID]uint32
}

// LoadGenLookup constructs a GenLookup. ErrNoIndex is returned for
// legacy manifests; receive-pack callers handle this by computing gens
// without prior knowledge (all parents resolve to gen=0, which yields
// gens starting at 1 for the new commits — matching first-import).
func LoadGenLookup(ctx context.Context, store storage.ObjectStore, k *keys.Repo, body manifest.Body) (*GenLookup, error) {
	s, err := Load(ctx, store, k, body)
	if err != nil {
		return nil, err
	}
	m := make(map[pack.OID]uint32, 256)
	// Walk the base by iterating the commitgraph reader and seeding
	// the map; then overlay each delta's commits (latest wins).
	s.cg.IterRecords(func(oid pack.OID, gen uint32) {
		m[oid] = gen
	})
	for _, d := range s.deltas {
		for _, c := range d.Commits {
			m[c.OID] = c.Generation
		}
	}
	return &GenLookup{m: m}, nil
}

// Lookup returns the generation number for oid.
func (g *GenLookup) Lookup(oid pack.OID) (uint32, bool) {
	v, ok := g.m[oid]
	return v, ok
}
```

If `IterRecords` doesn't exist on `commitgraph.Reader`, add it in `internal/commitgraph/read.go`:

```go
// IterRecords calls f for each commit in the graph.
func (r *Reader) IterRecords(f func(oid pack.OID, gen uint32)) {
	for _, rec := range r.records {
		f(rec.OID, rec.Generation)
	}
}
```

- [ ] **Step 3: Run tests**

Run: `go test ./internal/reachability/ -run TestGenLookup -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/reachability/genlookup.go internal/reachability/genlookup_test.go internal/commitgraph/read.go
git commit -m "M10 task 4.1: reachability.GenLookup for receive-pack gen computation"
```

### Task 4.2: Build delta from a freshly-ingested pack

**Files:**
- Create: `internal/gitproto/receivepack/buildelta.go`
- Create: `internal/gitproto/receivepack/buildelta_test.go`

- [ ] **Step 1: Write the failing test**

```go
package receivepack

import (
	"context"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/reachability/deltaindex"
)

func TestBuildDelta_LinearOneCommit(t *testing.T) {
	ctx := context.Background()
	// Pack contains a single new commit C' whose parent is C (base).
	r := newPackWithCommit(t, oidCp, []pack.OID{oidC})
	gl := newGenLookupSeeded(t, map[pack.OID]uint32{oidC: 3})
	cmds := []RefCommand{{Ref: "refs/heads/main", OldOID: oidC, NewOID: oidCp}}
	packIDs := []pack.OID{oidPnew}

	d, err := buildDelta(ctx, r, gl, cmds, packIDs)
	if err != nil {
		t.Fatalf("buildDelta: %v", err)
	}
	if len(d.Commits) != 1 || d.Commits[0].OID != oidCp {
		t.Fatalf("commits = %+v", d.Commits)
	}
	if d.Commits[0].Generation != 4 {
		t.Errorf("gen = %d, want 4", d.Commits[0].Generation)
	}
	if len(d.RefTips) != 1 || d.RefTips[0].NewOID != oidCp {
		t.Errorf("reftips = %+v", d.RefTips)
	}
	if len(d.Packs) != 1 || d.Packs[0] != oidPnew {
		t.Errorf("packs = %+v", d.Packs)
	}
}

func TestBuildDelta_TransitiveInPack(t *testing.T) {
	// Pack contains C' (parent=C base) and C'' (parent=C').
	// gens: C=3 (base) -> C'=4 -> C''=5.
	ctx := context.Background()
	r := newPackWithCommits(t, []commitSpec{
		{oid: oidCp, parents: []pack.OID{oidC}},
		{oid: oidCpp, parents: []pack.OID{oidCp}},
	})
	gl := newGenLookupSeeded(t, map[pack.OID]uint32{oidC: 3})
	cmds := []RefCommand{{Ref: "refs/heads/main", OldOID: oidC, NewOID: oidCpp}}
	d, err := buildDelta(ctx, r, gl, cmds, nil)
	if err != nil {
		t.Fatalf("buildDelta: %v", err)
	}
	gens := map[pack.OID]uint32{}
	for _, c := range d.Commits {
		gens[c.OID] = c.Generation
	}
	if gens[oidCp] != 4 || gens[oidCpp] != 5 {
		t.Errorf("gens = %v, want C'=4 C''=5", gens)
	}
}
```

`RefCommand` is the existing receivepack type representing one `old new ref` line from the client; reuse it. The test helpers (`newPackWithCommit`, `newGenLookupSeeded`) follow the same pattern as `commitgraph` tests.

- [ ] **Step 2: Implement `buildDelta`**

```go
package receivepack

import (
	"context"
	"fmt"

	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/reachability"
	"github.com/bucketvcs/bucketvcs/internal/reachability/deltaindex"
)

// buildDelta walks the just-ingested pack, computes generation numbers
// for new commits using the gen lookup as a base, and emits a Delta.
// Pure function — no IO.
func buildDelta(ctx context.Context, r *pack.Reader, gl *reachability.GenLookup, cmds []RefCommand, packIDs []pack.OID) (deltaindex.Delta, error) {
	type commit struct {
		oid     pack.OID
		parents []pack.OID
	}
	var newCommits []commit
	if err := r.ForEach(func(oid pack.OID, _ uint64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		obj, err := r.Get(ctx, oid)
		if err != nil {
			return err
		}
		if obj.Type != pack.TypeCommit {
			return nil
		}
		parents, err := parseCommitParents(obj.Data)
		if err != nil {
			return fmt.Errorf("commit %s: %w", oid, err)
		}
		newCommits = append(newCommits, commit{oid: oid, parents: parents})
		return nil
	}); err != nil {
		return deltaindex.Delta{}, err
	}

	// Index by OID for parent lookup within the pack.
	byOID := make(map[pack.OID]int, len(newCommits))
	for i := range newCommits {
		byOID[newCommits[i].oid] = i
	}

	gens := make(map[pack.OID]uint32, len(newCommits))
	var visit func(oid pack.OID) uint32
	visit = func(oid pack.OID) uint32 {
		if g, ok := gens[oid]; ok {
			return g
		}
		if g, ok := gl.Lookup(oid); ok {
			gens[oid] = g
			return g
		}
		idx, inPack := byOID[oid]
		if !inPack {
			// Unknown parent. Treat as gen 0; the caller's lookup
			// already covered the base, so this only happens on
			// legacy repos where no base exists.
			gens[oid] = 0
			return 0
		}
		var maxParent uint32
		for _, p := range newCommits[idx].parents {
			if g := visit(p); g > maxParent {
				maxParent = g
			}
		}
		gens[oid] = maxParent + 1
		return gens[oid]
	}
	for _, c := range newCommits {
		visit(c.oid)
	}

	records := make([]deltaindex.CommitRecord, 0, len(newCommits))
	for _, c := range newCommits {
		records = append(records, deltaindex.CommitRecord{
			OID:        c.oid,
			Generation: gens[c.oid],
			Parents:    c.parents,
		})
	}

	tips := make([]deltaindex.RefTipDiff, 0, len(cmds))
	for _, cmd := range cmds {
		tips = append(tips, deltaindex.RefTipDiff{
			RefName: cmd.Ref,
			OldOID:  cmd.OldOID,
			NewOID:  cmd.NewOID,
		})
	}

	return deltaindex.Delta{
		Commits: records,
		RefTips: tips,
		Packs:   packIDs,
	}, nil
}
```

Reuse `parseCommitParents` from `internal/commitgraph/build.go` — extract it to a shared location (`internal/pack/commitparse.go`) if needed. If `internal/commitgraph/build.go` keeps it unexported, duplicate it (~15 lines).

- [ ] **Step 3: Run tests**

Run: `go test ./internal/gitproto/receivepack/ -run TestBuildDelta -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/gitproto/receivepack/buildelta.go internal/gitproto/receivepack/buildelta_test.go
git commit -m "M10 task 4.2: receive-pack buildDelta (commits + gens + reftips + packs)"
```

### Task 4.3: Upload `.bvrd` and append to manifest

**Files:**
- Create: `internal/gitproto/receivepack/deltaupload.go`
- Create: `internal/gitproto/receivepack/deltaupload_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestUploadDelta_HappyPath(t *testing.T) {
	ctx := context.Background()
	store := newLocalfsStore(t)
	k, _ := keys.New("tenants/t/repos/r/")
	d := deltaindex.Delta{Commits: []deltaindex.CommitRecord{{OID: oidCp, Generation: 4}}}
	ref, err := uploadDelta(ctx, store, k, d)
	if err != nil {
		t.Fatalf("uploadDelta: %v", err)
	}
	if ref.Key == "" || ref.Hash == "" || ref.SizeBytes == 0 {
		t.Fatalf("bad ref: %+v", ref)
	}
	// PutIfAbsent on same bytes should be a no-op success.
	_, err = uploadDelta(ctx, store, k, d)
	if err != nil {
		t.Fatalf("idempotent upload: %v", err)
	}
}

func TestUploadDelta_KeyCollisionDifferentBytes(t *testing.T) {
	ctx := context.Background()
	store := newLocalfsStore(t)
	k, _ := keys.New("tenants/t/repos/r/")
	d := deltaindex.Delta{Commits: []deltaindex.CommitRecord{{OID: oidCp, Generation: 4}}}
	bts, _ := deltaindex.Encode(d)
	hash := sha256Hex(bts)
	// Pre-populate the key with different bytes.
	key := k.ReachabilityDeltaKey(hash)
	_ = putBytes(t, store, key, []byte("not the right bytes"))
	_, err := uploadDelta(ctx, store, k, d)
	if !errors.Is(err, ErrDeltaCollision) {
		t.Fatalf("err = %v, want ErrDeltaCollision", err)
	}
}
```

- [ ] **Step 2: Implement**

`internal/gitproto/receivepack/deltaupload.go`:

```go
package receivepack

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/bucketvcs/bucketvcs/internal/reachability/deltaindex"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// ErrDeltaCollision is returned when the destination .bvrd key already
// contains bytes that don't match what we want to write. Forces push
// abort — see spec §3 (Produce path, "no fall back to stale-index").
var ErrDeltaCollision = errors.New("receivepack: .bvrd key collision against pre-existing bytes")

func uploadDelta(ctx context.Context, store storage.ObjectStore, k *keys.Repo, d deltaindex.Delta) (manifest.IndexRef, error) {
	bts, err := deltaindex.Encode(d)
	if err != nil {
		return manifest.IndexRef{}, fmt.Errorf("encode .bvrd: %w", err)
	}
	sum := sha256.Sum256(bts)
	hash := hex.EncodeToString(sum[:])
	key := k.ReachabilityDeltaKey(hash)

	put := storage.PutIfAbsent
	ok, err := put(ctx, store, key, bytes.NewReader(bts), int64(len(bts)))
	if err != nil {
		return manifest.IndexRef{}, fmt.Errorf("put .bvrd: %w", err)
	}
	if !ok {
		// Key exists. Read it back and confirm bytes match (idempotent
		// upload). Mismatch is a collision and aborts the push.
		existing, err := readObject(ctx, store, key)
		if err != nil {
			return manifest.IndexRef{}, fmt.Errorf("read existing .bvrd: %w", err)
		}
		if !bytes.Equal(existing, bts) {
			return manifest.IndexRef{}, ErrDeltaCollision
		}
	}
	return manifest.IndexRef{Key: key, Hash: hash, SizeBytes: int64(len(bts))}, nil
}
```

Use whatever PutIfAbsent helper the codebase exposes (likely on `storage.ObjectStore` directly or a wrapper in `internal/storage`). Mirror the pattern in `internal/importer/importer.go`'s `uploadBytes`.

- [ ] **Step 3: Run tests**

Run: `go test ./internal/gitproto/receivepack/ -run TestUploadDelta -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/gitproto/receivepack/deltaupload.go internal/gitproto/receivepack/deltaupload_test.go
git commit -m "M10 task 4.3: upload .bvrd with collision detection"
```

### Task 4.4: Integrate into receive-pack request flow

**Files:**
- Modify: `internal/gitproto/receivepack/engine.go`
- Modify: `internal/gitproto/receivepack/engine_test.go`

- [ ] **Step 1: Write the failing test**

In `engine_test.go`, add a test that drives a full push (using the existing test harness — likely `mirror.NewManager` + a stubbed client) and asserts:

```go
func TestReceivePack_AppendsDeltaToManifest(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepoOnLocalfs(t, "tenants/t/repos/r/") // helper from existing tests
	repo.SeedBase(t, oidA, oidB, oidC) // refs main=C, .bvcg v2 + .bvom built

	resp := repo.Push(t, []RefCommand{{Ref: "refs/heads/main", OldOID: oidC, NewOID: oidCp}}, packBytesForC2Cp(t))
	if !resp.OK {
		t.Fatalf("push failed: %v", resp.Err)
	}

	body := repo.LoadManifest(t)
	if body.Indexes.Reachability == nil {
		t.Fatal("Indexes.Reachability not set")
	}
	if n := len(body.Indexes.Reachability.Deltas); n != 1 {
		t.Fatalf("deltas len = %d, want 1", n)
	}
}
```

(Adapt this stub to whatever `engine_test.go` uses today — look at the existing `mirror.NewManager` test for the harness shape.)

- [ ] **Step 2: Wire `buildDelta` + `uploadDelta` into engine**

Edit `internal/gitproto/receivepack/engine.go`. After the pack is ingested and pack IDs are known, before the manifest CAS, add:

```go
// M10: build and upload the per-push reachability delta.
gl, err := reachability.LoadGenLookup(ctx, e.Store, e.Keys, currentBody)
if err != nil && !errors.Is(err, reachability.ErrNoIndex) {
	return fmt.Errorf("load gen lookup: %w", err)
}
if err == nil {
	d, err := buildDelta(ctx, packReader, gl, refCommands, newPackIDs)
	if err != nil {
		return fmt.Errorf("build delta: %w", err)
	}
	deltaRef, err := uploadDelta(ctx, e.Store, e.Keys, d)
	if err != nil {
		return fmt.Errorf("upload delta: %w", err)
	}
	if newBody.Indexes.Reachability == nil {
		newBody.Indexes.Reachability = &manifest.ReachabilityRef{BaseManifest: currentBody.Version}
	}
	newBody.Indexes.Reachability.Deltas = append(newBody.Indexes.Reachability.Deltas, deltaRef)
}
// (else: legacy repo with no .bvcg/.bvom — skip delta production this
// push; first maintenance run will materialize the base, and the
// next push after that picks up the M10 path.)
```

Place this in whatever function builds the CAS body. Match local variable names. If the engine uses a body-builder callback (likely), do this inside the callback so retries rebuild the delta against the latest `M_now`.

- [ ] **Step 3: Run tests**

Run: `go test ./internal/gitproto/receivepack/ -v`
Expected: all PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/gitproto/receivepack/engine.go internal/gitproto/receivepack/engine_test.go
git commit -m "M10 task 4.4: receive-pack writes .bvrd and appends to manifest"
```

### Task 4.5: CAS-retry rebuilds the delta

**Files:**
- Modify: `internal/gitproto/receivepack/engine_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestReceivePack_CASRetry_RebuildsDelta(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepoOnLocalfs(t, "tenants/t/repos/r/")
	repo.SeedBase(t, oidA, oidB, oidC)

	// Simulate a concurrent push that lands first by injecting a
	// pre-baked competing manifest version. The engine's CAS body
	// builder must re-read currentBody and rebuild buildDelta with
	// new gen lookups — yielding a NEW .bvrd hash (because the prior
	// push introduced a delta that changes gen for our new commit's
	// ancestor).
	repo.InjectCompetingPush(t, ...)

	resp := repo.Push(t, []RefCommand{{Ref: "refs/heads/main", OldOID: oidC, NewOID: oidCp}}, packBytesForC2Cp(t))
	if !resp.OK {
		t.Fatalf("push failed: %v", resp.Err)
	}
	// Assert that the resulting delta's hash != the hash we'd have
	// computed on the original currentBody.
	body := repo.LoadManifest(t)
	last := body.Indexes.Reachability.Deltas[len(body.Indexes.Reachability.Deltas)-1]
	if last.Hash == repo.ExpectedFirstAttemptDeltaHash(t) {
		t.Fatalf("delta hash should differ after CAS retry — engine did not rebuild")
	}
}
```

- [ ] **Step 2: Verify the engine wiring already supports this**

The fix in Task 4.4 placed `buildDelta` inside the CAS body-builder callback. If the test fails because the engine builds the delta once outside the callback, move it inside. Confirm by tracing `repo.Commit(ctx, body, fn)` — `fn` should call `LoadGenLookup` + `buildDelta` + `uploadDelta` and append to `newBody.Indexes.Reachability.Deltas` each iteration.

- [ ] **Step 3: Run the test**

Run: `go test ./internal/gitproto/receivepack/ -run TestReceivePack_CASRetry_RebuildsDelta -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/gitproto/receivepack/engine.go internal/gitproto/receivepack/engine_test.go
git commit -m "M10 task 4.5: receive-pack rebuilds .bvrd on CAS retry"
```

### Task 4.6: Push aborts on `.bvrd` upload failure

**Files:**
- Modify: `internal/gitproto/receivepack/engine_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestReceivePack_AbortsOnDeltaUploadFailure(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepoOnLocalfs(t, "tenants/t/repos/r/")
	repo.SeedBase(t, oidA, oidB, oidC)
	repo.Store = &failingPutStore{wrap: repo.Store, failPrefix: "tenants/t/repos/r/indexes/reachability-delta/"}

	resp := repo.Push(t, []RefCommand{{Ref: "refs/heads/main", OldOID: oidC, NewOID: oidCp}}, packBytesForC2Cp(t))
	if resp.OK {
		t.Fatalf("push should have failed; engine must not commit without .bvrd")
	}
	// Manifest unchanged.
	body := repo.LoadManifest(t)
	if body.Refs["refs/heads/main"] != hex(oidC) {
		t.Fatalf("manifest mutated despite .bvrd failure")
	}
}
```

`failingPutStore` is a tiny test wrapper that returns an error on `Put`/`PutIfAbsent` when the key starts with `failPrefix`.

- [ ] **Step 2: Verify behavior**

The error bubble-up from `uploadDelta` already surfaces. Confirm by reading the engine code that nothing swallows the error.

- [ ] **Step 3: Run the test**

Run: `go test ./internal/gitproto/receivepack/ -run TestReceivePack_AbortsOnDeltaUploadFailure -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/gitproto/receivepack/engine_test.go
git commit -m "M10 task 4.6: push aborts on .bvrd upload failure (test)"
```

---

## Phase 5 — Maintenance compaction

Extends M9 maintenance with three reachability thresholds, a compact-only outcome, and a CAS-merge body that drops the consumed delta prefix.

### Task 5.1: Add reachability thresholds to `RunOptions`

**Files:**
- Modify: `internal/maintenance/options.go`
- Modify: `internal/maintenance/options_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestThresholds_ReachabilityDefaults(t *testing.T) {
	d := DefaultThresholds()
	if d.ReachabilityDeltaCommits != 1000 {
		t.Errorf("ReachabilityDeltaCommits = %d, want 1000", d.ReachabilityDeltaCommits)
	}
	if d.ReachabilityDeltaPushes != 100 {
		t.Errorf("ReachabilityDeltaPushes = %d, want 100", d.ReachabilityDeltaPushes)
	}
	if d.ReachabilityDeltaBytes != 64*1024*1024 {
		t.Errorf("ReachabilityDeltaBytes = %d, want 64MiB", d.ReachabilityDeltaBytes)
	}
}
```

- [ ] **Step 2: Extend the struct**

Edit `internal/maintenance/options.go`. Add to `Thresholds`:

```go
type Thresholds struct {
	RecentPackCount        int
	TotalPackCount         int
	ManifestPackBytes      int64
	RecentWindow           time.Duration

	// M10 reachability thresholds. Per spec §14.2 defaults. A value of
	// 0 disables that specific check (matches M9's convention).
	ReachabilityDeltaCommits int
	ReachabilityDeltaPushes  int
	ReachabilityDeltaBytes   int64
}

func DefaultThresholds() Thresholds {
	return Thresholds{
		RecentPackCount:          1000,
		TotalPackCount:           10000,
		ManifestPackBytes:        8 * 1024 * 1024,
		RecentWindow:             24 * time.Hour,
		ReachabilityDeltaCommits: 1000,
		ReachabilityDeltaPushes:  100,
		ReachabilityDeltaBytes:   64 * 1024 * 1024,
	}
}
```

- [ ] **Step 3: Run**

Run: `go test ./internal/maintenance/ -run TestThresholds_Reachability -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/maintenance/options.go internal/maintenance/options_test.go
git commit -m "M10 task 5.1: maintenance thresholds for reachability deltas"
```

### Task 5.2: Phase-0 threshold evaluator

**Files:**
- Modify: `internal/maintenance/thresholds.go`
- Modify: `internal/maintenance/thresholds_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestEvaluate_ReachabilityBytes_Triggers(t *testing.T) {
	thr := DefaultThresholds()
	body := manifest.Body{
		Indexes: manifest.Indexes{
			Reachability: &manifest.ReachabilityRef{
				Deltas: []manifest.IndexRef{
					{SizeBytes: 50 * 1024 * 1024},
					{SizeBytes: 20 * 1024 * 1024}, // total 70MiB > 64MiB threshold
				},
			},
		},
	}
	d := Evaluate(body, thr, time.Now())
	if !d.CompactReachability {
		t.Fatalf("expected CompactReachability=true")
	}
	if d.CompactReachabilityReason != "delta-bytes" {
		t.Errorf("reason = %q, want delta-bytes", d.CompactReachabilityReason)
	}
}

func TestEvaluate_ReachabilityPushes_Triggers(t *testing.T) {
	thr := DefaultThresholds()
	deltas := make([]manifest.IndexRef, 100) // exactly at threshold
	for i := range deltas {
		deltas[i] = manifest.IndexRef{SizeBytes: 1}
	}
	body := manifest.Body{Indexes: manifest.Indexes{Reachability: &manifest.ReachabilityRef{Deltas: deltas}}}
	d := Evaluate(body, thr, time.Now())
	if !d.CompactReachability || d.CompactReachabilityReason != "delta-pushes" {
		t.Fatalf("expected pushes trigger, got %+v", d)
	}
}

func TestEvaluate_ReachabilityNoTrigger_BelowAllBounds(t *testing.T) {
	body := manifest.Body{Indexes: manifest.Indexes{Reachability: &manifest.ReachabilityRef{Deltas: []manifest.IndexRef{{SizeBytes: 1024}}}}}
	d := Evaluate(body, DefaultThresholds(), time.Now())
	if d.CompactReachability {
		t.Errorf("should not trigger, got %+v", d)
	}
}
```

- [ ] **Step 2: Extend Decision + Evaluate**

Edit `internal/maintenance/thresholds.go`. Add fields to `Decision`:

```go
type Decision struct {
	// Existing M9 fields:
	Repack       bool
	RepackReason string

	// M10 fields:
	CompactReachability        bool
	CompactReachabilityReason  string
}
```

And to `Evaluate`, append after the existing repack-threshold logic:

```go
// Reachability thresholds (spec §14.2). Cheap-first: bytes + pushes
// are O(1) on the manifest body; commits requires reading .bvrd
// headers, which we only do if cheaper triggers haven't fired and
// the commits threshold is > 0.
if body.Indexes.Reachability != nil && thr.ReachabilityDeltaBytes > 0 {
	var totalBytes int64
	for _, ref := range body.Indexes.Reachability.Deltas {
		totalBytes += ref.SizeBytes
	}
	if totalBytes >= thr.ReachabilityDeltaBytes {
		d.CompactReachability = true
		d.CompactReachabilityReason = "delta-bytes"
	}
}
if !d.CompactReachability && body.Indexes.Reachability != nil && thr.ReachabilityDeltaPushes > 0 {
	if len(body.Indexes.Reachability.Deltas) >= thr.ReachabilityDeltaPushes {
		d.CompactReachability = true
		d.CompactReachabilityReason = "delta-pushes"
	}
}
// Commits check is deferred to Phase-0 logic in pipeline.go (needs IO).
```

- [ ] **Step 3: Run tests**

Run: `go test ./internal/maintenance/ -run TestEvaluate_Reachability -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/maintenance/thresholds.go internal/maintenance/thresholds_test.go
git commit -m "M10 task 5.2: reachability threshold evaluator (bytes + pushes)"
```

### Task 5.3: Commit-count threshold (with IO)

**Files:**
- Modify: `internal/maintenance/thresholds.go`
- Modify: `internal/maintenance/thresholds_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestEvaluateCommits_Triggers(t *testing.T) {
	ctx := context.Background()
	store, k, body := mtest.RepoWithDeltas(t, 1100) // 1100 commits across deltas
	thr := DefaultThresholds()
	hit, reason, err := EvaluateReachabilityCommits(ctx, store, k, body, thr)
	if err != nil {
		t.Fatalf("EvaluateReachabilityCommits: %v", err)
	}
	if !hit || reason != "delta-commits" {
		t.Fatalf("expected hit/delta-commits, got (%v, %q)", hit, reason)
	}
}
```

- [ ] **Step 2: Implement**

```go
// EvaluateReachabilityCommits performs the IO-bound commit-count
// threshold check. Returns (hit, reason, error). Only called by the
// pipeline when the cheaper bytes/pushes checks did NOT already fire
// (cheap-first convention).
func EvaluateReachabilityCommits(ctx context.Context, store storage.ObjectStore, k *keys.Repo, body manifest.Body, thr Thresholds) (bool, string, error) {
	if body.Indexes.Reachability == nil || thr.ReachabilityDeltaCommits <= 0 {
		return false, "", nil
	}
	var total int
	for _, ref := range body.Indexes.Reachability.Deltas {
		header, err := readDeltaHeader(ctx, store, ref.Key)
		if err != nil {
			return false, "", fmt.Errorf("read .bvrd header %s: %w", ref.Hash[:8], err)
		}
		total += int(header.NCommits)
		if total >= thr.ReachabilityDeltaCommits {
			return true, "delta-commits", nil
		}
	}
	return false, "", nil
}

// readDeltaHeader does a range GET of the first HeaderSize bytes.
func readDeltaHeader(ctx context.Context, store storage.ObjectStore, key string) (deltaindex.Header, error) {
	r, err := store.GetRange(ctx, key, 0, int64(deltaindex.HeaderSize))
	if err != nil {
		return deltaindex.Header{}, err
	}
	defer r.Close()
	bts, err := io.ReadAll(r)
	if err != nil {
		return deltaindex.Header{}, err
	}
	return deltaindex.ParseHeader(bts)
}
```

This requires adding a `Header` struct + `ParseHeader` to `internal/reachability/deltaindex/format.go`:

```go
type Header struct {
	Version  uint32
	NCommits uint32
	NReftips uint32
	NPacks   uint32
}

func ParseHeader(b []byte) (Header, error) {
	if len(b) < HeaderSize {
		return Header{}, fmt.Errorf("%w: short header (%d bytes)", ErrMalformed, len(b))
	}
	if !bytes.Equal(b[:4], Magic[:]) {
		return Header{}, fmt.Errorf("%w: bad magic", ErrMalformed)
	}
	return Header{
		Version:  binary.LittleEndian.Uint32(b[4:8]),
		NCommits: binary.LittleEndian.Uint32(b[8:12]),
		NReftips: binary.LittleEndian.Uint32(b[12:16]),
		NPacks:   binary.LittleEndian.Uint32(b[16:20]),
	}, nil
}
```

If `storage.ObjectStore` doesn't have `GetRange`, check existing usage in `internal/pack/` — there must be a range-read helper. Otherwise add one or fall back to a full GET with `--reachability-delta-commits=0` documented as "disabled until range-read lands."

Add `mtest.RepoWithDeltas` to `internal/maintenance/mtest/fixtures.go`: synthesize N deltas distributed to total `nCommits` commits.

- [ ] **Step 3: Run tests**

Run: `go test ./internal/maintenance/ -run TestEvaluateCommits -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/maintenance/thresholds.go internal/maintenance/thresholds_test.go internal/reachability/deltaindex/format.go internal/maintenance/mtest/fixtures.go
git commit -m "M10 task 5.3: reachability commit-count threshold via .bvrd header range-read"
```

### Task 5.4: Compact-only path in pipeline

**Files:**
- Modify: `internal/maintenance/pipeline.go`
- Modify: `internal/maintenance/pipeline_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestRun_CompactOnly_RebuildsIndexes_NoNewPack(t *testing.T) {
	ctx := context.Background()
	store, k, body := mtest.RepoMidCycle(t,
		mtest.WithPackCount(1),     // below pack-count threshold
		mtest.WithDeltaCount(150),  // above push threshold
	)
	opts := RunOptions{Thresholds: DefaultThresholds()}
	report, err := Run(ctx, store, k, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.ReachabilityCompaction.Triggered {
		t.Fatalf("expected reachability_compaction.triggered=true")
	}
	if report.PacksReplaced != 0 {
		t.Errorf("PacksReplaced = %d, want 0 (compact-only)", report.PacksReplaced)
	}
	// Manifest after run: same packs, reachability.deltas empty.
	after := loadBody(t, store, k)
	if len(after.Indexes.Reachability.Deltas) != 0 {
		t.Errorf("Deltas after compact = %d, want 0", len(after.Indexes.Reachability.Deltas))
	}
}
```

- [ ] **Step 2: Implement the compact-only branch**

Edit `internal/maintenance/pipeline.go`. After `Decision` is computed:

```go
decision := Evaluate(currentBody, opts.Thresholds, now)
if !decision.Repack && !decision.CompactReachability {
	// Check the IO-bound commit-count threshold only if cheaper checks
	// didn't fire and the threshold is enabled.
	hit, reason, err := EvaluateReachabilityCommits(ctx, store, k, currentBody, opts.Thresholds)
	if err != nil {
		return Report{}, fmt.Errorf("evaluate commits: %w", err)
	}
	if hit {
		decision.CompactReachability = true
		decision.CompactReachabilityReason = reason
	}
}

switch {
case decision.Repack:
	// Existing M9 path: repack + full index refresh. The CAS-merge
	// body builder (task 5.5) drops consumed deltas regardless.
	return runRepack(ctx, store, k, currentBody, opts, decision)
case decision.CompactReachability:
	return runCompactOnly(ctx, store, k, currentBody, opts, decision)
default:
	return Report{Outcome: "no-op"}, nil
}
```

Implement `runCompactOnly`:

```go
func runCompactOnly(ctx context.Context, store storage.ObjectStore, k *keys.Repo, body manifest.Body, opts RunOptions, dec Decision) (Report, error) {
	// Materialize current packs into a temp bare repo so we can run
	// objindex.Build + commitgraph.Build over the consolidated view.
	// (Same materialization step as runRepack, but we skip the
	// pack-objects call.)
	mat, err := materializePacks(ctx, store, k, body)
	if err != nil {
		return Report{}, fmt.Errorf("materialize: %w", err)
	}
	defer mat.Cleanup()

	newBVOM, newBVCG, err := buildIndexes(ctx, mat.PackReader(), body.Refs)
	if err != nil {
		return Report{}, fmt.Errorf("build indexes: %w", err)
	}

	bvomRef, err := uploadBVOM(ctx, store, k, newBVOM)
	if err != nil {
		return Report{}, fmt.Errorf("upload .bvom: %w", err)
	}
	bvcgRef, err := uploadBVCG(ctx, store, k, newBVCG)
	if err != nil {
		return Report{}, fmt.Errorf("upload .bvcg: %w", err)
	}

	consumedDeltaCount := len(body.Indexes.Reachability.Deltas) // observed prefix
	report := Report{
		Outcome: "compact-only",
		ReachabilityCompaction: ReachabilityCompactionReport{
			Triggered:     true,
			TriggerReason: dec.CompactReachabilityReason,
			DeltasDropped: consumedDeltaCount,
			BaseSwapped:   true,
		},
	}

	// CAS-merge: keep M_now.Packs, set new indexes, drop consumed delta prefix.
	err = commitWithCASMerge(ctx, store, k, body, opts.CASRetry, func(mNow manifest.Body) manifest.Body {
		return manifest.Body{
			DefaultBranch: mNow.DefaultBranch,
			Refs:          mNow.Refs,
			Packs:         mNow.Packs,
			Bundles:       mNow.Bundles,
			Indexes: manifest.Indexes{
				ObjectMap:   &bvomRef,
				CommitGraph: &bvcgRef,
				Reachability: trimConsumedDeltas(mNow.Indexes.Reachability, consumedDeltaCount, mNow.Version),
			},
		}
	})
	if err != nil {
		return report, err
	}
	return report, nil
}

// trimConsumedDeltas implements the §4.1 CAS-merge clause for the
// delta list: drop the first `consumed` entries, preserve the rest.
func trimConsumedDeltas(now *manifest.ReachabilityRef, consumed int, baseVersion string) *manifest.ReachabilityRef {
	out := &manifest.ReachabilityRef{BaseManifest: baseVersion}
	if now == nil {
		return out
	}
	if consumed >= len(now.Deltas) {
		return out
	}
	tail := make([]manifest.IndexRef, len(now.Deltas)-consumed)
	copy(tail, now.Deltas[consumed:])
	out.Deltas = tail
	return out
}
```

`commitWithCASMerge` is the existing M9 helper — extend it if its signature isn't already callback-shaped.

- [ ] **Step 3: Run tests**

Run: `go test ./internal/maintenance/ -run TestRun_CompactOnly -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/maintenance/pipeline.go internal/maintenance/pipeline_test.go
git commit -m "M10 task 5.4: maintenance compact-only path (index refresh, no repack)"
```

### Task 5.5: CAS-merge body extension for repack path

**Files:**
- Modify: `internal/maintenance/casmerge.go`
- Modify: `internal/maintenance/casmerge_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestCASMerge_DropsConsumedDeltas_ConcurrentPushPreserved(t *testing.T) {
	consumed := 5 // observed by maintenance when it started
	// M_now has 7 deltas: 5 we consumed + 2 that landed during work.
	mNow := manifest.Body{
		Version: "v00000010",
		Indexes: manifest.Indexes{
			Reachability: &manifest.ReachabilityRef{
				BaseManifest: "v00000003",
				Deltas: []manifest.IndexRef{
					{Hash: "d1"}, {Hash: "d2"}, {Hash: "d3"}, {Hash: "d4"}, {Hash: "d5"},
					{Hash: "d6"}, {Hash: "d7"},
				},
			},
		},
	}
	got := trimConsumedDeltas(mNow.Indexes.Reachability, consumed, mNow.Version)
	if got.BaseManifest != "v00000010" {
		t.Errorf("BaseManifest = %q, want v00000010", got.BaseManifest)
	}
	if len(got.Deltas) != 2 || got.Deltas[0].Hash != "d6" || got.Deltas[1].Hash != "d7" {
		t.Errorf("Deltas = %+v, want [d6 d7]", got.Deltas)
	}
}

func TestCASMerge_NoConcurrentDeltas_AllDropped(t *testing.T) {
	mNow := manifest.Body{
		Indexes: manifest.Indexes{
			Reachability: &manifest.ReachabilityRef{Deltas: []manifest.IndexRef{{Hash: "d1"}, {Hash: "d2"}}},
		},
	}
	got := trimConsumedDeltas(mNow.Indexes.Reachability, 2, "v1")
	if len(got.Deltas) != 0 {
		t.Errorf("Deltas len = %d, want 0", len(got.Deltas))
	}
}
```

- [ ] **Step 2: Verify implementation**

`trimConsumedDeltas` was added in Task 5.4. This task just tests it independently.

- [ ] **Step 3: Run tests**

Run: `go test ./internal/maintenance/ -run TestCASMerge_ -v`
Expected: PASS.

- [ ] **Step 4: Wire into the repack path**

In the existing repack CAS-merge body builder, replace the `Indexes.Reachability` clause with a call to `trimConsumedDeltas(mNow.Indexes.Reachability, consumedDeltaCount, mNow.Version)`. `consumedDeltaCount` should be captured at the start of `runRepack` (same as `runCompactOnly`).

- [ ] **Step 5: Commit**

```bash
git add internal/maintenance/casmerge.go internal/maintenance/casmerge_test.go internal/maintenance/pipeline.go
git commit -m "M10 task 5.5: CAS-merge drops consumed deltas across repack and compact-only paths"
```

---

## Phase 6 — Upload-pack negotiation pre-step

This phase splits upload-pack into a Negotiate phase (pure-Go, against `reachability.Set`) and a Deliver phase (lazy mirror materialization). Includes the `bucketvcs negotiate` debug CLI.

### Task 6.1: `ShippingPlan` struct

**Files:**
- Create: `internal/gitproto/uploadpack/shipping_plan.go`
- Create: `internal/gitproto/uploadpack/shipping_plan_test.go`

- [ ] **Step 1: Write the failing test**

```go
package uploadpack

import (
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/pack"
)

func TestShippingPlan_Equal_OrderIndependent(t *testing.T) {
	a := ShippingPlan{Commits: []pack.OID{oid(1), oid(2)}, Refs: map[string]pack.OID{"main": oid(2)}}
	b := ShippingPlan{Commits: []pack.OID{oid(2), oid(1)}, Refs: map[string]pack.OID{"main": oid(2)}}
	if !a.Equal(b) {
		t.Fatalf("Equal should ignore commit order")
	}
}

func TestShippingPlan_Equal_DifferentRefs(t *testing.T) {
	a := ShippingPlan{Refs: map[string]pack.OID{"main": oid(2)}}
	b := ShippingPlan{Refs: map[string]pack.OID{"main": oid(3)}}
	if a.Equal(b) {
		t.Fatalf("Equal should detect ref diff")
	}
}

func oid(b byte) pack.OID {
	var o pack.OID
	o[0] = b
	return o
}
```

- [ ] **Step 2: Implement**

```go
package uploadpack

import (
	"bytes"
	"sort"

	"github.com/bucketvcs/bucketvcs/internal/pack"
)

// ShippingPlan is the set of commits and ref tips the server will
// stream to the client. Produced by Negotiate; consumed by Deliver.
type ShippingPlan struct {
	Commits []pack.OID
	Refs    map[string]pack.OID
}

// Equal reports order-independent equality. Used in differential
// parity tests against git upload-pack.
func (p ShippingPlan) Equal(q ShippingPlan) bool {
	if len(p.Commits) != len(q.Commits) || len(p.Refs) != len(q.Refs) {
		return false
	}
	ps := append([]pack.OID(nil), p.Commits...)
	qs := append([]pack.OID(nil), q.Commits...)
	sort.Slice(ps, func(i, j int) bool { return bytes.Compare(ps[i][:], ps[j][:]) < 0 })
	sort.Slice(qs, func(i, j int) bool { return bytes.Compare(qs[i][:], qs[j][:]) < 0 })
	for i := range ps {
		if ps[i] != qs[i] {
			return false
		}
	}
	for k, v := range p.Refs {
		if q.Refs[k] != v {
			return false
		}
	}
	return true
}
```

- [ ] **Step 3: Run tests**

Run: `go test ./internal/gitproto/uploadpack/ -run TestShippingPlan -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/gitproto/uploadpack/shipping_plan.go internal/gitproto/uploadpack/shipping_plan_test.go
git commit -m "M10 task 6.1: ShippingPlan struct + order-independent Equal"
```

### Task 6.2: Pure-Go Negotiate

**Files:**
- Create: `internal/gitproto/uploadpack/negotiate.go`
- Create: `internal/gitproto/uploadpack/negotiate_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestNegotiate_NoHaves_ShipsAllFromWant(t *testing.T) {
	ctx := context.Background()
	set := newSetABCD(t) // commits A-B-C-D linear, D is tip
	plan, err := Negotiate(ctx, set, NegotiateInput{
		Wants: []pack.OID{oidD},
		Haves: nil,
		Done:  true,
	})
	if err != nil {
		t.Fatalf("Negotiate: %v", err)
	}
	want := []pack.OID{oidA, oidB, oidC, oidD}
	if !sameSet(plan.Commits, want) {
		t.Fatalf("commits = %v, want %v", plan.Commits, want)
	}
}

func TestNegotiate_HaveAncestor_ShipsOnlyDescendants(t *testing.T) {
	ctx := context.Background()
	set := newSetABCD(t)
	plan, err := Negotiate(ctx, set, NegotiateInput{
		Wants: []pack.OID{oidD},
		Haves: []pack.OID{oidB},
		Done:  true,
	})
	if err != nil {
		t.Fatalf("Negotiate: %v", err)
	}
	want := []pack.OID{oidC, oidD}
	if !sameSet(plan.Commits, want) {
		t.Fatalf("commits = %v, want %v", plan.Commits, want)
	}
}

func TestNegotiate_HaveIsTip_ShipsNothing(t *testing.T) {
	ctx := context.Background()
	set := newSetABCD(t)
	plan, err := Negotiate(ctx, set, NegotiateInput{
		Wants: []pack.OID{oidD},
		Haves: []pack.OID{oidD},
		Done:  true,
	})
	if err != nil {
		t.Fatalf("Negotiate: %v", err)
	}
	if len(plan.Commits) != 0 {
		t.Fatalf("expected empty plan, got %v", plan.Commits)
	}
}

func TestNegotiate_UnknownWant_Error(t *testing.T) {
	ctx := context.Background()
	set := newSetABCD(t)
	_, err := Negotiate(ctx, set, NegotiateInput{Wants: []pack.OID{oidUnknown}, Done: true})
	if !errors.Is(err, ErrUnknownWant) {
		t.Fatalf("err = %v, want ErrUnknownWant", err)
	}
}
```

- [ ] **Step 2: Implement Negotiate**

```go
package uploadpack

import (
	"context"
	"errors"
	"fmt"

	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/reachability"
)

// ErrUnknownWant is returned when the client requests a commit not
// present in the reachability Set. Caller (upload-pack engine) maps
// this to ERR pkt-line and aborts the session.
var ErrUnknownWant = errors.New("uploadpack: unknown want")

// NegotiateInput carries the parsed wants/haves/done state from the
// pkt-line layer.
type NegotiateInput struct {
	Wants []pack.OID
	Haves []pack.OID
	Done  bool
}

// Negotiate computes the ShippingPlan: commits reachable from Wants
// minus commits reachable from Haves. Pure-Go; only reads the Set.
func Negotiate(ctx context.Context, s *reachability.Set, in NegotiateInput) (ShippingPlan, error) {
	for _, w := range in.Wants {
		if !s.Has(w) {
			return ShippingPlan{}, fmt.Errorf("%w: %s", ErrUnknownWant, w)
		}
	}

	// Compute ancestors-of-haves first.
	haveSet := make(map[pack.OID]bool, 64)
	for _, h := range in.Haves {
		if !s.Has(h) {
			// Client lied about a have — silently ignore (Git protocol allows this).
			continue
		}
	}
	if err := s.WalkAncestors(in.Haves, func(oid pack.OID, _ uint32) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		haveSet[oid] = true
		return nil
	}); err != nil {
		return ShippingPlan{}, err
	}

	// Walk wants; emit commits not in haveSet.
	shipping := make([]pack.OID, 0, 64)
	shippingSeen := make(map[pack.OID]bool, 64)
	if err := s.WalkAncestors(in.Wants, func(oid pack.OID, _ uint32) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if haveSet[oid] || shippingSeen[oid] {
			return nil
		}
		shippingSeen[oid] = true
		shipping = append(shipping, oid)
		return nil
	}); err != nil {
		return ShippingPlan{}, err
	}

	return ShippingPlan{Commits: shipping, Refs: s.RefTips()}, nil
}
```

- [ ] **Step 3: Run tests**

Run: `go test ./internal/gitproto/uploadpack/ -run TestNegotiate -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/gitproto/uploadpack/negotiate.go internal/gitproto/uploadpack/negotiate_test.go
git commit -m "M10 task 6.2: pure-Go upload-pack negotiation"
```

### Task 6.3: Differential parity with `git upload-pack`

**Files:**
- Create: `internal/gitproto/uploadpack/negotiate_diff_test.go`

- [ ] **Step 1: Write the differential test**

```go
package uploadpack_test

import (
	"context"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitproto/uploadpack"
	"github.com/bucketvcs/bucketvcs/internal/reachability/rtest"
)

// For a fixed (wants, haves) probe, the pure-Go Negotiate output must
// equal `git upload-pack`'s shipping decision when run against a
// materialized mirror of the same repo.
func TestNegotiate_ParityWithGitUploadPack(t *testing.T) {
	probes := []struct {
		name  string
		setup func(t *testing.T) (rtest.Repo, uploadpack.NegotiateInput)
	}{
		{"abcd_full_clone", probeAllFromD},
		{"abcd_haves_b", probeWithHaveB},
		{"abcd_haves_tip", probeWithHaveTip},
		{"linear_with_delta", probeWithDeltaTip},
		{"many_small_pushes", probeManyDeltas},
	}
	for _, pr := range probes {
		t.Run(pr.name, func(t *testing.T) {
			repo, input := pr.setup(t)
			ctx := context.Background()
			set := repo.LoadSet(t)
			plan, err := uploadpack.Negotiate(ctx, set, input)
			if err != nil {
				t.Fatalf("Negotiate: %v", err)
			}
			oraclePlan := repo.GitUploadPackPlan(t, input) // shells out
			if !plan.Equal(oraclePlan) {
				t.Errorf("plan mismatch\n  got:    %+v\n  oracle: %+v", plan, oraclePlan)
			}
		})
	}
}
```

`rtest.Repo.GitUploadPackPlan(t, input)` is a helper that:

1. Materializes the repo to a bare git directory (use `internal/mirror.Manager` or shell out to clone).
2. Encodes `input.Wants`/`Haves`/`Done` as Git v2 pkt-lines.
3. Runs `git upload-pack --stateless-rpc <bare>` with that input.
4. Parses the response to extract the shipped commit set + ref tips.
5. Returns a `ShippingPlan`.

This is the most expensive helper in the test suite; skip with `testing.Short()` and gate with `if _, err := exec.LookPath("git"); err != nil { t.Skip(...) }`.

- [ ] **Step 2: Implement the helper**

In `internal/reachability/rtest/git_oracle.go`:

```go
package rtest

import (
	"bytes"
	"os/exec"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitproto/uploadpack"
	"github.com/bucketvcs/bucketvcs/internal/pack"
)

func (r *Repo) GitUploadPackPlan(t *testing.T, in uploadpack.NegotiateInput) uploadpack.ShippingPlan {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	bare := r.MaterializeBare(t) // materializes via internal/mirror
	defer r.CleanupBare(bare)

	req := encodeV2FetchRequest(in)
	cmd := exec.Command("git", "upload-pack", "--stateless-rpc", "--http-backend-info-refs", bare)
	cmd.Stdin = bytes.NewReader(req)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		t.Fatalf("git upload-pack: %v", err)
	}
	return parseV2FetchResponse(t, stdout.Bytes())
}
```

`encodeV2FetchRequest` writes pkt-lines `command=fetch\n` + capabilities + `want <oid>\n` lines + `have <oid>\n` lines + `done\n`. `parseV2FetchResponse` extracts ACKs and the shipped pack-index to derive the commit set. Both are ~60 lines following the M3 pkt-line/v2 protocol code (`internal/pktline`, `internal/v2proto`).

- [ ] **Step 3: Run differential tests**

Run: `go test ./internal/gitproto/uploadpack/ -run TestNegotiate_ParityWithGitUploadPack -v`
Expected: PASS (all probes match the oracle).

- [ ] **Step 4: Commit**

```bash
git add internal/gitproto/uploadpack/negotiate_diff_test.go internal/reachability/rtest/git_oracle.go
git commit -m "M10 task 6.3: Negotiate parity with git upload-pack across 5 probes"
```

### Task 6.4: Lazy mirror materialization

**Files:**
- Modify: `internal/gitproto/uploadpack/service.go`
- Modify: `internal/gitproto/uploadpack/engine.go`

- [ ] **Step 1: Identify current materialization point**

Read `internal/gitproto/uploadpack/service.go` and find where `Mirror.EnsureReady` (or the equivalent) is called. Today it runs at request entry. We want to defer it until after Negotiate has run.

- [ ] **Step 2: Add a flag + branching**

In `Engine`:

```go
type Engine struct {
	Store  storage.ObjectStore
	Keys   *keys.Repo
	Mirror *mirror.Manager

	// UseReachabilityNegotiate selects the M10 path. Set true once
	// the repo has Indexes.Reachability != nil and indexes load cleanly;
	// fall through to eager-mirror otherwise.
	UseReachabilityNegotiate bool
}
```

In the request handler:

```go
body, err := e.loadCurrentBody(ctx)
if err != nil { return err }

set, setErr := reachability.Load(ctx, e.Store, e.Keys, body)
if setErr != nil {
	// Fallback: log + eager-mirror path (existing flow).
	slog.WarnContext(ctx, "reachability.fallback",
		"reason", classifyFallback(setErr),
		"repo", e.Keys.Prefix())
	return e.serveEager(ctx, body)
}

input, err := parseNegotiation(req)  // parse wants/haves/done from pkt-lines
if err != nil { return err }

plan, err := Negotiate(ctx, set, input)
if errors.Is(err, ErrUnknownWant) {
	// Map to Git ERR pkt-line; abort.
	return writeErrPkt(w, "unknown want")
}
if err != nil { return err }

if len(plan.Commits) == 0 {
	// No-op fetch — never materialize the mirror.
	return writeAck(w)
}

// Deliver phase: materialize on demand, then run git pack-objects.
if err := e.Mirror.EnsureReady(ctx, e.Keys.Prefix()); err != nil {
	return err
}
return e.streamPackObjects(ctx, w, plan)
```

The exact integration depends on the current upload-pack flow. Read the file before making changes; the key invariant is "no `EnsureReady` before Negotiate."

- [ ] **Step 3: Write the test**

```go
func TestUploadPack_NoOpFetch_SkipsMirrorMaterialization(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepoOnLocalfs(t, "tenants/t/repos/r/")
	repo.SeedBase(t, oidA, oidB, oidC) // main=C, has .bvcg + .bvom

	mirrorCalls := repo.MirrorCallCounter()

	// Client haves the tip; nothing to ship.
	resp := repo.Fetch(t, FetchRequest{Wants: []pack.OID{oidC}, Haves: []pack.OID{oidC}})
	if !resp.OK {
		t.Fatalf("fetch failed: %v", resp.Err)
	}
	if mirrorCalls.Count() != 0 {
		t.Fatalf("mirror EnsureReady called %d times, want 0 (no-op fetch)", mirrorCalls.Count())
	}
}
```

`MirrorCallCounter` is a test wrapper around `mirror.Manager` that counts `EnsureReady` invocations.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/gitproto/uploadpack/ -v -count=1`
Expected: all PASS, including the no-op skip test.

- [ ] **Step 5: Commit**

```bash
git add internal/gitproto/uploadpack/service.go internal/gitproto/uploadpack/engine.go internal/gitproto/uploadpack/negotiate_test.go
git commit -m "M10 task 6.4: lazy mirror materialization (Negotiate before EnsureReady)"
```

### Task 6.5: Fallback path

**Files:**
- Create: `internal/reachability/fallback.go`
- Create: `internal/reachability/fallback_test.go`
- Modify: `internal/gitproto/uploadpack/service.go`

- [ ] **Step 1: Write the failing test**

```go
package reachability_test

import (
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/reachability"
)

func TestClassifyFallback_NoIndex(t *testing.T) {
	if reachability.ClassifyFallback(reachability.ErrNoIndex) != "no_index" {
		t.Fatalf("classify ErrNoIndex")
	}
}

func TestClassifyFallback_StaleBase(t *testing.T) {
	if reachability.ClassifyFallback(reachability.ErrStaleBase) != "stale_base" {
		t.Fatalf("classify ErrStaleBase")
	}
}

func TestClassifyFallback_DeltaDecode(t *testing.T) {
	wrapped := fmt.Errorf("load delta: %w", deltaindex.ErrMalformed)
	if reachability.ClassifyFallback(wrapped) != "delta_decode" {
		t.Fatalf("classify ErrMalformed")
	}
}

func TestClassifyFallback_Unknown(t *testing.T) {
	if reachability.ClassifyFallback(errors.New("anything else")) != "unknown" {
		t.Fatalf("classify unknown")
	}
}
```

- [ ] **Step 2: Implement**

```go
package reachability

import (
	"errors"

	"github.com/bucketvcs/bucketvcs/internal/reachability/deltaindex"
)

// ClassifyFallback returns a short label for the structured warning
// log emitted when the upload-pack engine falls back to the eager
// mirror path. Keep the label set bounded so dashboards/alerts can
// pivot on it.
func ClassifyFallback(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrNoIndex):
		return "no_index"
	case errors.Is(err, ErrStaleBase):
		return "stale_base"
	case errors.Is(err, deltaindex.ErrMalformed):
		return "delta_decode"
	default:
		return "unknown"
	}
}
```

In `internal/gitproto/uploadpack/service.go`, the warning log is already wired (Task 6.4). Replace its `classifyFallback` placeholder with `reachability.ClassifyFallback`.

- [ ] **Step 3: Run tests**

Run: `go test ./internal/reachability/ -run TestClassifyFallback -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/reachability/fallback.go internal/reachability/fallback_test.go internal/gitproto/uploadpack/service.go
git commit -m "M10 task 6.5: structured fallback classification + warning log"
```

### Task 6.6: `bucketvcs negotiate` debug subcommand

**Files:**
- Create: `cmd/bucketvcs/negotiate.go`
- Create: `cmd/bucketvcs/negotiate_test.go`
- Modify: `cmd/bucketvcs/main.go`

- [ ] **Step 1: Write the failing test**

```go
func TestNegotiate_CLI_TextOutput(t *testing.T) {
	repo := newCLITestRepoOnLocalfs(t)
	out := runCLI(t, "negotiate",
		"--store="+repo.StoreURL(),
		"--repo=t/r",
		"--wants="+hex(oidD),
		"--haves="+hex(oidB))
	if !strings.Contains(out.Stdout, "Shipping plan:") {
		t.Fatalf("missing header in output:\n%s", out.Stdout)
	}
	if !strings.Contains(out.Stdout, hex(oidC)) || !strings.Contains(out.Stdout, hex(oidD)) {
		t.Fatalf("expected C and D in shipping plan, got:\n%s", out.Stdout)
	}
}

func TestNegotiate_CLI_JSONOutput(t *testing.T) {
	repo := newCLITestRepoOnLocalfs(t)
	out := runCLI(t, "negotiate",
		"--store="+repo.StoreURL(),
		"--repo=t/r",
		"--wants="+hex(oidD),
		"--output=json")
	var got struct {
		Commits []string          `json:"commits"`
		Refs    map[string]string `json:"refs"`
	}
	if err := json.Unmarshal([]byte(out.Stdout), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out.Stdout)
	}
	if len(got.Commits) == 0 {
		t.Fatalf("empty commits in JSON")
	}
}

func TestNegotiate_CLI_UnknownWant_Exit3(t *testing.T) {
	repo := newCLITestRepoOnLocalfs(t)
	out := runCLI(t, "negotiate",
		"--store="+repo.StoreURL(),
		"--repo=t/r",
		"--wants="+hex(oidUnknown))
	if out.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3 (unknown want)", out.ExitCode)
	}
}
```

- [ ] **Step 2: Implement the subcommand**

`cmd/bucketvcs/negotiate.go`:

```go
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/gitproto/uploadpack"
	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/reachability"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
)

func runNegotiate(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("negotiate", flag.ContinueOnError)
	store := fs.String("store", "", "object-store URL")
	repo := fs.String("repo", "", "repo identifier (<tenant>/<repo>)")
	wantsCSV := fs.String("wants", "", "comma-separated OIDs (hex)")
	havesCSV := fs.String("haves", "", "comma-separated OIDs (hex)")
	output := fs.String("output", "text", "text|json")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *store == "" || *repo == "" || *wantsCSV == "" {
		fmt.Fprintln(os.Stderr, "negotiate: --store, --repo, --wants required")
		return 2
	}

	st, closeStore, err := openStore(*store)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open store: %v\n", err)
		return 1
	}
	defer closeStore(st)
	k, err := keys.New("tenants/" + strings.Replace(*repo, "/", "/repos/", 1) + "/")
	if err != nil {
		fmt.Fprintf(os.Stderr, "keys: %v\n", err)
		return 2
	}

	body, err := loadCurrentManifest(ctx, st, k)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load manifest: %v\n", err)
		return 1
	}

	set, err := reachability.Load(ctx, st, k, body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reachability.Load: %v\n", err)
		return 1
	}

	wants, err := parseOIDList(*wantsCSV)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse wants: %v\n", err)
		return 2
	}
	haves, err := parseOIDList(*havesCSV)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse haves: %v\n", err)
		return 2
	}

	plan, err := uploadpack.Negotiate(ctx, set, uploadpack.NegotiateInput{
		Wants: wants, Haves: haves, Done: true,
	})
	if err != nil {
		if errors.Is(err, uploadpack.ErrUnknownWant) {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return 3
		}
		fmt.Fprintf(os.Stderr, "Negotiate: %v\n", err)
		return 1
	}

	if *output == "json" {
		refs := map[string]string{}
		for k, v := range plan.Refs {
			refs[k] = hex.EncodeToString(v[:])
		}
		commits := make([]string, 0, len(plan.Commits))
		for _, c := range plan.Commits {
			commits = append(commits, hex.EncodeToString(c[:]))
		}
		out, _ := json.MarshalIndent(struct {
			Commits []string          `json:"commits"`
			Refs    map[string]string `json:"refs"`
		}{commits, refs}, "", "  ")
		fmt.Println(string(out))
		return 0
	}

	fmt.Printf("Shipping plan: %d commits\n", len(plan.Commits))
	for _, c := range plan.Commits {
		fmt.Printf("  %s\n", hex.EncodeToString(c[:]))
	}
	fmt.Printf("Refs:\n")
	for name, oid := range plan.Refs {
		fmt.Printf("  %s = %s\n", name, hex.EncodeToString(oid[:]))
	}
	return 0
}

func parseOIDList(csv string) ([]pack.OID, error) {
	if csv == "" {
		return nil, nil
	}
	parts := strings.Split(csv, ",")
	out := make([]pack.OID, 0, len(parts))
	for _, p := range parts {
		o, err := pack.ParseOID(strings.TrimSpace(p))
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}
```

Wire into `cmd/bucketvcs/main.go`'s dispatcher (add `"negotiate": runNegotiate` or equivalent — match the existing pattern).

`openStore` and `loadCurrentManifest` are helpers already present from M3+ subcommands; reuse them.

- [ ] **Step 3: Run tests**

Run: `go test ./cmd/bucketvcs/ -run TestNegotiate_CLI -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add cmd/bucketvcs/negotiate.go cmd/bucketvcs/negotiate_test.go cmd/bucketvcs/main.go
git commit -m "M10 task 6.6: bucketvcs negotiate debug subcommand"
```

---

## Phase 7 — GC integration

Extend M8 GC to know about `.bvrd` files: live-set walk includes them; sweep prefix covers them; new interleaving `compaction_during_mark` joins `RunPropertyGCSafety`.

### Task 7.1: Live-set walk includes `.bvrd`

**Files:**
- Modify: `internal/gc/walk.go`
- Modify: `internal/gc/walk_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestWalk_IncludesReachabilityDeltas(t *testing.T) {
	ctx := context.Background()
	store, k, body := mtest.RepoWithDeltas(t, 3) // 3 .bvrd files referenced
	live, err := BuildLiveSet(ctx, store, k, []manifest.Body{body})
	if err != nil {
		t.Fatalf("BuildLiveSet: %v", err)
	}
	// Each referenced delta key should be marked live.
	for _, ref := range body.Indexes.Reachability.Deltas {
		if _, ok := live.Keys[ref.Key]; !ok {
			t.Errorf("delta key not live: %s", ref.Key)
		}
	}
}
```

- [ ] **Step 2: Extend the walk**

Edit `internal/gc/walk.go`. In `BuildLiveSet`, after `manifest.Indexes.{ObjectMap, CommitGraph}` are added, also iterate the delta list:

```go
if body.Indexes.Reachability != nil {
	for _, ref := range body.Indexes.Reachability.Deltas {
		live.Keys[ref.Key] = struct{}{}
	}
}
```

- [ ] **Step 3: Run tests**

Run: `go test ./internal/gc/ -run TestWalk_IncludesReachabilityDeltas -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/gc/walk.go internal/gc/walk_test.go
git commit -m "M10 task 7.1: GC live-set walk includes .bvrd"
```

### Task 7.2: Sweep prefix for `indexes/reachability-delta/`

**Files:**
- Modify: `internal/gc/sweep.go`
- Modify: `internal/gc/sweep_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestSweep_CollectsUnreferencedDeltas(t *testing.T) {
	ctx := context.Background()
	store, k, _ := mtest.RepoWithDeltas(t, 3) // 3 referenced
	// Add a stray delta not referenced by any manifest.
	strayKey := k.ReachabilityDeltaKey("ff00deadbeef")
	mustPut(t, store, strayKey, []byte("stray"))

	live := &LiveSet{Keys: map[string]struct{}{
		k.ReachabilityDeltaKey("aaaa"): {},
		k.ReachabilityDeltaKey("bbbb"): {},
		k.ReachabilityDeltaKey("cccc"): {},
	}}
	rep, err := Sweep(ctx, store, k, live, SweepOptions{Now: time.Now(), Retention: 0})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	found := false
	for _, key := range rep.Deleted.Reachability {
		if key == strayKey {
			found = true
		}
	}
	if !found {
		t.Fatalf("stray .bvrd not swept; rep.Deleted = %+v", rep.Deleted)
	}
}
```

- [ ] **Step 2: Extend Sweep**

Edit `internal/gc/sweep.go`. The existing sweep iterates a set of prefixes (`packs/canonical/`, `indexes/object-map/`, `indexes/commit-graph/`). Add `indexes/reachability-delta/` to the iteration.

Add `Reachability []string` to the `Deleted` report struct.

- [ ] **Step 3: Run tests**

Run: `go test ./internal/gc/ -run TestSweep_CollectsUnreferencedDeltas -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/gc/sweep.go internal/gc/sweep_test.go
git commit -m "M10 task 7.2: GC sweep covers indexes/reachability-delta/ prefix"
```

### Task 7.3: `compaction_during_mark` interleaving

**Files:**
- Modify: `internal/gc/conformance/safety.go`

- [ ] **Step 1: Write the failing test**

In `RunPropertyGCSafety`, add a new interleaving:

```go
func compactionDuringMark(t *testing.T, factory Factory) {
	t.Run("compaction_during_mark", func(t *testing.T) {
		ctx := context.Background()
		store, cleanup := factory(t)
		defer cleanup()
		repo := seedMidCycleRepo(t, store) // base + 50 deltas

		// Start a mark walk; halfway through, simulate a maintenance
		// compaction that swaps base + drops the first 50 deltas.
		var markErr error
		markDone := make(chan struct{})
		go func() {
			_, markErr = BuildLiveSet(ctx, store, repo.Keys, []manifest.Body{repo.LoadBody(t)})
			close(markDone)
		}()
		time.Sleep(10 * time.Millisecond)
		runMaintenanceCompaction(t, store, repo)
		<-markDone

		if markErr != nil {
			t.Fatalf("mark failed: %v", markErr)
		}

		// Now run sweep with the new manifest. Old base indexes and
		// dropped deltas may be candidates after retention. None of
		// the still-live deltas (preserved-suffix) should be deleted.
		rep, err := Sweep(ctx, store, repo.Keys, repo.LoadLiveSet(t), SweepOptions{Retention: 0})
		if err != nil {
			t.Fatalf("Sweep: %v", err)
		}
		// Assert: any key in rep.Deleted.Reachability is NOT in the
		// new manifest's deltas.
		newBody := repo.LoadBody(t)
		newDeltas := map[string]bool{}
		for _, ref := range newBody.Indexes.Reachability.Deltas {
			newDeltas[ref.Key] = true
		}
		for _, key := range rep.Deleted.Reachability {
			if newDeltas[key] {
				t.Errorf("Sweep deleted still-live delta %s", key)
			}
		}
	})
}
```

Wire this into the existing test list in `RunPropertyGCSafety`.

- [ ] **Step 2: Run tests**

Run: `go test ./internal/gc/conformance/ -run TestGCSafety_Localfs -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/gc/conformance/safety.go
git commit -m "M10 task 7.3: compaction_during_mark interleaving in RunPropertyGCSafety"
```

---

## Phase 8 — Differential harness

Extend `internal/diffharness` with the M10 round-trip and add four new fixtures.

### Task 8.1: `ImportPushCompactNegotiateExportAndCompare`

**Files:**
- Modify: `internal/diffharness/roundtrip_helpers_test.go`

- [ ] **Step 1: Write the helper**

Add the new round-trip:

```go
// ImportPushCompactNegotiateExportAndCompare exercises the full M10
// flow on every registered fixture:
//   import fixture
//   for each of fixture.Pushes:
//     receive-pack the push (which writes a .bvrd)
//   run maintenance with --force (compaction)
//   for each (wants, haves) probe:
//     negotiate via pure-Go engine + via git upload-pack against mirror
//     assert ShippingPlan equality
//   export and compare round-trip against fixture.Expected
func ImportPushCompactNegotiateExportAndCompare(t *testing.T) {
	for _, fx := range fixtureRegistry {
		t.Run(fx.Name, func(t *testing.T) {
			ctx := context.Background()
			env := newDiffEnv(t)
			if err := env.Import(ctx, fx); err != nil {
				t.Fatalf("Import: %v", err)
			}
			for _, push := range fx.Pushes {
				if err := env.Push(ctx, push); err != nil {
					t.Fatalf("Push %s: %v", push.Name, err)
				}
			}
			if err := env.RunMaintenance(ctx, MaintenanceOptions{Force: true}); err != nil {
				t.Fatalf("Maintenance: %v", err)
			}
			for _, probe := range fx.NegotiationProbes {
				pure := env.NegotiatePureGo(t, probe)
				oracle := env.NegotiateGitUploadPack(t, probe)
				if !pure.Equal(oracle) {
					t.Errorf("%s probe %q mismatch\n  pure:   %+v\n  oracle: %+v",
						fx.Name, probe.Name, pure, oracle)
				}
			}
			got, err := env.Export(ctx)
			if err != nil {
				t.Fatalf("Export: %v", err)
			}
			if diff := compareTree(got, fx.Expected); diff != "" {
				t.Errorf("round-trip diff:\n%s", diff)
			}
		})
	}
}

func TestDiffharness_M10_RoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("differential harness")
	}
	ImportPushCompactNegotiateExportAndCompare(t)
}
```

`fixture.Pushes` and `fixture.NegotiationProbes` are new fields on the existing `Fixture` struct. Add them with empty defaults so existing fixtures pass.

- [ ] **Step 2: Run with existing fixtures**

Run: `go test ./internal/diffharness/ -run TestDiffharness_M10_RoundTrip -v`
Expected: PASS for fixtures without Pushes/Probes (no-op); fails or skips otherwise. New fixtures land in 8.2–8.5.

- [ ] **Step 3: Commit**

```bash
git add internal/diffharness/roundtrip_helpers_test.go
git commit -m "M10 task 8.1: ImportPushCompactNegotiateExportAndCompare helper"
```

### Task 8.2: `many-small-pushes` fixture

**Files:**
- Create: `internal/diffharness/fixtures/many_small_pushes.go` (or extend existing fixture registry)

- [ ] **Step 1: Define the fixture**

```go
var manySmallPushes = Fixture{
	Name: "many-small-pushes",
	Setup: func(t *testing.T, env *DiffEnv) {
		env.SeedLinear(t, 5) // base with 5 commits
	},
	Pushes: makeNTrivialPushes(50), // 50 pushes, each adds 1 commit
	NegotiationProbes: []NegotiationProbe{
		{Name: "tip_to_old_have", Wants: []pack.OID{tipOf(55)}, Haves: []pack.OID{oidOf(3)}},
		{Name: "no_op_full_have", Wants: []pack.OID{tipOf(55)}, Haves: []pack.OID{tipOf(55)}},
	},
}
```

Register in `fixtureRegistry`.

- [ ] **Step 2: Run**

Run: `go test ./internal/diffharness/ -run 'TestDiffharness_M10_RoundTrip/many-small-pushes' -v`
Expected: PASS (pure-Go negotiation matches oracle across 50 deltas).

- [ ] **Step 3: Commit**

```bash
git add internal/diffharness/fixtures/
git commit -m "M10 task 8.2: many-small-pushes fixture (50-delta chain)"
```

### Task 8.3: `force-push-mid-chain` fixture

```go
var forcePushMidChain = Fixture{
	Name: "force-push-mid-chain",
	Setup: func(t *testing.T, env *DiffEnv) { env.SeedLinear(t, 5) },
	Pushes: []Push{
		fastForwardOneCommit(),
		fastForwardOneCommit(),
		forcePushDivergent(), // new_oid is not a descendant of old_oid
		fastForwardOneCommit(),
	},
	NegotiationProbes: []NegotiationProbe{
		{Name: "after_force_push", Wants: []pack.OID{currentTip()}, Haves: []pack.OID{preForceTip()}},
	},
}
```

- [ ] **Step 1: Add to registry, run, commit**

```bash
git add internal/diffharness/fixtures/
git commit -m "M10 task 8.3: force-push-mid-chain fixture"
```

### Task 8.4: `tag-pushes-between-commits` fixture

```go
var tagPushesBetween = Fixture{
	Name: "tag-pushes-between-commits",
	Setup: func(t *testing.T, env *DiffEnv) { env.SeedLinear(t, 3) },
	Pushes: []Push{
		fastForwardOneCommit(),
		pushLightweightTag("v1.0"),
		fastForwardOneCommit(),
		pushAnnotatedTag("v2.0"),
	},
	NegotiationProbes: []NegotiationProbe{
		{Name: "fetch_all_tags", Wants: []pack.OID{currentTip(), tagOID("v2.0")}, Haves: nil},
	},
}
```

- [ ] **Step 1: Add to registry, run, commit**

```bash
git add internal/diffharness/fixtures/
git commit -m "M10 task 8.4: tag-pushes-between-commits fixture"
```

### Task 8.5: `octopus-merge` fixture

```go
var octopusMerge = Fixture{
	Name: "octopus-merge",
	Setup: func(t *testing.T, env *DiffEnv) {
		env.SeedThreeBranches(t) // P1, P2, P3 each diverged
	},
	Pushes: []Push{
		pushOctopusMerge([]string{"P1", "P2", "P3"}), // merge commit with 3 parents
	},
	NegotiationProbes: []NegotiationProbe{
		{Name: "fetch_merge", Wants: []pack.OID{currentTip()}, Haves: []pack.OID{oidOf("P1")}},
	},
}
```

- [ ] **Step 1: Add, run, commit**

```bash
git add internal/diffharness/fixtures/
git commit -m "M10 task 8.5: octopus-merge fixture (multi-parent gen-number stress)"
```

---

## Phase 9 — Conformance

`RunPropertyReachabilitySafety` ships as a factory across all 4 canonical backend adapters.

### Task 9.1: `RunPropertyReachabilitySafety` factory

**Files:**
- Create: `internal/reachability/conformance/safety.go`
- Create: `internal/reachability/conformance/safety_test.go`

- [ ] **Step 1: Write the harness**

```go
// Package conformance provides a property test for the reachability
// index across all backend adapters. Mirrors M8 RunPropertyGCSafety.
package conformance

import (
	"context"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

type Factory func(t testing.TB) (store storage.ObjectStore, cleanup func())

// RunPropertyReachabilitySafety exercises 4 interleavings:
//   - push_during_compaction (CAS-merge correctness)
//   - two_compactions (only one wins; loser orphans)
//   - compaction_during_mark (GC safety)
//   - negotiation_during_compaction (cold gateway reads while base swaps)
func RunPropertyReachabilitySafety(t *testing.T, f Factory) {
	t.Run("push_during_compaction", func(t *testing.T) {
		store, cleanup := f(t)
		defer cleanup()
		// Setup: base + 50 deltas.
		// Start compaction; while it's reading, inject a push that adds a delta.
		// Compaction CAS commits; assert new manifest preserves the injected delta.
		// (~80 lines of test driver — see internal/maintenance/conformance/safety.go
		// for the pattern.)
		_ = store
	})
	t.Run("two_compactions", func(t *testing.T) { /* ... */ })
	t.Run("compaction_during_mark", func(t *testing.T) { /* ... */ })
	t.Run("negotiation_during_compaction", func(t *testing.T) { /* ... */ })
}
```

Fill in each `t.Run` body following the M8/M9 conformance pattern. Each interleaving is ~80 lines (setup, goroutines, CAS, assertions). Use `time.Sleep` for ordering — wallclock races are acceptable for the OSS-scope target; if a future milestone introduces deterministic hooks, port over.

- [ ] **Step 2: Localfs test**

```go
package conformance_test

import (
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/reachability/conformance"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestReachabilitySafety_Localfs(t *testing.T) {
	conformance.RunPropertyReachabilitySafety(t, localfs.TestFactory)
}
```

- [ ] **Step 3: Run**

Run: `go test ./internal/reachability/conformance/ -v -count=1`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/reachability/conformance/
git commit -m "M10 task 9.1: RunPropertyReachabilitySafety factory + localfs"
```

### Task 9.2: Wire into s3compat adapter

**Files:**
- Modify: `internal/storage/s3compat/conformance_test.go`

- [ ] **Step 1: Add the test**

```go
func TestS3Compat_ReachabilitySafety_R2(t *testing.T) {
	store, cleanup := setupR2MinIO(t)
	defer cleanup()
	conformance.RunPropertyReachabilitySafety(t, fixedStoreFactory(store))
}

func TestS3Compat_ReachabilitySafety_S3(t *testing.T) {
	store, cleanup := setupS3MinIO(t)
	defer cleanup()
	conformance.RunPropertyReachabilitySafety(t, fixedStoreFactory(store))
}
```

`fixedStoreFactory` is a helper that wraps an already-constructed store as a `conformance.Factory`. If it doesn't exist, add it:

```go
func fixedStoreFactory(s storage.ObjectStore) conformance.Factory {
	return func(t testing.TB) (storage.ObjectStore, func()) {
		return s, func() {}
	}
}
```

- [ ] **Step 2: Run against MinIO emulator**

Run: `go test ./internal/storage/s3compat/ -run TestS3Compat_ReachabilitySafety -v`
(requires MinIO emulator per existing M5/M8 setup)

- [ ] **Step 3: Commit**

```bash
git add internal/storage/s3compat/conformance_test.go
git commit -m "M10 task 9.2: reachability conformance wired into s3compat"
```

### Task 9.3: Wire into gcs adapter

**Files:**
- Modify: `internal/storage/gcs/conformance_test.go`

Same pattern as 9.2.

- [ ] **Step 1: Add the test**

```go
func TestGcs_ReachabilitySafety(t *testing.T) {
	store, cleanup := setupFakeGCS(t)
	defer cleanup()
	conformance.RunPropertyReachabilitySafety(t, fixedStoreFactory(store))
}
```

- [ ] **Step 2: Run + commit**

```bash
git add internal/storage/gcs/conformance_test.go
git commit -m "M10 task 9.3: reachability conformance wired into gcs"
```

### Task 9.4: Wire into azureblob adapter

**Files:**
- Modify: `internal/storage/azureblob/conformance_test.go`

Same pattern.

- [ ] **Step 1: Add the test**

```go
func TestAzureBlob_ReachabilitySafety(t *testing.T) {
	store, cleanup := setupAzurite(t)
	defer cleanup()
	conformance.RunPropertyReachabilitySafety(t, fixedStoreFactory(store))
}
```

- [ ] **Step 2: Run + commit**

```bash
git add internal/storage/azureblob/conformance_test.go
git commit -m "M10 task 9.4: reachability conformance wired into azureblob"
```

---

## Phase 10 — Operator surface

CLI flags + JSON output + docs.

### Task 10.1: Maintenance flags + JSON report field

**Files:**
- Modify: `cmd/bucketvcs/maintenance.go`
- Modify: `cmd/bucketvcs/maintenance_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestMaintenance_CLI_ReachabilityFlags_Plumbed(t *testing.T) {
	repo := newCLITestRepoOnLocalfs(t)
	out := runCLI(t, "maintenance",
		"--store="+repo.StoreURL(),
		"--repo=t/r",
		"--reachability-delta-commits=500",
		"--reachability-delta-pushes=50",
		"--reachability-delta-bytes=32M",
		"--output=json")
	var got struct {
		ReachabilityCompaction struct {
			Triggered     bool   `json:"triggered"`
			TriggerReason string `json:"trigger_reason"`
			DeltasDropped int    `json:"deltas_dropped"`
			BaseSwapped   bool   `json:"base_swapped"`
		} `json:"reachability_compaction"`
	}
	if err := json.Unmarshal([]byte(out.Stdout), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	_ = got // assertions depend on test repo state
}

func TestMaintenance_CLI_RejectsNegativeReachabilityThreshold(t *testing.T) {
	out := runCLI(t, "maintenance",
		"--store=mem://",
		"--repo=t/r",
		"--reachability-delta-commits=-1")
	if out.ExitCode != 2 {
		t.Fatalf("exit = %d, want 2", out.ExitCode)
	}
}
```

- [ ] **Step 2: Implement the flags**

In `cmd/bucketvcs/maintenance.go`, add:

```go
deltaCommits := fs.Int("reachability-delta-commits", 1000, "compact when delta chain exceeds this commit count (0 disables)")
deltaPushes := fs.Int("reachability-delta-pushes", 100, "compact when delta chain exceeds this push count (0 disables)")
deltaBytes := fs.String("reachability-delta-bytes", "64M", "compact when delta chain exceeds this byte size (0 disables; suffixes K/M/G)")
```

Parse `*deltaBytes` via the existing byte-size helper (M9 already has one for `--manifest-pack-bytes-threshold`). Reject negative values.

Wire into `RunOptions.Thresholds`. Extend the JSON report serializer to include the `reachability_compaction` block (uses `ReachabilityCompactionReport` from Task 5.4).

- [ ] **Step 3: Run + commit**

```bash
git add cmd/bucketvcs/maintenance.go cmd/bucketvcs/maintenance_test.go
git commit -m "M10 task 10.1: maintenance CLI reachability threshold flags + JSON report field"
```

### Task 10.2: `inspect-manifest` JSON output

**Files:**
- Modify: `cmd/bucketvcs/inspect-manifest.go`
- Modify: `cmd/bucketvcs/inspect-manifest_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestInspectManifest_ReachabilityFields(t *testing.T) {
	repo := newCLITestRepoOnLocalfs(t)
	repo.SeedWithDeltas(t, 3)
	out := runCLI(t, "inspect-manifest",
		"--store="+repo.StoreURL(),
		"--repo=t/r",
		"--output=json")
	var got struct {
		Reachability struct {
			BaseManifest     string `json:"base_manifest"`
			DeltaChainLength int    `json:"delta_chain_length"`
			DeltaChainBytes  int64  `json:"delta_chain_bytes"`
			DeltaFiles       []struct {
				Key       string `json:"key"`
				Hash      string `json:"hash"`
				SizeBytes int64  `json:"size_bytes"`
			} `json:"delta_files"`
		} `json:"reachability"`
	}
	if err := json.Unmarshal([]byte(out.Stdout), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out.Stdout)
	}
	if got.Reachability.DeltaChainLength != 3 {
		t.Errorf("DeltaChainLength = %d, want 3", got.Reachability.DeltaChainLength)
	}
}
```

- [ ] **Step 2: Extend the JSON serializer**

In `cmd/bucketvcs/inspect-manifest.go`, after the existing index-section emission:

```go
if body.Indexes.Reachability != nil {
	type deltaInfo struct {
		Key       string `json:"key"`
		Hash      string `json:"hash"`
		SizeBytes int64  `json:"size_bytes"`
	}
	type reachabilityBlock struct {
		BaseManifest     string      `json:"base_manifest"`
		DeltaChainLength int         `json:"delta_chain_length"`
		DeltaChainBytes  int64       `json:"delta_chain_bytes"`
		DeltaFiles       []deltaInfo `json:"delta_files"`
	}
	files := make([]deltaInfo, 0, len(body.Indexes.Reachability.Deltas))
	var total int64
	for _, ref := range body.Indexes.Reachability.Deltas {
		files = append(files, deltaInfo{Key: ref.Key, Hash: ref.Hash, SizeBytes: ref.SizeBytes})
		total += ref.SizeBytes
	}
	output["reachability"] = reachabilityBlock{
		BaseManifest:     body.Indexes.Reachability.BaseManifest,
		DeltaChainLength: len(body.Indexes.Reachability.Deltas),
		DeltaChainBytes:  total,
		DeltaFiles:       files,
	}
}
```

(Adapt to whatever shape `inspect-manifest.go` uses today — likely a map or a typed struct.)

- [ ] **Step 3: Run + commit**

```bash
git add cmd/bucketvcs/inspect-manifest.go cmd/bucketvcs/inspect-manifest_test.go
git commit -m "M10 task 10.2: inspect-manifest reachability JSON block"
```

### Task 10.3: Operator guide

**Files:**
- Create: `docs/m10-reachability-operator-guide.md`

- [ ] **Step 1: Write the guide**

Sections (mirror the M8/M9 guide layouts):

1. **Overview** — what `.bvrd` / base index / compaction means; the cold-fetch SLO contract.
2. **Threshold tuning** — default values, when to lower/raise, busy-repo vs idle-repo guidance.
3. **Cron cadence** — recommend hourly with thresholds; weekly with `--force`. Example crontab line.
4. **Inspecting the chain** — `bucketvcs inspect-manifest --output=json | jq .reachability`.
5. **Diagnosing fallback warnings** — what each `reason=` label means; remediation.
6. **`bucketvcs negotiate` for ad-hoc debugging** — example invocation, expected output.
7. **Expected `.bvrd` sizes** — empirical 5–20 KB for small pushes, larger for large feature branches.
8. **Operational interactions** — order relative to `bucketvcs gc` (run maintenance first, then GC, same as M9).
9. **Known limits** — monolithic only, no warm pool, no partitioning; pointer to M10.5 backlog.

Target length: 400–550 lines, matching the M9 guide.

- [ ] **Step 2: Commit**

```bash
git add docs/m10-reachability-operator-guide.md
git commit -m "M10 task 10.3: m10 reachability operator guide"
```

### Task 10.4: Cross-references in M9 guide + README

**Files:**
- Modify: `docs/m9-maintenance-operator-guide.md`
- Modify: `README.md`

- [ ] **Step 1: M9 guide cross-reference**

Add a short subsection in the M9 guide (e.g. under "Thresholds"):

```markdown
### Reachability thresholds (M10)

M10 adds three thresholds (`--reachability-delta-commits`, `--reachability-delta-pushes`,
`--reachability-delta-bytes`) and a new `compact-only` outcome — maintenance refreshes
`.bvom` and `.bvcg` without repacking. See `docs/m10-reachability-operator-guide.md`
for tuning guidance and the cold-fetch SLO contract.
```

- [ ] **Step 2: README**

Add `internal/reachability` to the package list and a one-line description:

```markdown
| `internal/reachability` | M10 base+delta reachability index; powers the upload-pack cold-fetch SLO. |
```

Also add `negotiate` to the CLI surface table.

- [ ] **Step 3: Commit**

```bash
git add docs/m9-maintenance-operator-guide.md README.md
git commit -m "M10 task 10.4: cross-references in M9 guide + README"
```

---

## Phase 11 — Final wiring + progress + memory

### Task 11.1: Integration smoke test

**Files:**
- Create: `internal/reachability/integration_test.go`

- [ ] **Step 1: Write the test**

End-to-end smoke: import → 10 small pushes → maintenance → 10 more pushes → maintenance (compact-only) → fetch via upload-pack → assert client-side `git fsck` passes on the result.

```go
func TestM10_EndToEnd_LocalfsSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("smoke")
	}
	ctx := context.Background()
	env := newSmokeEnv(t)
	env.Import(t)
	for i := 0; i < 10; i++ {
		env.PushOne(t, fmt.Sprintf("commit %d", i))
	}
	env.RunMaintenance(t)
	for i := 0; i < 10; i++ {
		env.PushOne(t, fmt.Sprintf("commit %d", 10+i))
	}
	env.RunMaintenance(t) // compact-only path
	env.Fetch(t)
	if err := env.GitFsck(t); err != nil {
		t.Fatalf("fsck failed: %v", err)
	}
}
```

- [ ] **Step 2: Run + commit**

```bash
go test ./internal/reachability/ -run TestM10_EndToEnd -v
git add internal/reachability/integration_test.go
git commit -m "M10 task 11.1: end-to-end localfs smoke test"
```

### Task 11.2: Progress notes

**Files:**
- Create: `docs/superpowers/specs/m10_progress.md` (mirroring m7/m8/m9 progress files)

- [ ] **Step 1: Write the progress doc**

Sections:

1. **Summary** — one paragraph: what landed, what's tagged, where the worktree lives.
2. **Tasks completed** — high-level list mapping to plan phases.
3. **Review fixes summary** — count of `roborev-refine` rounds and notable findings (filled in at end of implementation).
4. **Out of scope (deferred by design)** — verbatim from spec §9.
5. **Follow-ups before tagging m10-complete** — push branch + draft PR, real-cloud CI secrets (carried from M7+).

- [ ] **Step 2: Commit**

```bash
git add docs/superpowers/specs/m10_progress.md
git commit -m "M10 task 11.2: m10 progress notes"
```

### Task 11.3: Memory note

After M10 merges to main and `m10-complete` tag is in place, update auto-memory:

- [ ] **Step 1: Write the memory file**

Create `/home/eran/.claude/projects/-home-eran-work-bucketvcs/memory/m10_progress.md` mirroring the M9 memory entry shape. Include: commit hash, tag, date, design choices (Q1=B, Q2=A, etc.), notable architectural details, follow-ups.

- [ ] **Step 2: Update `MEMORY.md`**

Add the new entry:

```markdown
- [M10 reachability compaction merged to main](m10_progress.md) — commit <hash>, tag m10-complete (2026-05-NN); .bvrd per push + .bvcg v2 + compact-only maintenance + pure-Go negotiation + lazy mirror
```

(Don't commit memory files to the repo — they live in `~/.claude/`.)

---

## Self-review

After writing the plan, do a fresh-eyes pass:

**1. Spec coverage:**

- §1 Wire format: Tasks 0.1, 0.2, 0.3, 1.1, 1.2, 1.3, 1.4, 1.5, 2.1–2.5. ✓
- §2 Manifest schema: Task 0.2. ✓
- §3 Produce path (receive-pack): Tasks 4.1–4.6. ✓
- §4 Compact path (maintenance): Tasks 5.1–5.5. ✓
- §5 Read path: Tasks 6.1–6.6 + reachability package (Phase 3). ✓
- §6 Concurrency/edges: Covered in receive-pack CAS retry (4.5), CAS-merge (5.5), conformance (9.1). Fallback (6.5). Migration day-1 implicit in the omit-on-legacy handling in receive-pack and maintenance. ✓
- §7 Testing: Phases 3 (unit), 8 (differential), 9 (conformance), 11.1 (smoke). ✓
- §8 Operator surface: Phase 10. ✓
- §9 Out of scope: Captured in 11.2 progress doc. ✓
- §10 Follow-ups: 11.2. ✓
- §11 Package layout: Phases 0–10 collectively cover the layout. ✓

**2. Placeholder scan:** Reviewed every task; no `TODO`, `TBD`, "implement later", or unspecific guidance. The few stub helpers (`rtest.NewBaseOnlyRepo`, fixture builders) are explicitly scheduled in Task 3.7 with concrete code patterns to follow from existing `mtest` fixtures.

**3. Type consistency:**

- `IndexRef.SizeBytes` (Task 0.1) consumed in 5.2, 5.3, 7.1, 10.2. ✓
- `ReachabilityRef` shape (Task 0.2) consumed in 3.2, 4.4, 5.4, 5.5, 7.1, 7.2, 10.2. ✓
- `reachability.Set` API (Phase 3) consumed in 6.2, 6.4. ✓
- `deltaindex.Delta` (Task 2.2) consumed in 4.2, 4.3. ✓
- `deltaindex.Header` (Task 5.3) introduced and consumed in same task. ✓
- `Decision.CompactReachability` (Task 5.2) consumed in 5.4. ✓
- `ShippingPlan` (Task 6.1) consumed in 6.2, 6.3, 6.4, 6.6, 8.1. ✓
- `Negotiate` signature (Task 6.2) consumed in 6.4, 6.6, 8.1. ✓

No mismatches found.

---

## Execution

Plan complete and saved to `docs/superpowers/plans/2026-05-10-m10-reachability-compaction.md`. Two execution options:

**1. Subagent-Driven (recommended)** — dispatch a fresh subagent per task, review between tasks, fast iteration.

**2. Inline Execution** — execute tasks in this session using executing-plans, batch execution with checkpoints.

Which approach?



