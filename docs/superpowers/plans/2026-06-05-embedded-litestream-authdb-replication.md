# Embedded Litestream Authdb Replication Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Continuously replicate the sqlite authdb into any supported object store by embedding Litestream v0.5 with a custom `ReplicaClient` backed by `storage.ObjectStore`, with restore-on-boot, a restore/status CLI, and a CAS lease split-brain guard.

**Architecture:** New package `internal/authreplica` with three units: `Client` (litestream `ReplicaClient` over `ObjectStore`), `Lease` (CAS lease at `sys/authdb/lease.json`), `Runner` (two-phase lifecycle glue: Prepare = lease+restore before authdb open; StartReplication = litestream store after authdb open). Serve gains `--auth-db-replica` flags; new `bucketvcs authdb restore|replica-status` subcommand.

**Tech Stack:** Go 1.26, `github.com/benbjohnson/litestream v0.5.11` (pinned), `github.com/superfly/ltx`, existing `internal/storage` adapters, `modernc.org/sqlite` (shared by us and litestream — the documented-safe combination).

**Spec:** `docs/superpowers/specs/2026-06-05-embedded-litestream-authdb-replication-design.md`

**Module path:** `github.com/bucketvcs/bucketvcs`

---

## Background for the implementer (read first)

- **`storage.ObjectStore`** (internal/storage/objectstore.go) has NO unconditional Put/Delete. Writes are `PutIfAbsent` / `PutIfVersionMatches`; deletes are `DeleteIfVersionMatches`. Sentinels in internal/storage/errors.go: `storage.ErrNotFound`, `storage.ErrAlreadyExists`, `storage.ErrVersionMismatch`, `storage.ErrNotSupported`. `List` returns `*ListPage{Objects []ObjectMetadata, NextToken string}`; `ObjectMetadata` has `Key, Version, Size, ContentType, ModifiedAt`. `GetRange(ctx, key, start, endInclusive)`.
- **litestream v0.5.11 `ReplicaClient`** interface (package `litestream`, types from `github.com/superfly/ltx`):
  `Type() string`; `Init(ctx) error` (idempotent); `LTXFiles(ctx, level int, seek ltx.TXID, useMetadata bool) (ltx.FileIterator, error)`; `OpenLTXFile(ctx, level int, minTXID, maxTXID ltx.TXID, offset, size int64) (io.ReadCloser, error)` (missing → `os.ErrNotExist`); `WriteLTXFile(ctx, level, minTXID, maxTXID, r io.Reader) (*ltx.FileInfo, error)`; `DeleteLTXFiles(ctx, a []*ltx.FileInfo) error`; `DeleteAll(ctx) error`; `SetLogger(*slog.Logger)`.
- **Canonical filename format:** `ltx.FormatFilename(minTXID, maxTXID)` / `ltx.ParseFilename(name)` — same names the `file` backend uses (`<root>/ltx/<level>/<formatted>`). We mirror that layout under our prefix.
- **Embedding flow:** `db := litestream.NewDB(path)`; `db.Replica = litestream.NewReplicaWithClient(db, client)`; `db.EnsureExists(ctx)` restores iff the local file is missing (call BEFORE the app opens sqlite); `store := litestream.NewStore([]*litestream.DB{db}, levels)`; `store.Open(ctx)` starts replication+compaction; `store.Close(ctx)` does a bounded shutdown sync. `litestream.CompactionLevels` is `[]*litestream.CompactionLevel` with fields `Level int`, `Interval time.Duration`.
- **The litestream conformance harness is NOT importable** (`package litestream_test` + `internal/testingutil`). We adapt its behaviors into our own conformance file (Task 3). Apache-2.0 — keep an attribution comment.
- **Serve conventions:** flags on a `flag.FlagSet` in cmd/bucketvcs/serveflags.go; usage errors are `fmt.Fprintln(stderr, "serve: ...")` + `return 2` (see the LFS hard-require at cmd/bucketvcs/serve.go:96-101). Authdb opens at serve.go:251 (`openAuthDB(*authDB, ...)`). Replica-serve mode is active when `*replicaOf != ""` (serve.go:171, `isReplica`). Background workers start near serve.go:489-496 under `serveCtx`.
- **Metric/audit conventions:** `slog.LogAttrs(ctx, slog.LevelInfo, "metric", slog.String("metric_name", NAME), slog.Int64("value", V), ...)`; audit events use message = event name + `slog.Bool("audit", true)` + `slog.String("event", NAME)` (see internal/gateway/log.go:13-53).
- **Smokes** live in `scripts/*.sh`, bash, `set -euo pipefail`, exit 77 = SKIP, success marker line at the end.
- **Verify before relying on fields:** after Task 1, run `go doc github.com/benbjohnson/litestream.RestoreOptions` and `go doc github.com/superfly/ltx.ParseTXID`. Confirmed-by-usage fields: `RestoreOptions.OutputPath string`, `.TXID ltx.TXID`, `.Timestamp time.Time`. If a name differs, adjust call sites in Tasks 5/7 accordingly (one-line fixes).

---

### Task 1: Pin the litestream dependency

**Files:**
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1: Add pinned dependency**

```bash
cd /home/eran/work/bucketvcs
go get github.com/benbjohnson/litestream@v0.5.11
go get github.com/superfly/ltx
go mod tidy
```

- [ ] **Step 2: Verify it builds and inspect the API we depend on**

```bash
go build ./... && go doc github.com/benbjohnson/litestream.ReplicaClient | head -30
go doc github.com/benbjohnson/litestream.RestoreOptions
go doc github.com/benbjohnson/litestream.CompactionLevel
go doc github.com/superfly/ltx.FormatFilename
go doc github.com/superfly/ltx.ParseTXID
```

Expected: build OK; `ReplicaClient` shows the 7 methods listed in Background. Note the exact `RestoreOptions` field names for Tasks 5/7.

- [ ] **Step 3: Commit**

```bash
git add go.mod go.sum
git commit -m "build: pin github.com/benbjohnson/litestream v0.5.11"
```

---

### Task 2: `authreplica.Client` — ReplicaClient over ObjectStore

**Files:**
- Create: `internal/authreplica/client.go`
- Test: `internal/authreplica/client_test.go`

- [ ] **Step 1: Write the failing tests**

`internal/authreplica/client_test.go`:

```go
package authreplica

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/superfly/ltx"

	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func newLocalFS(t *testing.T) storage.ObjectStore {
	t.Helper()
	s, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// ltxPayload returns bytes whose first chunk parses as an LTX header is NOT
// guaranteed here; Client falls back to store timestamps when PeekHeader
// fails (see WriteLTXFile). Real-LTX round-trips are covered by the Runner
// integration test in runner_test.go.
func ltxPayload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

func TestClient_WriteOpenRoundTrip(t *testing.T) {
	ctx := context.Background()
	c := NewClient(newLocalFS(t), "sys/authdb")
	body := ltxPayload(4096)

	info, err := c.WriteLTXFile(ctx, 0, ltx.TXID(1), ltx.TXID(5), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if info.Level != 0 || info.MinTXID != 1 || info.MaxTXID != 5 || info.Size != int64(len(body)) {
		t.Fatalf("bad FileInfo: %+v", info)
	}

	rc, err := c.OpenLTXFile(ctx, 0, ltx.TXID(1), ltx.TXID(5), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, body) {
		t.Fatalf("round-trip mismatch: got %d bytes", len(got))
	}
}

func TestClient_OpenLTXFile_Range(t *testing.T) {
	ctx := context.Background()
	c := NewClient(newLocalFS(t), "sys/authdb")
	body := ltxPayload(1000)
	if _, err := c.WriteLTXFile(ctx, 1, 10, 20, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}

	// offset+size
	rc, err := c.OpenLTXFile(ctx, 1, 10, 20, 100, 50)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, body[100:150]) {
		t.Fatalf("range mismatch: got %d bytes", len(got))
	}

	// offset, size=0 → rest of file
	rc, err = c.OpenLTXFile(ctx, 1, 10, 20, 900, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, body[900:]) {
		t.Fatalf("tail range mismatch: got %d bytes", len(got))
	}
}

func TestClient_OpenLTXFile_NotExist(t *testing.T) {
	c := NewClient(newLocalFS(t), "sys/authdb")
	_, err := c.OpenLTXFile(context.Background(), 0, 1, 2, 0, 0)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want os.ErrNotExist, got %v", err)
	}
}

func TestClient_LTXFiles_OrderAndSeek(t *testing.T) {
	ctx := context.Background()
	c := NewClient(newLocalFS(t), "sys/authdb")
	for _, r := range [][2]ltx.TXID{{1, 5}, {6, 9}, {10, 12}} {
		if _, err := c.WriteLTXFile(ctx, 0, r[0], r[1], bytes.NewReader(ltxPayload(64))); err != nil {
			t.Fatal(err)
		}
	}

	itr, err := c.LTXFiles(ctx, 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	var mins []ltx.TXID
	for itr.Next() {
		mins = append(mins, itr.Item().MinTXID)
	}
	itr.Close()
	if len(mins) != 3 || mins[0] != 1 || mins[1] != 6 || mins[2] != 10 {
		t.Fatalf("bad order: %v", mins)
	}

	// seek skips files with MinTXID < seek
	itr, err = c.LTXFiles(ctx, 0, 6, false)
	if err != nil {
		t.Fatal(err)
	}
	mins = nil
	for itr.Next() {
		mins = append(mins, itr.Item().MinTXID)
	}
	itr.Close()
	if len(mins) != 2 || mins[0] != 6 {
		t.Fatalf("bad seek result: %v", mins)
	}

	// empty level → empty iterator, no error
	itr, err = c.LTXFiles(ctx, 7, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if itr.Next() {
		t.Fatal("expected empty iterator")
	}
	itr.Close()
}

func TestClient_WriteLTXFile_OverwritesExisting(t *testing.T) {
	// Crash-retry semantics: litestream may rewrite the same (level,min,max).
	// Native clients do an unconditional PUT; we synthesize it from CAS.
	ctx := context.Background()
	c := NewClient(newLocalFS(t), "sys/authdb")
	if _, err := c.WriteLTXFile(ctx, 0, 1, 5, bytes.NewReader(ltxPayload(64))); err != nil {
		t.Fatal(err)
	}
	second := ltxPayload(128)
	if _, err := c.WriteLTXFile(ctx, 0, 1, 5, bytes.NewReader(second)); err != nil {
		t.Fatal(err)
	}
	rc, err := c.OpenLTXFile(ctx, 0, 1, 5, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, second) {
		t.Fatal("second write did not win")
	}
}

func TestClient_DeleteLTXFiles_Idempotent(t *testing.T) {
	ctx := context.Background()
	c := NewClient(newLocalFS(t), "sys/authdb")
	if _, err := c.WriteLTXFile(ctx, 0, 1, 5, bytes.NewReader(ltxPayload(64))); err != nil {
		t.Fatal(err)
	}
	infos := []*ltx.FileInfo{
		{Level: 0, MinTXID: 1, MaxTXID: 5},
		{Level: 0, MinTXID: 90, MaxTXID: 99}, // never existed — must not error
	}
	if err := c.DeleteLTXFiles(ctx, infos); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteLTXFiles(ctx, infos); err != nil { // repeat — idempotent
		t.Fatal(err)
	}
	if _, err := c.OpenLTXFile(ctx, 0, 1, 5, 0, 0); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want gone, got %v", err)
	}
}

func TestClient_DeleteAll(t *testing.T) {
	ctx := context.Background()
	c := NewClient(newLocalFS(t), "sys/authdb")
	for lvl := 0; lvl < 3; lvl++ {
		if _, err := c.WriteLTXFile(ctx, lvl, 1, 5, bytes.NewReader(ltxPayload(64))); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.DeleteAll(ctx); err != nil {
		t.Fatal(err)
	}
	itr, err := c.LTXFiles(ctx, 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if itr.Next() {
		t.Fatal("expected no files after DeleteAll")
	}
	itr.Close()
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test ./internal/authreplica/ -run TestClient -v 2>&1 | head -20
```

Expected: compile error — `NewClient` undefined.

- [ ] **Step 3: Implement the client**

`internal/authreplica/client.go`:

```go
// Package authreplica embeds Litestream to continuously replicate the
// sqlite authdb into a storage.ObjectStore and restore it on boot.
package authreplica

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/superfly/ltx"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// ReplicaClientType is reported by Type() and appears in litestream logs.
const ReplicaClientType = "bucketvcs-objectstore"

// DefaultPrefix is the reserved top-level key prefix used by --auth-db-replica=auto.
// Repo data lives entirely under "tenants/"; GC mark/sweep never lists outside it.
const DefaultPrefix = "sys/authdb"

// casRetryLimit bounds the Head+CAS loops so adapter misbehavior cannot hang
// replication forever.
const casRetryLimit = 8

// Client implements litestream.ReplicaClient on top of a storage.ObjectStore.
// Key layout mirrors litestream's file backend:
//
//	<prefix>/ltx/<level>/<ltx.FormatFilename(minTXID, maxTXID)>
type Client struct {
	store  storage.ObjectStore
	prefix string
	logger *slog.Logger
}

// NewClient returns a Client writing under prefix (no trailing slash needed).
func NewClient(store storage.ObjectStore, prefix string) *Client {
	return &Client{
		store:  store,
		prefix: strings.Trim(prefix, "/"),
		logger: slog.Default(),
	}
}

// Type implements litestream.ReplicaClient.
func (c *Client) Type() string { return ReplicaClientType }

// Init implements litestream.ReplicaClient. Object stores need no setup.
func (c *Client) Init(ctx context.Context) error { return nil }

// SetLogger implements litestream.ReplicaClient.
func (c *Client) SetLogger(l *slog.Logger) {
	if l != nil {
		c.logger = l
	}
}

func (c *Client) levelDir(level int) string {
	return path.Join(c.prefix, "ltx", strconv.Itoa(level))
}

func (c *Client) ltxKey(level int, minTXID, maxTXID ltx.TXID) string {
	return path.Join(c.levelDir(level), ltx.FormatFilename(minTXID, maxTXID))
}

// LTXFiles implements litestream.ReplicaClient. useMetadata is accepted but
// we always serve timestamps from object ModifiedAt: object stores give us no
// mtime control, so header-accurate timestamps would need a per-object GET.
// The skew vs the LTX header timestamp is bounded by the sync interval (~1s).
func (c *Client) LTXFiles(ctx context.Context, level int, seek ltx.TXID, useMetadata bool) (ltx.FileIterator, error) {
	prefix := c.levelDir(level) + "/"
	var infos []*ltx.FileInfo
	var token string
	for {
		page, err := c.store.List(ctx, prefix, &storage.ListOptions{ContinuationToken: token})
		if err != nil {
			return nil, fmt.Errorf("authreplica: list %s: %w", prefix, err)
		}
		for _, om := range page.Objects {
			minTXID, maxTXID, err := ltx.ParseFilename(path.Base(om.Key))
			if err != nil {
				continue // foreign object (e.g. localfs .meta sidecar exposure); skip
			}
			if minTXID < seek {
				continue
			}
			infos = append(infos, &ltx.FileInfo{
				Level:     level,
				MinTXID:   minTXID,
				MaxTXID:   maxTXID,
				Size:      om.Size,
				CreatedAt: om.ModifiedAt.UTC(),
			})
		}
		if page.NextToken == "" {
			break
		}
		token = page.NextToken
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].MinTXID < infos[j].MinTXID })
	return ltx.NewFileInfoSliceIterator(infos), nil
}

// OpenLTXFile implements litestream.ReplicaClient.
func (c *Client) OpenLTXFile(ctx context.Context, level int, minTXID, maxTXID ltx.TXID, offset, size int64) (io.ReadCloser, error) {
	key := c.ltxKey(level, minTXID, maxTXID)
	if offset == 0 && size == 0 {
		obj, err := c.store.Get(ctx, key, nil)
		if errors.Is(err, storage.ErrNotFound) {
			return nil, os.ErrNotExist
		}
		if err != nil {
			return nil, err
		}
		return obj.Body, nil
	}
	end := offset + size - 1
	if size <= 0 { // offset to end-of-file
		meta, err := c.store.Head(ctx, key)
		if errors.Is(err, storage.ErrNotFound) {
			return nil, os.ErrNotExist
		}
		if err != nil {
			return nil, err
		}
		end = meta.Size - 1
	}
	rc, err := c.store.GetRange(ctx, key, offset, end)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, os.ErrNotExist
	}
	return rc, err
}

// WriteLTXFile implements litestream.ReplicaClient. The body is buffered in
// memory: the authdb is small (MBs) and ObjectStore CAS retries need a
// re-readable source. CreatedAt comes from the LTX header timestamp when it
// parses (mirrors the file backend); otherwise falls back to now.
func (c *Client) WriteLTXFile(ctx context.Context, level int, minTXID, maxTXID ltx.TXID, r io.Reader) (*ltx.FileInfo, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("authreplica: read ltx body: %w", err)
	}
	createdAt := time.Now().UTC()
	if hdr, _, err := ltx.PeekHeader(bytes.NewReader(b)); err == nil {
		createdAt = time.UnixMilli(hdr.Timestamp).UTC()
	}
	key := c.ltxKey(level, minTXID, maxTXID)
	if err := c.putUnconditional(ctx, key, b); err != nil {
		return nil, err
	}
	return &ltx.FileInfo{
		Level:     level,
		MinTXID:   minTXID,
		MaxTXID:   maxTXID,
		Size:      int64(len(b)),
		CreatedAt: createdAt,
	}, nil
}

// putUnconditional synthesizes last-writer-wins PUT semantics (what
// litestream's native clients have) from our CAS primitives.
func (c *Client) putUnconditional(ctx context.Context, key string, b []byte) error {
	_, err := c.store.PutIfAbsent(ctx, key, bytes.NewReader(b), nil)
	if err == nil || !errors.Is(err, storage.ErrAlreadyExists) {
		return err
	}
	for i := 0; i < casRetryLimit; i++ {
		meta, err := c.store.Head(ctx, key)
		if errors.Is(err, storage.ErrNotFound) {
			if _, err := c.store.PutIfAbsent(ctx, key, bytes.NewReader(b), nil); err == nil {
				return nil
			} else if !errors.Is(err, storage.ErrAlreadyExists) {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		_, err = c.store.PutIfVersionMatches(ctx, key, meta.Version, bytes.NewReader(b), nil)
		if err == nil {
			return nil
		}
		if !errors.Is(err, storage.ErrVersionMismatch) && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
	}
	return fmt.Errorf("authreplica: put %s: CAS retry limit (%d) exceeded", key, casRetryLimit)
}

// DeleteLTXFiles implements litestream.ReplicaClient. Per-key conditional
// deletes (never a batch-delete API) — sidesteps the documented R2
// DeleteObjects silent-failure bug. Missing files are not an error.
func (c *Client) DeleteLTXFiles(ctx context.Context, a []*ltx.FileInfo) error {
	for _, info := range a {
		key := c.ltxKey(info.Level, info.MinTXID, info.MaxTXID)
		if err := c.deleteKey(ctx, key); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) deleteKey(ctx context.Context, key string) error {
	for i := 0; i < casRetryLimit; i++ {
		meta, err := c.store.Head(ctx, key)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		err = c.store.DeleteIfVersionMatches(ctx, key, meta.Version)
		if err == nil || errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if !errors.Is(err, storage.ErrVersionMismatch) {
			return err
		}
	}
	return fmt.Errorf("authreplica: delete %s: CAS retry limit (%d) exceeded", key, casRetryLimit)
}

// DeleteAll implements litestream.ReplicaClient (tests/reset only).
func (c *Client) DeleteAll(ctx context.Context) error {
	prefix := c.prefix + "/"
	var token string
	for {
		page, err := c.store.List(ctx, prefix, &storage.ListOptions{ContinuationToken: token})
		if err != nil {
			return err
		}
		for _, om := range page.Objects {
			if err := c.store.DeleteIfVersionMatches(ctx, om.Key, om.Version); err != nil &&
				!errors.Is(err, storage.ErrNotFound) {
				if errors.Is(err, storage.ErrVersionMismatch) {
					if err := c.deleteKey(ctx, om.Key); err != nil {
						return err
					}
					continue
				}
				return err
			}
		}
		if page.NextToken == "" {
			return nil
		}
		token = page.NextToken
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test ./internal/authreplica/ -run TestClient -v
```

Expected: all PASS. If `ltx.PeekHeader` / `ltx.NewFileInfoSliceIterator` / `ltx.FileInfo` field names differ from the above, check `go doc github.com/superfly/ltx FileInfo` etc. and adjust — the shapes here come from litestream v0.5.11's file backend source.

- [ ] **Step 5: Verify interface compliance compiles**

Add to the bottom of `client.go`:

```go
// Compile-time interface check.
var _ litestream.ReplicaClient = (*Client)(nil)
```

with import `"github.com/benbjohnson/litestream"`. Run `go build ./internal/authreplica/`. Expected: OK.

- [ ] **Step 6: Commit**

```bash
git add internal/authreplica/
git commit -m "feat(authreplica): litestream ReplicaClient over storage.ObjectStore"
```

---

### Task 3: Conformance suite across all four backends

**Files:**
- Create: `internal/authreplica/conformance_test.go`

The litestream harness is `package litestream_test` and not importable; this file adapts its scenarios (Apache-2.0). It reuses the env-gating convention from `internal/storage/*/\*_conformance_test.go` (`BUCKETVCS_S3_BUCKET`, `BUCKETVCS_GCS_BUCKET`, `BUCKETVCS_AZURE_CONTAINER`).

- [ ] **Step 1: Write the conformance test**

`internal/authreplica/conformance_test.go`:

```go
package authreplica

// Conformance scenarios adapted from litestream's replica_client_test.go
// (Apache-2.0, github.com/benbjohnson/litestream) — that harness lives in
// package litestream_test and cannot be imported. Re-run this file when
// bumping the pinned litestream version.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/superfly/ltx"

	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/azureblob"
	"github.com/bucketvcs/bucketvcs/internal/storage/gcs"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
	"github.com/bucketvcs/bucketvcs/internal/storage/s3compat"
)

// eachBackend runs fn against every reachable backend. localfs always runs;
// cloud backends skip unless their conformance env vars are set (same
// convention as internal/storage/*/*_conformance_test.go).
func eachBackend(t *testing.T, fn func(t *testing.T, store storage.ObjectStore)) {
	t.Run("localfs", func(t *testing.T) {
		s, err := localfs.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		fn(t, s)
	})
	t.Run("s3compat", func(t *testing.T) {
		bucket := os.Getenv("BUCKETVCS_S3_BUCKET")
		if bucket == "" {
			t.Skip("BUCKETVCS_S3_BUCKET unset — skipping live S3 conformance")
		}
		s, err := s3compat.Open(context.Background(), s3compat.Config{
			Bucket:   bucket,
			Endpoint: os.Getenv("BUCKETVCS_S3_ENDPOINT"),
		})
		if err != nil {
			t.Fatal(err)
		}
		fn(t, s)
	})
	t.Run("gcs", func(t *testing.T) {
		bucket := os.Getenv("BUCKETVCS_GCS_BUCKET")
		if bucket == "" {
			t.Skip("BUCKETVCS_GCS_BUCKET unset — skipping live GCS conformance")
		}
		s, err := gcs.Open(context.Background(), gcs.Config{
			Bucket:          bucket,
			Endpoint:        os.Getenv("BUCKETVCS_GCS_ENDPOINT"),
			CredentialsFile: os.Getenv("BUCKETVCS_GCS_CREDENTIALS_FILE"),
		})
		if err != nil {
			t.Fatal(err)
		}
		fn(t, s)
	})
	t.Run("azureblob", func(t *testing.T) {
		cont := os.Getenv("BUCKETVCS_AZURE_CONTAINER")
		if cont == "" {
			t.Skip("BUCKETVCS_AZURE_CONTAINER unset — skipping live azureblob conformance")
		}
		s, err := azureblob.Open(context.Background(), azureblob.Config{
			Container:        cont,
			Account:          os.Getenv("BUCKETVCS_AZURE_ACCOUNT"),
			AccountKey:       os.Getenv("BUCKETVCS_AZURE_ACCOUNT_KEY"),
			ConnectionString: os.Getenv("BUCKETVCS_AZURE_CONNECTION_STRING"),
			ServiceURL:       os.Getenv("BUCKETVCS_AZURE_SERVICE_URL"),
		})
		if err != nil {
			t.Fatal(err)
		}
		fn(t, s)
	})
}

// newConformanceClient gives each test run a unique prefix so live-bucket
// runs do not collide, and cleans up after itself.
func newConformanceClient(t *testing.T, store storage.ObjectStore) *Client {
	t.Helper()
	c := NewClient(store, fmt.Sprintf("sys/authdb-conformance/%s", t.Name()))
	t.Cleanup(func() { _ = c.DeleteAll(context.Background()) })
	return c
}

func TestConformance_WriteThenList(t *testing.T) {
	eachBackend(t, func(t *testing.T, store storage.ObjectStore) {
		ctx := context.Background()
		c := newConformanceClient(t, store)
		for _, r := range [][2]ltx.TXID{{1, 1}, {2, 4}, {5, 5}} {
			if _, err := c.WriteLTXFile(ctx, 0, r[0], r[1], bytes.NewReader(ltxPayload(512))); err != nil {
				t.Fatal(err)
			}
		}
		itr, err := c.LTXFiles(ctx, 0, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		defer itr.Close()
		var n int
		var last ltx.TXID
		for itr.Next() {
			item := itr.Item()
			if item.MinTXID < last {
				t.Fatalf("iterator out of order: %d after %d", item.MinTXID, last)
			}
			last = item.MinTXID
			if item.Size != 512 {
				t.Fatalf("size mismatch: %d", item.Size)
			}
			if item.CreatedAt.IsZero() {
				t.Fatal("zero CreatedAt")
			}
			n++
		}
		if n != 3 {
			t.Fatalf("want 3 files, got %d", n)
		}
	})
}

func TestConformance_OpenMissingIsNotExist(t *testing.T) {
	eachBackend(t, func(t *testing.T, store storage.ObjectStore) {
		c := newConformanceClient(t, store)
		_, err := c.OpenLTXFile(context.Background(), 0, 1000, 1001, 0, 0)
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("want os.ErrNotExist, got %v", err)
		}
	})
}

func TestConformance_RangedRead(t *testing.T) {
	eachBackend(t, func(t *testing.T, store storage.ObjectStore) {
		ctx := context.Background()
		c := newConformanceClient(t, store)
		body := ltxPayload(2048)
		if _, err := c.WriteLTXFile(ctx, 2, 7, 9, bytes.NewReader(body)); err != nil {
			t.Fatal(err)
		}
		rc, err := c.OpenLTXFile(ctx, 2, 7, 9, 1024, 512)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if !bytes.Equal(got, body[1024:1536]) {
			t.Fatalf("range read mismatch (%d bytes)", len(got))
		}
	})
}

func TestConformance_DeleteAndDeleteAll(t *testing.T) {
	eachBackend(t, func(t *testing.T, store storage.ObjectStore) {
		ctx := context.Background()
		c := newConformanceClient(t, store)
		info, err := c.WriteLTXFile(ctx, 0, 1, 2, bytes.NewReader(ltxPayload(64)))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.DeleteLTXFiles(ctx, []*ltx.FileInfo{info}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.OpenLTXFile(ctx, 0, 1, 2, 0, 0); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("want deleted, got %v", err)
		}
		if _, err := c.WriteLTXFile(ctx, 1, 3, 4, bytes.NewReader(ltxPayload(64))); err != nil {
			t.Fatal(err)
		}
		if err := c.DeleteAll(ctx); err != nil {
			t.Fatal(err)
		}
		itr, err := c.LTXFiles(ctx, 1, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		defer itr.Close()
		if itr.Next() {
			t.Fatal("files remain after DeleteAll")
		}
	})
}
```

- [ ] **Step 2: Run (localfs path; cloud subtests skip)**

```bash
go test ./internal/authreplica/ -run TestConformance -v 2>&1 | tail -20
```

Expected: localfs subtests PASS; s3compat/gcs/azureblob SKIP. If the `s3compat.Open`/`gcs.Open`/`azureblob.Open` signatures or Config field names differ, copy the exact invocation from the corresponding `internal/storage/<adapter>/*_conformance_test.go` — that file is the source of truth.

- [ ] **Step 3 (optional, if MinIO is running locally): live S3 pass**

```bash
BUCKETVCS_S3_BUCKET=<bucket> BUCKETVCS_S3_ENDPOINT=http://127.0.0.1:9000 \
  go test ./internal/authreplica/ -run TestConformance -v 2>&1 | tail -20
```

- [ ] **Step 4: Commit**

```bash
git add internal/authreplica/conformance_test.go
git commit -m "test(authreplica): backend conformance suite adapted from litestream harness"
```

---

### Task 4: `authreplica.Lease` — CAS split-brain guard

**Files:**
- Create: `internal/authreplica/lease.go`
- Test: `internal/authreplica/lease_test.go`

- [ ] **Step 1: Write the failing tests**

`internal/authreplica/lease_test.go`:

```go
package authreplica

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLease_AcquireFresh(t *testing.T) {
	l := NewLease(newLocalFS(t), "sys/authdb", time.Minute)
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l.InstanceID() == "" {
		t.Fatal("empty instance id")
	}
}

func TestLease_SecondAcquireFailsWhileLive(t *testing.T) {
	store := newLocalFS(t)
	a := NewLease(store, "sys/authdb", time.Minute)
	if err := a.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	b := NewLease(store, "sys/authdb", time.Minute)
	err := b.Acquire(context.Background())
	if !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("want ErrLeaseHeld, got %v", err)
	}
	// Holder must be named for the operator.
	if !contains(err.Error(), a.InstanceID()) {
		t.Fatalf("error does not name holder: %v", err)
	}
}

func TestLease_TakeoverAfterExpiry(t *testing.T) {
	store := newLocalFS(t)
	a := NewLease(store, "sys/authdb", time.Minute)
	if err := a.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	b := NewLease(store, "sys/authdb", time.Minute)
	b.now = func() time.Time { return time.Now().Add(2 * time.Minute) } // a's lease expired
	if err := b.Acquire(context.Background()); err != nil {
		t.Fatalf("takeover failed: %v", err)
	}
}

func TestLease_RenewAndLoss(t *testing.T) {
	store := newLocalFS(t)
	a := NewLease(store, "sys/authdb", time.Minute)
	if err := a.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.Renew(context.Background()); err != nil {
		t.Fatal(err)
	}

	// b steals after expiry; a's next renew must report loss.
	b := NewLease(store, "sys/authdb", time.Minute)
	b.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if err := b.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.Renew(context.Background()); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("want ErrLeaseLost, got %v", err)
	}
}

func TestLease_ReleaseAllowsReacquire(t *testing.T) {
	store := newLocalFS(t)
	a := NewLease(store, "sys/authdb", time.Minute)
	if err := a.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	b := NewLease(store, "sys/authdb", time.Minute)
	if err := b.Acquire(context.Background()); err != nil {
		t.Fatalf("reacquire after release failed: %v", err)
	}
}

func contains(s, sub string) bool {
	return sub != "" && len(s) >= len(sub) && (s == sub || len(s) > len(sub) && (s[:len(sub)] == sub || contains(s[1:], sub)))
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test ./internal/authreplica/ -run TestLease -v 2>&1 | head -10
```

Expected: compile error — `NewLease` undefined.

- [ ] **Step 3: Implement the lease**

`internal/authreplica/lease.go`:

```go
package authreplica

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// ErrLeaseHeld is returned by Acquire when another live instance holds the lease.
var ErrLeaseHeld = errors.New("authreplica: lease held by another instance")

// ErrLeaseLost is returned by Renew when the lease was taken over.
var ErrLeaseLost = errors.New("authreplica: lease lost to another instance")

// leaseDoc is the JSON body of <prefix>/lease.json.
type leaseDoc struct {
	InstanceID string    `json:"instance_id"`
	Hostname   string    `json:"hostname"`
	PID        int       `json:"pid"`
	RenewedAt  time.Time `json:"renewed_at"`
	TTLSeconds int64     `json:"ttl_s"`
}

// Lease is a CAS lease over a single object. It protects the replica
// lineage from concurrent writers (split-brain), not the local DB.
type Lease struct {
	store storage.ObjectStore
	key   string
	ttl   time.Duration
	now   func() time.Time // test seam

	id  string
	ver storage.ObjectVersion
}

// NewLease returns an unacquired lease at <prefix>/lease.json.
func NewLease(store storage.ObjectStore, prefix string, ttl time.Duration) *Lease {
	idb := make([]byte, 16)
	_, _ = rand.Read(idb)
	return &Lease{
		store: store,
		key:   path.Join(prefix, "lease.json"),
		ttl:   ttl,
		now:   time.Now,
		id:    hex.EncodeToString(idb),
	}
}

// InstanceID returns this instance's random identity.
func (l *Lease) InstanceID() string { return l.id }

func (l *Lease) body() ([]byte, error) {
	host, _ := os.Hostname()
	return json.Marshal(leaseDoc{
		InstanceID: l.id,
		Hostname:   host,
		PID:        os.Getpid(),
		RenewedAt:  l.now().UTC(),
		TTLSeconds: int64(l.ttl / time.Second),
	})
}

// Acquire claims the lease: PutIfAbsent, or CAS takeover if the current
// holder's lease has expired. A live holder yields ErrLeaseHeld naming it.
func (l *Lease) Acquire(ctx context.Context) error {
	b, err := l.body()
	if err != nil {
		return err
	}
	ver, err := l.store.PutIfAbsent(ctx, l.key, bytes.NewReader(b), nil)
	if err == nil {
		l.ver = ver
		return nil
	}
	if !errors.Is(err, storage.ErrAlreadyExists) {
		return fmt.Errorf("authreplica: acquire lease: %w", err)
	}

	obj, err := l.store.Get(ctx, l.key, nil)
	if errors.Is(err, storage.ErrNotFound) {
		return l.Acquire(ctx) // released between PutIfAbsent and Get; retry once via recursion
	}
	if err != nil {
		return fmt.Errorf("authreplica: read lease: %w", err)
	}
	raw, readErr := io.ReadAll(obj.Body)
	obj.Body.Close()
	if readErr != nil {
		return fmt.Errorf("authreplica: read lease: %w", readErr)
	}
	var doc leaseDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("authreplica: parse lease %s: %w", l.key, err)
	}

	expiry := doc.RenewedAt.Add(time.Duration(doc.TTLSeconds) * time.Second)
	if l.now().Before(expiry) {
		return fmt.Errorf("%w: instance=%s host=%s pid=%d renewed_at=%s",
			ErrLeaseHeld, doc.InstanceID, doc.Hostname, doc.PID, doc.RenewedAt.Format(time.RFC3339))
	}

	// Expired — take over via CAS on the stale version.
	ver, err = l.store.PutIfVersionMatches(ctx, l.key, obj.Metadata.Version, bytes.NewReader(b), nil)
	if err != nil {
		if errors.Is(err, storage.ErrVersionMismatch) || errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("%w: lost takeover race", ErrLeaseHeld)
		}
		return fmt.Errorf("authreplica: lease takeover: %w", err)
	}
	l.ver = ver
	return nil
}

// Renew refreshes RenewedAt via CAS on our held version. ErrLeaseLost means
// another instance took over — the caller must stop replicating.
func (l *Lease) Renew(ctx context.Context) error {
	b, err := l.body()
	if err != nil {
		return err
	}
	ver, err := l.store.PutIfVersionMatches(ctx, l.key, l.ver, bytes.NewReader(b), nil)
	if err == nil {
		l.ver = ver
		return nil
	}
	if errors.Is(err, storage.ErrVersionMismatch) || errors.Is(err, storage.ErrNotFound) {
		return ErrLeaseLost
	}
	return fmt.Errorf("authreplica: renew lease: %w", err)
}

// Release deletes the lease if we still hold it; losing it is not an error.
func (l *Lease) Release(ctx context.Context) error {
	err := l.store.DeleteIfVersionMatches(ctx, l.key, l.ver)
	if err == nil || errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrVersionMismatch) {
		return nil
	}
	return fmt.Errorf("authreplica: release lease: %w", err)
}
```

NOTE: if `obj.Metadata.Version` is not how `storage.Object` exposes its version (check `internal/storage/objectstore.go`'s `Object` struct — it may be `obj.Version` or similar), adjust the one access in `Acquire`.

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test ./internal/authreplica/ -run TestLease -v
```

Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/authreplica/lease.go internal/authreplica/lease_test.go
git commit -m "feat(authreplica): CAS lease split-brain guard"
```

---

### Task 5: `authreplica.Runner` — lifecycle glue + restore round-trip

**Files:**
- Create: `internal/authreplica/runner.go`
- Test: `internal/authreplica/runner_test.go`

- [ ] **Step 1: Write the failing integration test**

`internal/authreplica/runner_test.go`:

```go
package authreplica

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// openSQL opens the test sqlite DB the same way sqlitestore does (WAL + busy timeout).
func openSQL(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	return db
}

// TestRunner_ReplicateDeleteRestore is the load-bearing integration test:
// real litestream, real LTX files, full replicate → wipe → restore cycle.
func TestRunner_ReplicateDeleteRestore(t *testing.T) {
	ctx := context.Background()
	store := newLocalFS(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "auth.db")

	cfg := Config{DBPath: dbPath, Store: store, Prefix: DefaultPrefix, LeaseTTL: time.Minute}

	// Phase 1: prepare (no backup yet → restore is a no-op), open DB, replicate.
	r, err := Prepare(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	db := openSQL(t, dbPath)
	if _, err := db.ExecContext(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO t (v) VALUES ('alpha'), ('beta')`); err != nil {
		t.Fatal(err)
	}
	if err := r.StartReplication(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.SyncNow(ctx); err != nil { // deterministic flush to the store
		t.Fatal(err)
	}
	db.Close()
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// Simulate total disk loss: remove the DB and every litestream artifact.
	matches, _ := filepath.Glob(dbPath + "*")
	for _, m := range matches {
		if err := os.RemoveAll(m); err != nil {
			t.Fatal(err)
		}
	}

	// Phase 2: a fresh boot restores from the bucket.
	r2, err := Prepare(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close(ctx)
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("restore did not recreate db: %v", err)
	}
	db2 := openSQL(t, dbPath)
	defer db2.Close()
	var n int
	if err := db2.QueryRowContext(ctx, `SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("want 2 rows after restore, got %d", n)
	}
}

func TestRunner_PrepareFailsWhenLeaseHeld(t *testing.T) {
	ctx := context.Background()
	store := newLocalFS(t)
	dir := t.TempDir()
	cfg := Config{DBPath: filepath.Join(dir, "a.db"), Store: store, Prefix: DefaultPrefix, LeaseTTL: time.Minute}

	r1, err := Prepare(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r1.Close(ctx)

	cfg2 := cfg
	cfg2.DBPath = filepath.Join(dir, "b.db")
	if _, err := Prepare(ctx, cfg2); err == nil {
		t.Fatal("second Prepare should fail while lease held")
	}
}

func TestRunner_SkipRestore(t *testing.T) {
	ctx := context.Background()
	store := newLocalFS(t)
	dbPath := filepath.Join(t.TempDir(), "a.db")
	cfg := Config{DBPath: dbPath, Store: store, Prefix: DefaultPrefix, LeaseTTL: time.Minute, SkipRestore: true}
	r, err := Prepare(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(ctx)
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatal("SkipRestore must not create/restore the db")
	}
}

var _ = storage.ErrNotFound // keep storage import if otherwise unused
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test ./internal/authreplica/ -run TestRunner -v 2>&1 | head -10
```

Expected: compile error — `Config`/`Prepare` undefined.

- [ ] **Step 3: Implement the runner**

`internal/authreplica/runner.go`:

```go
package authreplica

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/benbjohnson/litestream"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// DefaultLeaseTTL is the lease validity window; renewal runs every TTL/3.
const DefaultLeaseTTL = 60 * time.Second

// Config configures authdb replication.
type Config struct {
	DBPath      string
	Store       storage.ObjectStore
	Prefix      string        // defaults to DefaultPrefix
	LeaseTTL    time.Duration // defaults to DefaultLeaseTTL
	SkipRestore bool          // operator escape hatch: --auth-db-replica-skip-restore
	Logger      *slog.Logger
}

// Runner ties the lease, restore-on-boot, and litestream replication to the
// serve lifecycle. Two phases, matching serve's boot order:
//
//	r, err := Prepare(ctx, cfg)   // lease + restore-iff-missing; BEFORE sqlitestore.Open
//	...open authdb...
//	err = r.StartReplication(ctx) // litestream store + heartbeat;  AFTER sqlitestore.Open
//	...
//	r.Close(ctx)                  // shutdown sync → litestream close → lease release
type Runner struct {
	cfg    Config
	logger *slog.Logger
	lease  *Lease
	client *Client

	mu      sync.Mutex
	lsdb    *litestream.DB
	lsstore *litestream.Store
	stopped bool

	hbCancel context.CancelFunc
	hbDone   chan struct{}
}

// Prepare acquires the lease and restores the DB from the replica iff the
// local file is missing. Restore failures are fatal by design (fail-closed):
// booting with an empty authdb while a replica exists would fork history.
func Prepare(ctx context.Context, cfg Config) (*Runner, error) {
	if cfg.Prefix == "" {
		cfg.Prefix = DefaultPrefix
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = DefaultLeaseTTL
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With(slog.String("subsystem", "authreplica"))

	r := &Runner{
		cfg:    cfg,
		logger: logger,
		lease:  NewLease(cfg.Store, cfg.Prefix, cfg.LeaseTTL),
		client: NewClient(cfg.Store, cfg.Prefix),
	}
	r.client.SetLogger(slog.New(newLevelFilterHandler(logger.Handler(), slog.LevelWarn)))

	if err := r.lease.Acquire(ctx); err != nil {
		return nil, err
	}

	lsdb := litestream.NewDB(cfg.DBPath)
	lsdb.Logger = slog.New(newLevelFilterHandler(logger.Handler(), slog.LevelWarn))
	lsdb.Replica = litestream.NewReplicaWithClient(lsdb, r.client)
	r.lsdb = lsdb

	if !cfg.SkipRestore {
		start := time.Now()
		if err := lsdb.EnsureExists(ctx); err != nil {
			_ = r.lease.Release(ctx)
			return nil, fmt.Errorf("authreplica: restore-on-boot: %w "+
				"(refusing to start with an empty authdb while a replica may exist; "+
				"use --auth-db-replica-skip-restore to override)", err)
		}
		logger.LogAttrs(ctx, slog.LevelInfo, "authdb.replica.restored",
			slog.Bool("audit", true),
			slog.String("event", "authdb.replica.restored"),
			slog.String("db_path", cfg.DBPath),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
		)
	}
	return r, nil
}

// StartReplication opens the litestream store (replication + compaction) and
// the lease heartbeat. Call after the authdb file exists (sqlitestore.Open).
func (r *Runner) StartReplication(ctx context.Context) error {
	levels := litestream.CompactionLevels{
		{Level: 0},
		{Level: 1, Interval: 30 * time.Second},
		{Level: 2, Interval: 5 * time.Minute},
	}
	st := litestream.NewStore([]*litestream.DB{r.lsdb}, levels)
	st.Logger = slog.New(newLevelFilterHandler(r.logger.Handler(), slog.LevelWarn))
	if err := st.Open(ctx); err != nil {
		return fmt.Errorf("authreplica: open litestream store: %w", err)
	}
	r.mu.Lock()
	r.lsstore = st
	r.mu.Unlock()

	hbCtx, cancel := context.WithCancel(context.Background())
	r.hbCancel = cancel
	r.hbDone = make(chan struct{})
	go r.heartbeat(hbCtx)
	r.logger.Info("authdb replication started",
		slog.String("backend", r.cfg.Store.Name()), slog.String("prefix", r.cfg.Prefix))
	return nil
}

// heartbeat renews the lease every TTL/3. Lease loss stops replication but
// NOT the server: the lease protects the replica lineage, not the local DB.
func (r *Runner) heartbeat(ctx context.Context) {
	defer close(r.hbDone)
	t := time.NewTicker(r.cfg.LeaseTTL / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			err := r.lease.Renew(ctx)
			if err == nil {
				continue
			}
			if errors.Is(err, ErrLeaseLost) {
				r.logger.LogAttrs(ctx, slog.LevelError, "authdb.replica.lease_lost",
					slog.Bool("audit", true),
					slog.String("event", "authdb.replica.lease_lost"),
					slog.String("instance_id", r.lease.InstanceID()),
				)
				r.stopReplication(ctx)
				return
			}
			r.logger.LogAttrs(ctx, slog.LevelInfo, "metric",
				slog.String("metric_name", "authdb_replica_lease_renew_errors_total"),
				slog.Int64("value", 1))
			r.logger.Warn("lease renew failed (will retry)", slog.Any("error", err))
		}
	}
}

// stopReplication closes the litestream store without releasing the lease
// (we no longer own it). Safe to call once; Close handles the normal path.
func (r *Runner) stopReplication(ctx context.Context) {
	r.mu.Lock()
	st := r.lsstore
	r.lsstore = nil
	r.stopped = true
	r.mu.Unlock()
	if st == nil {
		return
	}
	if err := st.Close(ctx); err != nil {
		r.logger.Error("close litestream store after lease loss", slog.Any("error", err))
	}
	r.logger.LogAttrs(ctx, slog.LevelError, "authdb.replica.replication_stopped",
		slog.Bool("audit", true),
		slog.String("event", "authdb.replica.replication_stopped"))
}

// SyncNow forces a full WAL→LTX→store sync. Used by tests and shutdown.
func (r *Runner) SyncNow(ctx context.Context) error {
	r.mu.Lock()
	lsdb := r.lsdb
	r.mu.Unlock()
	if lsdb == nil {
		return nil
	}
	return lsdb.SyncAndWait(ctx)
}

// Close performs the ordered shutdown: heartbeat off → final sync via the
// litestream store close (bounded by its ShutdownSyncTimeout) → lease release.
func (r *Runner) Close(ctx context.Context) error {
	if r.hbCancel != nil {
		r.hbCancel()
		<-r.hbDone
		r.hbCancel = nil
	}
	r.mu.Lock()
	st := r.lsstore
	r.lsstore = nil
	lost := r.stopped
	r.mu.Unlock()

	var firstErr error
	if st != nil {
		if err := st.Close(ctx); err != nil {
			firstErr = fmt.Errorf("authreplica: close litestream store: %w", err)
		}
	}
	if !lost { // only release a lease we still own
		if err := r.lease.Release(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
```

Also create the small slog level filter in `internal/authreplica/loglevel.go`:

```go
package authreplica

import (
	"context"
	"log/slog"
)

// levelFilterHandler suppresses litestream's sub-WARN chatter while keeping
// its warnings/errors in our log stream.
type levelFilterHandler struct {
	inner slog.Handler
	min   slog.Level
}

func newLevelFilterHandler(inner slog.Handler, min slog.Level) slog.Handler {
	return &levelFilterHandler{inner: inner, min: min}
}

func (h *levelFilterHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= h.min && h.inner.Enabled(ctx, l)
}
func (h *levelFilterHandler) Handle(ctx context.Context, rec slog.Record) error {
	return h.inner.Handle(ctx, rec)
}
func (h *levelFilterHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &levelFilterHandler{inner: h.inner.WithAttrs(attrs), min: h.min}
}
func (h *levelFilterHandler) WithGroup(name string) slog.Handler {
	return &levelFilterHandler{inner: h.inner.WithGroup(name), min: h.min}
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test ./internal/authreplica/ -v 2>&1 | tail -20
```

Expected: all PASS, including the replicate→delete→restore round-trip. Debugging notes if the round-trip fails:
- `EnsureExists` restores only when the DB file is missing — confirm the glob removed `auth.db`, `auth.db-wal`, `auth.db-shm`, and any `auth.db-litestream` state directory.
- If `litestream.CompactionLevels` literal syntax fails to compile, check `go doc github.com/benbjohnson/litestream.CompactionLevel` — it may be `[]*CompactionLevel` requiring `&litestream.CompactionLevel{...}` elements.
- If `store.Open(ctx)` fails because the DB file does not exist yet in `TestRunner_PrepareFailsWhenLeaseHeld` (no replication started there), that's fine — that test never calls StartReplication.

- [ ] **Step 5: Run the full package + vet**

```bash
go vet ./internal/authreplica/ && go test ./internal/authreplica/
```

Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add internal/authreplica/
git commit -m "feat(authreplica): Runner lifecycle — lease, restore-on-boot, litestream store"
```

---

### Task 6: serve wiring — flags, validation, boot/shutdown

**Files:**
- Modify: `cmd/bucketvcs/serveflags.go` (flag declarations)
- Modify: `cmd/bucketvcs/serve.go` (validation + boot + shutdown)
- Test: `cmd/bucketvcs/authreplica_resolve_test.go`
- Create: `cmd/bucketvcs/authreplica_resolve.go`

- [ ] **Step 1: Write the failing test for target resolution + validation**

`cmd/bucketvcs/authreplica_resolve_test.go`:

```go
package main

import (
	"strings"
	"testing"
)

func TestResolveAuthDBReplica(t *testing.T) {
	cases := []struct {
		name      string
		replica   string // --auth-db-replica
		storeURL  string // --store
		authDB    string // --auth-db (DSN inference)
		isReplica bool   // M26 replica-serve mode
		wantErr   string // substring; "" = ok
		wantAuto  bool   // resolved to system store + DefaultPrefix
	}{
		{name: "off is nil", replica: "off", storeURL: "localfs:/tmp/x"},
		{name: "empty is nil", replica: "", storeURL: "localfs:/tmp/x"},
		{name: "auto ok", replica: "auto", storeURL: "localfs:/tmp/x", wantAuto: true},
		{name: "auto needs store", replica: "auto", storeURL: "", wantErr: "--store"},
		{name: "explicit url ok", replica: "localfs:/tmp/replica", storeURL: "localfs:/tmp/x"},
		{name: "postgres dsn rejected", replica: "auto", storeURL: "localfs:/tmp/x",
			authDB: "postgres://u@h/db", wantErr: "embedded sqlite"},
		{name: "libsql dsn rejected", replica: "auto", storeURL: "localfs:/tmp/x",
			authDB: "libsql://db.turso.io", wantErr: "embedded sqlite"},
		{name: "replica-serve rejected", replica: "auto", storeURL: "localfs:/tmp/x",
			isReplica: true, wantErr: "replica"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := resolveAuthDBReplica(tc.replica, tc.storeURL, tc.authDB, tc.isReplica)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.replica == "off" || tc.replica == "" {
				if spec != nil {
					t.Fatal("want nil spec when off")
				}
				return
			}
			if spec == nil {
				t.Fatal("want non-nil spec")
			}
			if tc.wantAuto && (spec.UseSystemStore != true || spec.Prefix == "") {
				t.Fatalf("auto not resolved: %+v", spec)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./cmd/bucketvcs/ -run TestResolveAuthDBReplica -v 2>&1 | head -5
```

Expected: compile error — `resolveAuthDBReplica` undefined.

- [ ] **Step 3: Implement the resolver**

`cmd/bucketvcs/authreplica_resolve.go`:

```go
package main

import (
	"errors"
	"net/url"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/authreplica"
)

// authDBReplicaSpec is the resolved --auth-db-replica configuration.
// UseSystemStore=true means replicate into the --store bucket; otherwise
// StoreURL names a dedicated location (parsed by openStore).
type authDBReplicaSpec struct {
	UseSystemStore bool
	StoreURL       string
	Prefix         string
}

// isNonSQLiteAuthDB mirrors sqlitestore's backend inference (resolveBackend):
// postgres/postgresql/libsql/http/https schemes select non-embedded backends.
func isNonSQLiteAuthDB(value string) bool {
	u, err := url.Parse(value)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "postgres", "postgresql", "libsql", "http", "https":
		return true
	}
	return false
}

// resolveAuthDBReplica validates and resolves the --auth-db-replica flag.
// Returns (nil, nil) when replication is off.
func resolveAuthDBReplica(replica, storeURL, authDB string, isReplicaServe bool) (*authDBReplicaSpec, error) {
	replica = strings.TrimSpace(replica)
	if replica == "" || replica == "off" {
		return nil, nil
	}
	if isNonSQLiteAuthDB(authDB) {
		return nil, errors.New("--auth-db-replica: replication is for the embedded sqlite backend; libsql/postgres bring their own durability")
	}
	if isReplicaServe {
		return nil, errors.New("--auth-db-replica: not allowed in replica-serve mode (--replica-of); only the primary replicates the authdb")
	}
	if replica == "auto" {
		if storeURL == "" {
			return nil, errors.New("--auth-db-replica=auto requires --store")
		}
		return &authDBReplicaSpec{UseSystemStore: true, Prefix: authreplica.DefaultPrefix}, nil
	}
	return &authDBReplicaSpec{StoreURL: replica, Prefix: authreplica.DefaultPrefix}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
go test ./cmd/bucketvcs/ -run TestResolveAuthDBReplica -v
```

Expected: PASS.

- [ ] **Step 5: Declare the serve flags**

In `cmd/bucketvcs/serveflags.go`, next to the existing auth-db flags, add to the `serveFlags` struct and `registerServeFlags`:

```go
// struct fields:
authDBReplica            *string
authDBReplicaLeaseTTL    *time.Duration
authDBReplicaSkipRestore *bool

// registrations:
sf.authDBReplica = fs.String("auth-db-replica", "",
	`Replicate the sqlite authdb via embedded Litestream: "auto" (sys/authdb/ in --store), a storage URL, or "off" (default)`)
sf.authDBReplicaLeaseTTL = fs.Duration("auth-db-replica-lease-ttl", authreplica.DefaultLeaseTTL,
	"Replication lease TTL; a second serve targeting the same prefix is refused until the holder's lease expires")
sf.authDBReplicaSkipRestore = fs.Bool("auth-db-replica-skip-restore", false,
	"Skip restore-on-boot when the local authdb file is missing (use only when the replica location is known-empty)")
```

(Match the exact field-naming/registration style already in the file; import `authreplica`.)

- [ ] **Step 6: Wire boot + shutdown in serve.go**

Locate in `cmd/bucketvcs/serve.go`:
- where the ObjectStore is opened (`openStore(*storeURL)` result, before the repo engine),
- the authdb open at ~serve.go:251 (`authS, _, err := openAuthDB(...)`),
- the `isReplica := *replicaOf != ""` flag at ~serve.go:171.

Insert, in order:

(a) **Validation** with the other early flag checks (style of serve.go:96-101):

```go
replicaSpec, err := resolveAuthDBReplica(*sf.authDBReplica, *sf.storeURL, *sf.authDB, isReplica)
if err != nil {
	fmt.Fprintf(stderr, "serve: %v\n", err)
	return 2
}
```

(b) **Prepare — immediately BEFORE the `openAuthDB` call** (the store must already be open; if the ObjectStore is currently opened after the authdb, move the authdb open later — `openAuthDB` has no dependency on anything between, verify with a quick read):

```go
var replRunner *authreplica.Runner
if replicaSpec != nil {
	replStore := store // the system ObjectStore already opened from --store
	if !replicaSpec.UseSystemStore {
		replStore, err = openStore(replicaSpec.StoreURL)
		if err != nil {
			fmt.Fprintf(stderr, "serve: --auth-db-replica: %v\n", err)
			return 1
		}
	}
	authDBPath, err := resolveAuthDB(*sf.authDB, realEnv())
	if err != nil {
		fmt.Fprintf(stderr, "serve: --auth-db-replica: %v\n", err)
		return 1
	}
	replRunner, err = authreplica.Prepare(serveCtx, authreplica.Config{
		DBPath:      authDBPath,
		Store:       replStore,
		Prefix:      replicaSpec.Prefix,
		LeaseTTL:    *sf.authDBReplicaLeaseTTL,
		SkipRestore: *sf.authDBReplicaSkipRestore,
		Logger:      slog.Default(),
	})
	if err != nil {
		fmt.Fprintf(stderr, "serve: auth-db replication: %v\n", err)
		return 1
	}
}
```

NOTE: if `serveCtx` is not yet created at this point (it is created at ~serve.go:482), use the function's base `ctx` for Prepare — Prepare is synchronous, no goroutines.

(c) **StartReplication — immediately AFTER `openAuthDB` succeeds** (the sqlite file now exists):

```go
if replRunner != nil {
	if err := replRunner.StartReplication(ctx); err != nil {
		fmt.Fprintf(stderr, "serve: auth-db replication: %v\n", err)
		return 1
	}
}
```

(d) **Shutdown** — register before the authdb close so ordering is: drain → litestream shutdown-sync/close → authdb close → lease release. Since `Runner.Close` does litestream-close + lease-release together and deferred functions run LIFO, place this defer immediately AFTER `defer authS.Close()`:

```go
if replRunner != nil {
	defer func() {
		shCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := replRunner.Close(shCtx); err != nil {
			slog.Default().Error("auth-db replication shutdown", slog.Any("error", err))
		}
	}()
}
```

Wait — LIFO means a defer registered after `defer authS.Close()` runs BEFORE it. That is exactly the required order (litestream store closes before the app's DB handle). Keep this placement.

- [ ] **Step 7: Build + run existing serve tests**

```bash
go build ./... && go test ./cmd/bucketvcs/ 2>&1 | tail -5
```

Expected: build OK, existing tests still pass.

- [ ] **Step 8: Manual end-to-end sanity**

```bash
TMP=$(mktemp -d)
./bin/bucketvcs 2>/dev/null || go build -o /tmp/bucketvcs ./cmd/bucketvcs
/tmp/bucketvcs serve --addr=127.0.0.1:0 --store=localfs:$TMP/store \
  --auth-db=$TMP/auth.db --auth-db-replica=auto &
sleep 2
ls $TMP/store/sys/authdb/ltx/0/ && echo REPLICATION_WRITES_OK
kill %1; wait
```

Expected: at least one `.ltx` object (plus localfs `.meta` sidecars) and `REPLICATION_WRITES_OK`. (Adjust serve's required flags per its current `--help` if it refuses to start with this minimal set.)

- [ ] **Step 9: Commit**

```bash
git add cmd/bucketvcs/
git commit -m "feat(serve): --auth-db-replica embedded litestream replication wiring"
```

---

### Task 7: `bucketvcs authdb restore` CLI

**Files:**
- Create: `cmd/bucketvcs/authdbcmd.go` (subcommand; named to avoid clashing with the existing helper file `authdb.go`)
- Modify: `cmd/bucketvcs/main.go` (dispatch + usage)
- Test: `cmd/bucketvcs/authdbcmd_test.go`

- [ ] **Step 1: Write the failing test**

`cmd/bucketvcs/authdbcmd_test.go`:

```go
package main

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/bucketvcs/bucketvcs/internal/authreplica"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// seedReplica replicates a tiny DB into a localfs store and returns its root.
func seedReplica(t *testing.T) (storeRoot, dbPath string) {
	t.Helper()
	ctx := context.Background()
	storeRoot = t.TempDir()
	st, err := localfs.Open(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	dbPath = filepath.Join(t.TempDir(), "auth.db")
	r, err := authreplica.Prepare(ctx, authreplica.Config{
		DBPath: dbPath, Store: st, Prefix: authreplica.DefaultPrefix, LeaseTTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `CREATE TABLE marker (v TEXT); INSERT INTO marker VALUES ('present')`); err != nil {
		t.Fatal(err)
	}
	if err := r.StartReplication(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.SyncNow(ctx); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	return storeRoot, dbPath
}

func TestAuthDBRestore_ToOutput(t *testing.T) {
	storeRoot, _ := seedReplica(t)
	out := filepath.Join(t.TempDir(), "restored.db")

	var stdout, stderr bytes.Buffer
	code := run(context.Background(),
		[]string{"authdb", "restore", "--replica=localfs:" + storeRoot, "--output=" + out},
		&stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	db, err := sql.Open("sqlite", "file:"+out)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v string
	if err := db.QueryRow(`SELECT v FROM marker`).Scan(&v); err != nil || v != "present" {
		t.Fatalf("restored db wrong: v=%q err=%v", v, err)
	}
}

func TestAuthDBRestore_RefusesOverwriteWithoutForce(t *testing.T) {
	storeRoot, _ := seedReplica(t)
	out := filepath.Join(t.TempDir(), "x.db")
	if err := os.WriteFile(out, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(),
		[]string{"authdb", "restore", "--replica=localfs:" + storeRoot, "--output=" + out},
		&stdout, &stderr)
	if code == 0 {
		t.Fatal("expected refusal without --force")
	}
	if !strings.Contains(stderr.String(), "--force") {
		t.Fatalf("error should mention --force: %s", stderr.String())
	}
	// --if-not-exists turns the same situation into a no-op success.
	code = run(context.Background(),
		[]string{"authdb", "restore", "--replica=localfs:" + storeRoot, "--output=" + out, "--if-not-exists"},
		&stdout, &stderr)
	if code != 0 {
		t.Fatalf("--if-not-exists should no-op, got %d: %s", code, stderr.String())
	}
}

func TestAuthDBReplicaStatus(t *testing.T) {
	storeRoot, _ := seedReplica(t)
	var stdout, stderr bytes.Buffer
	code := run(context.Background(),
		[]string{"authdb", "replica-status", "--replica=localfs:" + storeRoot},
		&stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"level":0`) {
		t.Fatalf("status missing level 0 line: %s", stdout.String())
	}
}
```

NOTE: if `run`'s signature differs (check `cmd/bucketvcs/main.go:24` — it may take an env or use variadic writers), match the existing CLI tests in the package (e.g. the policy CLI tests) for how to invoke commands.

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test ./cmd/bucketvcs/ -run 'TestAuthDB' -v 2>&1 | head -10
```

Expected: failures — `authdb` is an unknown subcommand.

- [ ] **Step 3: Implement the subcommand**

`cmd/bucketvcs/authdbcmd.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/benbjohnson/litestream"
	"github.com/superfly/ltx"

	"github.com/bucketvcs/bucketvcs/internal/authreplica"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

const authdbUsage = `usage: bucketvcs authdb <action> [flags]

actions:
  restore         restore the authdb from a replica (optionally point-in-time)
  replica-status  show replica levels, latest TXIDs and lease holder
`

func runAuthDBCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, authdbUsage)
		return 2
	}
	switch args[0] {
	case "restore":
		return runAuthDBRestore(ctx, args[1:], stdout, stderr)
	case "replica-status":
		return runAuthDBReplicaStatus(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "authdb: unknown action %q\n", args[0])
		fmt.Fprint(stderr, authdbUsage)
		return 2
	}
}

// openReplicaTarget resolves --replica/--store into (ObjectStore, prefix).
func openReplicaTarget(replica, storeURL string) (storage.ObjectStore, string, error) {
	switch strings.TrimSpace(replica) {
	case "", "off":
		return nil, "", fmt.Errorf("--replica is required (\"auto\" or a storage URL)")
	case "auto":
		if storeURL == "" {
			return nil, "", fmt.Errorf("--replica=auto requires --store")
		}
		st, err := openStore(storeURL)
		if err != nil {
			return nil, "", err
		}
		return st, authreplica.DefaultPrefix, nil
	default:
		st, err := openStore(replica)
		if err != nil {
			return nil, "", err
		}
		return st, authreplica.DefaultPrefix, nil
	}
}

func runAuthDBRestore(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("authdb restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	replica := fs.String("replica", "", `replica location: "auto" (with --store) or a storage URL`)
	storeURL := fs.String("store", "", "system store URL (with --replica=auto)")
	authDB := fs.String("auth-db", "", "target authdb path (default: standard resolution)")
	output := fs.String("output", "", "restore to this path instead of the authdb path")
	timestamp := fs.String("timestamp", "", "point-in-time upper bound (RFC3339)")
	txid := fs.String("txid", "", "TXID upper bound (hex)")
	ifNotExists := fs.Bool("if-not-exists", false, "exit 0 without restoring if the target exists")
	force := fs.Bool("force", false, "overwrite an existing target")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	target := *output
	if target == "" {
		p, err := resolveAuthDB(*authDB, realEnv())
		if err != nil {
			fmt.Fprintf(stderr, "authdb restore: %v\n", err)
			return 1
		}
		target = p
	}
	if _, err := os.Stat(target); err == nil {
		if *ifNotExists {
			fmt.Fprintf(stdout, "authdb restore: %s exists; nothing to do\n", target)
			return 0
		}
		if !*force {
			fmt.Fprintf(stderr, "authdb restore: %s exists; pass --force to overwrite or --if-not-exists to no-op\n", target)
			return 2
		}
		if err := os.Remove(target); err != nil {
			fmt.Fprintf(stderr, "authdb restore: %v\n", err)
			return 1
		}
	}

	st, prefix, err := openReplicaTarget(*replica, *storeURL)
	if err != nil {
		fmt.Fprintf(stderr, "authdb restore: %v\n", err)
		return 2
	}
	client := authreplica.NewClient(st, prefix)

	lsdb := litestream.NewDB(target)
	r := litestream.NewReplicaWithClient(lsdb, client)
	opt := litestream.NewRestoreOptions()
	opt.OutputPath = target
	if *timestamp != "" {
		ts, err := time.Parse(time.RFC3339, *timestamp)
		if err != nil {
			fmt.Fprintf(stderr, "authdb restore: --timestamp: %v\n", err)
			return 2
		}
		opt.Timestamp = ts
	}
	if *txid != "" {
		v, err := strconv.ParseUint(strings.TrimPrefix(*txid, "0x"), 16, 64)
		if err != nil {
			fmt.Fprintf(stderr, "authdb restore: --txid: %v\n", err)
			return 2
		}
		opt.TXID = ltx.TXID(v)
	}
	if err := r.Restore(ctx, opt); err != nil {
		fmt.Fprintf(stderr, "authdb restore: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "restored %s\n", target)
	return 0
}

func runAuthDBReplicaStatus(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("authdb replica-status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	replica := fs.String("replica", "", `replica location: "auto" (with --store) or a storage URL`)
	storeURL := fs.String("store", "", "system store URL (with --replica=auto)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	st, prefix, err := openReplicaTarget(*replica, *storeURL)
	if err != nil {
		fmt.Fprintf(stderr, "authdb replica-status: %v\n", err)
		return 2
	}
	client := authreplica.NewClient(st, prefix)

	enc := json.NewEncoder(stdout)
	for level := 0; level <= 9; level++ {
		itr, err := client.LTXFiles(ctx, level, 0, false)
		if err != nil {
			fmt.Fprintf(stderr, "authdb replica-status: level %d: %v\n", level, err)
			return 1
		}
		var n int
		var maxTXID ltx.TXID
		var latest time.Time
		var bytes int64
		for itr.Next() {
			it := itr.Item()
			n++
			bytes += it.Size
			if it.MaxTXID > maxTXID {
				maxTXID = it.MaxTXID
			}
			if it.CreatedAt.After(latest) {
				latest = it.CreatedAt
			}
		}
		itr.Close()
		if n == 0 {
			continue
		}
		_ = enc.Encode(map[string]any{
			"level": level, "files": n, "bytes": bytes,
			"max_txid": fmt.Sprintf("%016x", uint64(maxTXID)),
			"latest":   latest.UTC().Format(time.RFC3339),
		})
	}
	return 0
}
```

- [ ] **Step 4: Register the subcommand**

In `cmd/bucketvcs/main.go`'s `run` switch (alphabetical-ish with the others):

```go
case "authdb":
	return runAuthDBCmd(ctx, rest, stdout, stderr)
```

and add one line to the usage text following its existing format.

- [ ] **Step 5: Run tests to verify they pass**

```bash
go test ./cmd/bucketvcs/ -run 'TestAuthDB' -v
```

Expected: PASS. If `litestream.NewRestoreOptions()` field names differ (`OutputPath`/`TXID`/`Timestamp` were confirmed from v0.5.11 usage), fix per `go doc github.com/benbjohnson/litestream.RestoreOptions`.

- [ ] **Step 6: Commit**

```bash
git add cmd/bucketvcs/
git commit -m "feat(cli): bucketvcs authdb restore + replica-status"
```

---

### Task 8: Smoke script (durability + PITR + lease contention)

**Files:**
- Create: `scripts/authdb-replica-smoke-localfs.sh`

- [ ] **Step 1: Write the smoke**

```bash
#!/usr/bin/env bash
# scripts/authdb-replica-smoke-localfs.sh
#
# End-to-end smoke for embedded Litestream authdb replication (localfs).
# 1. serve --auth-db-replica=auto; create user+token; kill -9; DELETE the
#    local authdb; restart; assert the token still authenticates.
# 2. Point-in-time restore: token A, checkpoint timestamp, token B,
#    `authdb restore --timestamp` → A present, B absent.
# 3. Lease contention: second serve against the same prefix exits non-zero.
#
# Requires `bucketvcs` (or builds it) and `git` on PATH. No cloud creds.
# Same script works against MinIO: set SMOKE_STORE_URL=s3://bucket and the
# usual BUCKETVCS_S3_* env, then rerun.

set -euo pipefail

BVCS=${BVCS:-}
if [[ -z "$BVCS" ]]; then
  BVCS=$(mktemp -d)/bucketvcs
  go build -o "$BVCS" ./cmd/bucketvcs
fi

WORK=$(mktemp -d)
trap 'kill $(jobs -p) 2>/dev/null || true; rm -rf "$WORK"' EXIT
STORE_URL=${SMOKE_STORE_URL:-localfs:$WORK/store}
AUTHDB=$WORK/auth.db
PORT=$(python3 - <<'EOF'
import socket
s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()
EOF
)

serve() {
  "$BVCS" serve --addr="127.0.0.1:$PORT" --store="$STORE_URL" \
    --auth-db="$AUTHDB" --auth-db-replica=auto "$@" &
  SERVE_PID=$!
  for _ in $(seq 1 50); do
    curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && return 0
    sleep 0.2
  done
  echo "FAIL: serve did not become ready"; exit 1
}

# --- Phase 1: durability through total local-disk loss --------------------
serve
"$BVCS" user add --auth-db="$AUTHDB" --name=smoke --admin >/dev/null
TOKEN=$("$BVCS" token create --auth-db="$AUTHDB" --user=smoke --scopes=all | sed -n 's/^token=//p')
"$BVCS" repo register --auth-db="$AUTHDB" smoke/repo >/dev/null
sleep 2   # let litestream sync (1s monitor interval)
kill -9 "$SERVE_PID"; wait "$SERVE_PID" 2>/dev/null || true

rm -f "$AUTHDB" "$AUTHDB"-wal "$AUTHDB"-shm
rm -rf "$AUTHDB"-litestream

serve
CODE=$(curl -s -o /dev/null -w '%{http_code}' -u "smoke:$TOKEN" \
  "http://127.0.0.1:$PORT/smoke/repo/info/refs?service=git-upload-pack")
[[ "$CODE" == 200 ]] || { echo "FAIL: token did not survive restore (HTTP $CODE)"; exit 1; }
echo "PHASE1_DURABILITY_OK"

# --- Phase 2: point-in-time restore ----------------------------------------
sleep 2
CUTOFF=$(date -u +%Y-%m-%dT%H:%M:%SZ)
sleep 2
"$BVCS" token create --auth-db="$AUTHDB" --user=smoke --scopes=all --label=after-cutoff >/dev/null
sleep 2   # replicate token B
kill "$SERVE_PID"; wait "$SERVE_PID" 2>/dev/null || true

PITR=$WORK/pitr.db
"$BVCS" authdb restore --replica="${SMOKE_STORE_URL:-localfs:$WORK/store}" \
  --store="$STORE_URL" --output="$PITR" --timestamp="$CUTOFF"
BEFORE=$(sqlite3 "$PITR" "SELECT COUNT(*) FROM tokens" 2>/dev/null || echo "")
AFTER=$(sqlite3 "$AUTHDB" "SELECT COUNT(*) FROM tokens")
[[ -n "$BEFORE" && "$BEFORE" -lt "$AFTER" ]] || {
  echo "FAIL: PITR did not exclude post-cutoff token (before=$BEFORE after=$AFTER)"; exit 1; }
echo "PHASE2_PITR_OK"

# --- Phase 3: lease contention ----------------------------------------------
serve
if "$BVCS" serve --addr="127.0.0.1:1" --store="$STORE_URL" \
     --auth-db="$WORK/other.db" --auth-db-replica=auto 2>"$WORK/second.log"; then
  echo "FAIL: second serve should have been refused (lease held)"; exit 1
fi
grep -qi lease "$WORK/second.log" || { echo "FAIL: refusal does not mention lease"; exit 1; }
echo "PHASE3_LEASE_OK"

echo "AUTHDB_REPLICA_SMOKE_OK"
```

Adjust to reality while implementing: exact `user add`/`token create`/`repo register` flag names (copy from an existing smoke in `scripts/`), the healthz path, and whether `sqlite3` CLI is available (if not, use a tiny `go run` snippet to count rows; or guard with exit 77 SKIP when `sqlite3` is missing).

- [ ] **Step 2: Run it**

```bash
chmod +x scripts/authdb-replica-smoke-localfs.sh
./scripts/authdb-replica-smoke-localfs.sh
```

Expected: `PHASE1_DURABILITY_OK`, `PHASE2_PITR_OK`, `PHASE3_LEASE_OK`, `AUTHDB_REPLICA_SMOKE_OK`.

- [ ] **Step 3: Commit**

```bash
git add scripts/authdb-replica-smoke-localfs.sh
git commit -m "test(smoke): authdb replication durability + PITR + lease contention"
```

---

### Task 9: Operator guide + docs

**Files:**
- Create: `docs/operator-guides/authdb-replication.md`
- Modify: `README.md` (one feature bullet, following its existing list style)

- [ ] **Step 1: Write the operator guide**

`docs/operator-guides/authdb-replication.md` — follow the structure/tone of the existing guides in that directory. Required sections (write them fully, with real commands):

1. **What it does** — embedded Litestream v0.5; continuous WAL→LTX shipping of the sqlite authdb into the object store; restore-on-boot; RPO ≈ 1s sync interval.
2. **Enabling** — `--auth-db-replica=auto` (replica at `sys/authdb/` in `--store`) or an explicit storage URL; `--auth-db-replica-lease-ttl`; `--auth-db-replica-skip-restore` and exactly when it is safe.
3. **The reserved `sys/` prefix** — never write tenant data there; GC never lists outside `tenants/`; do not point lifecycle DELETE rules at it except as recommended below.
4. **Single-writer rule + lease** — exactly one serve per replica prefix; what the refusal error looks like; takeover after TTL; lease-lost behavior (replication stops, server keeps serving, alert on `authdb.replica.lease_lost`).
5. **Restore & PITR** — `bucketvcs authdb restore` examples for: disaster recovery to the standard path, `--output` inspection copies, `--timestamp`/`--txid` PITR; `replica-status` interpretation.
6. **Backend notes** — R2: set a lifecycle rule on `sys/authdb/` as a fallback against the DeleteObjects bug; watch `replica-status` file counts (litestream issue #976). localfs: `.meta` sidecars are expected.
7. **Failure modes table** — replication errors (fail-open, serve continues), boot-restore failure (fail-closed, serve refuses), LTX position mismatch after unclean shutdown (manual recovery steps: stop serve, `authdb restore --output` to verify the replica, then either accept replica state by deleting local file or re-seed the replica with `--auth-db-replica-skip-restore` after `DeleteAll` via a fresh prefix).
8. **Metrics & audit events** — the four metrics and four audit events from the spec, with one-line alerting guidance each.
9. **Limitations** — single replica destination (litestream v0.5), sqlite backend only, not for the M26 read gateways, BYOB tenants' buckets never hold the authdb.

- [ ] **Step 2: README bullet**

Add one line to README's feature list mentioning authdb replication + restore-on-boot, linking the guide.

- [ ] **Step 3: Commit**

```bash
git add docs/operator-guides/authdb-replication.md README.md
git commit -m "docs: authdb replication operator guide"
```

---

### Task 10: Full verification pass

- [ ] **Step 1: Whole-tree build, vet, test**

```bash
go build ./... && go vet ./... && go test ./... 2>&1 | tail -15
```

Expected: all green (cloud conformance subtests SKIP without env).

- [ ] **Step 2: Re-run the smoke**

```bash
./scripts/authdb-replica-smoke-localfs.sh
```

Expected: `AUTHDB_REPLICA_SMOKE_OK`.

- [ ] **Step 3: If MinIO is available, run the S3 variant**

```bash
# (env names per scripts/ conventions for the existing MinIO smokes)
SMOKE_STORE_URL=s3://<bucket> BUCKETVCS_S3_ENDPOINT=http://127.0.0.1:9000 \
  ./scripts/authdb-replica-smoke-localfs.sh
BUCKETVCS_S3_BUCKET=<bucket> BUCKETVCS_S3_ENDPOINT=http://127.0.0.1:9000 \
  go test ./internal/authreplica/ -run TestConformance -v
```

- [ ] **Step 4: Final commit if anything moved**

```bash
git status --short && git add -A && git commit -m "chore: authdb replication final verification fixes" || true
```

---

## Plan self-review (done at authoring time)

- **Spec coverage:** Client mapping table → Task 2; conformance-across-backends → Task 3 (adapted, since the upstream harness is unimportable — deviation from spec noted); lease → Task 4; Runner/EnsureExists/compaction defaults/shutdown order → Task 5; flags + validation matrix + boot sequence → Task 6; restore CLI + replica-status → Task 7; smokes incl. kill-9 durability, PITR, lease contention → Task 8; operator guide incl. R2 lifecycle + metrics/audit → Task 9 (metric/audit emission itself is in Tasks 5–6 code).
- **Known uncertainty, handled in-plan:** exact `RestoreOptions`/`CompactionLevel` field shapes and `storage.Object` version access are verified by `go doc` steps with one-line adjustment instructions at each call site; CLI flag names in the smoke are to be copied from existing smokes.
- **Type consistency:** `authreplica.NewClient/NewLease/Prepare/StartReplication/SyncNow/Close` used identically across Tasks 5–8; `resolveAuthDBReplica` spec struct used only in Task 6.
