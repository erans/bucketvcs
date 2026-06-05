# Usage & Activity Log Shipping Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship two durable NDJSON streams (activity = audit events, usage = operation metering) into the object store under `sys/logs/`, batched through a crash-safe local spool (1000 events OR 15 min rotation), on by default.

**Architecture:** New `internal/shiplog` package: an `Engine` (spool files, rotation, ship loop, bounded pending queue) fed by (a) a fanout `slog.Handler` that taps `audit=true` records — zero audit-site changes, stderr untouched — and (b) a typed `UsageEvent` API called from gateway/LFS/proxied handler sites. Serve wires flags, boot-time leftover shipping, and an ordered shutdown flush.

**Tech Stack:** Go stdlib (`log/slog`, `compress/gzip`, `encoding/json`), existing `storage.ObjectStore` (`PutIfAbsent`), localfs for tests.

**Spec:** `docs/superpowers/specs/2026-06-05-usage-activity-log-shipping-design.md`
**Module:** `github.com/bucketvcs/bucketvcs`

---

## Background for the implementer (read first)

- **Logging today:** serve uses the stdlib default slog handler (`logger := slog.Default()` at cmd/bucketvcs/serve.go:403; `slog.SetDefault` is never called). Audit events are emitted via `logger.LogAttrs(ctx, level, "<event.name>", slog.Bool("audit", true), slog.String("event", "<name>"), ...)` — the `audit` attr is always inline per-record (never via `logger.With`), so a record-attr walk finds it.
- **Metric lines** (`metric_name` attrs) are NOT shipped — only `audit=true` records go to the activity stream.
- **`sys/` prefix** is reserved (GC provably never lists outside `tenants/`); `sys/authdb/` set the precedent. Logs go to `sys/logs/`.
- **`storage.ObjectStore`:** `PutIfAbsent(ctx, key, body io.Reader, opts) (ObjectVersion, error)` with sentinel `storage.ErrAlreadyExists` — for shipping, AlreadyExists = an identical re-ship = treat as success. List/Get not needed by the engine.
- **State-dir resolution precedent:** `resolveAuthDB(flag, realEnv())` in cmd/bucketvcs/authdb.go:35-49 (flag → `BUCKETVCS_AUTH_DB` env → `$XDG_STATE_HOME/bucketvcs/...` → `$HOME/.local/state/bucketvcs/...`); `realEnv()` at :13-27. Mirror this for the spool dir.
- **Serve shutdown region:** `defer authS.Close()` at serve.go:~340 with the M28 replication defer right after (:348-371). CRITICAL ordering: the shipper PUTs to the system store, so its shutdown flush must run BEFORE `closeStore(store)`'s defer — i.e. the shipper's defer must be REGISTERED AFTER the store's close-defer (LIFO). The store's `defer closeStore(store)` is registered shortly after `openStore` (~serve.go:225). Anything registered later runs earlier — verify by reading.
- **Actor identity at gateway sites:** `gateway.ActorFromContext(ctx) *auth.Actor` (internal/gateway/auth.go:23); use `a.Name`, fallback `a.UserID`, else `"anonymous"`. SSH handlers carry the actor on their session/EngineRequest (`eng.Actor`).
- **Proxied bundle/pack handlers** (internal/gateway/proxied_routes.go) already count served bytes (`bytesServed` feeding `emitMetric(... kind+"_uri_served_bytes" ...)` at :339-341) — piggyback there.
- **Flag conventions:** serveflags.go struct + registrations; validation errors `fmt.Fprintf(stderr, "serve: ...")` + `return 2`. Env-twin convention: default from `os.Getenv` (see `--auth-db-replica`).
- **Spec deviation to encode:** the spec lists a `clone` usage kind; clone vs fetch is indistinguishable at the transport layer without negotiation introspection — emit `fetch` for both and document (update the spec's kind list in the operator guide; note in report).

---

## File structure

```
internal/shiplog/
  engine.go        # Engine, Config, Stream, spool/rotation/intake
  ship.go          # ship loop, gzip+PutIfAbsent, bounded cap, boot leftovers
  tap.go           # fanout slog.Handler (audit=true → activity)
  usage.go         # UsageEvent type + marshal + Engine.Usage
  engine_test.go   # rotation/intake tests
  ship_test.go     # ship/leftover/cap/fault tests
  tap_test.go      # tap routing tests
  usage_test.go    # usage marshal tests
cmd/bucketvcs/
  shiplog_resolve.go       # resolveSpoolDir + flag resolution helper
  shiplog_resolve_test.go
  serveflags.go            # +4 flags
  serve.go                 # wiring: engine boot, SetDefault, shutdown defer
internal/gateway/          # usage instrumentation (upload-pack/receive-pack/LFS/proxied)
internal/sshd/             # usage instrumentation (ssh transport)
scripts/logship-smoke-localfs.sh
docs/operator-guides/log-shipping.md
docs/upgrade-notes.md      # v-next entry (on-by-default + sys/logs/)
README.md                  # feature bullet
```

---

### Task 1: Engine core — intake, spool files, rotation

**Files:**
- Create: `internal/shiplog/engine.go`
- Test: `internal/shiplog/engine_test.go`

- [ ] **Step 1: Write the failing tests**

`internal/shiplog/engine_test.go`:

```go
package shiplog

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func newTestEngine(t *testing.T, mutate func(*Config)) (*Engine, storage.ObjectStore, string) {
	t.Helper()
	st, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	spool := t.TempDir()
	cfg := Config{
		Store:         st,
		SpoolDir:      spool,
		MaxEvents:     5, // small for tests
		MaxAge:        time.Hour,
		SpoolMaxBytes: 1 << 20,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = e.Close(ctx)
	})
	return e, st, spool
}

// drainAppends blocks until the intake goroutine has applied all queued events.
func drainAppends(t *testing.T, e *Engine) {
	t.Helper()
	if err := e.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func spoolFiles(t *testing.T, spool, contains string) []string {
	t.Helper()
	ents, err := os.ReadDir(spool)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, en := range ents {
		if strings.Contains(en.Name(), contains) {
			out = append(out, en.Name())
		}
	}
	return out
}

func TestEngine_AppendsNDJSONToActiveFile(t *testing.T) {
	e, _, spool := newTestEngine(t, nil)
	e.Enqueue(StreamActivity, []byte(`{"event":"a"}`))
	e.Enqueue(StreamActivity, []byte(`{"event":"b"}`))
	drainAppends(t, e)

	files := spoolFiles(t, spool, "activity-")
	if len(files) != 1 {
		t.Fatalf("want 1 active activity file, got %v", files)
	}
	raw, err := os.ReadFile(filepath.Join(spool, files[0]))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 || lines[0] != `{"event":"a"}` || lines[1] != `{"event":"b"}` {
		t.Fatalf("bad spool content: %q", raw)
	}
}

func TestEngine_RotatesAtMaxEvents(t *testing.T) {
	e, _, spool := newTestEngine(t, nil) // MaxEvents=5
	for i := 0; i < 12; i++ {
		e.Enqueue(StreamUsage, []byte(`{"n":1}`))
	}
	drainAppends(t, e)
	// 12 events @5/file → 2 pending + 1 active(2 events)
	pending := spoolFiles(t, spool, ".pending.")
	if len(pending) != 2 {
		t.Fatalf("want 2 pending files, got %v", pending)
	}
}

func TestEngine_RotatesAtMaxAge(t *testing.T) {
	now := time.Now()
	clock := &now
	e, _, spool := newTestEngine(t, func(c *Config) {
		c.MaxAge = 15 * time.Minute
		c.Now = func() time.Time { return *clock }
	})
	e.Enqueue(StreamActivity, []byte(`{"event":"x"}`))
	drainAppends(t, e)
	if n := len(spoolFiles(t, spool, ".pending.")); n != 0 {
		t.Fatalf("premature rotation: %d pending", n)
	}
	*clock = now.Add(16 * time.Minute)
	e.tickRotate() // exported-for-test hook driving the age check
	drainAppends(t, e)
	if n := len(spoolFiles(t, spool, ".pending.")); n != 1 {
		t.Fatalf("want age rotation → 1 pending, got %d", n)
	}
}

func TestEngine_EmptyFileNeverRotates(t *testing.T) {
	now := time.Now()
	clock := &now
	e, _, spool := newTestEngine(t, func(c *Config) {
		c.MaxAge = 15 * time.Minute
		c.Now = func() time.Time { return *clock }
	})
	*clock = now.Add(2 * time.Hour)
	e.tickRotate()
	drainAppends(t, e)
	if n := len(spoolFiles(t, spool, ".pending.")); n != 0 {
		t.Fatalf("empty stream rotated: %d pending", n)
	}
	// No active file should even exist until the first event arrives.
	if n := len(spoolFiles(t, spool, ".ndjson")); n != 0 {
		t.Fatalf("idle stream created files: %v", spoolFiles(t, spool, ""))
	}
}

func TestEngine_DropsWhenChannelFull(t *testing.T) {
	e, _, _ := newTestEngine(t, func(c *Config) {
		c.QueueSize = 1
		c.pauseIntake = true // test seam: intake goroutine doesn't consume
	})
	e.Enqueue(StreamActivity, []byte(`{"a":1}`)) // fills queue
	e.Enqueue(StreamActivity, []byte(`{"a":2}`)) // must drop, not block
	if got := e.DroppedEvents(); got != 1 {
		t.Fatalf("want 1 dropped, got %d", got)
	}
	e.resumeIntake()
}
```

- [ ] **Step 2: Run to verify failure**

```bash
go test ./internal/shiplog/ -run TestEngine -v 2>&1 | head -5
```
Expected: compile error (`New` undefined).

- [ ] **Step 3: Implement `internal/shiplog/engine.go`**

```go
// Package shiplog batches activity (audit) and usage (metering) events into
// local NDJSON spool files and ships them to the object store under
// sys/logs/<stream>/. Fail-open by design: the request path never blocks on
// logging, and a store outage degrades the trail without affecting serving.
package shiplog

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Stream identifies one of the two shipped log streams.
type Stream string

const (
	StreamActivity Stream = "activity"
	StreamUsage    Stream = "usage"
)

// Defaults (spec §Decision summary).
const (
	DefaultMaxEvents     = 1000
	DefaultMaxAge        = 15 * time.Minute
	DefaultSpoolMaxBytes = 256 << 20 // 256 MB
	DefaultQueueSize     = 4096
	DefaultPrefix        = "sys/logs"
	shipTick             = 5 * time.Second
)

// Config configures the Engine.
type Config struct {
	Store         storage.ObjectStore
	Prefix        string        // default DefaultPrefix
	SpoolDir      string        // required
	MaxEvents     int           // rotation event threshold; default 1000
	MaxAge        time.Duration // rotation age threshold (since first event); default 15m
	SpoolMaxBytes int64         // pending-file cap; default 256MB
	QueueSize     int           // intake channel size; default 4096
	Logger        *slog.Logger  // MUST be built on the base (non-tap) handler
	Now           func() time.Time

	pauseIntake bool // test seam: do not start the intake goroutine
}

type event struct {
	stream Stream
	line   []byte
}

// streamState is owned exclusively by the intake goroutine.
type streamState struct {
	f       *os.File
	path    string
	count   int
	firstAt time.Time
	seq     int
}

// Engine owns the spool directory and both streams.
type Engine struct {
	cfg        Config
	logger     *slog.Logger
	instanceID string

	ch     chan event
	flush  chan chan struct{}
	rotate chan struct{}

	mu      sync.Mutex // guards pending list (intake adds, ship loop removes)
	pending []string   // absolute paths of rotated, unshipped files

	dropped       atomic.Int64
	droppedFiles  atomic.Int64
	shippedFiles  atomic.Int64
	shippedEvents atomic.Int64

	intakeDone chan struct{}
	shipDone   chan struct{}
	shipCancel context.CancelFunc
	closeOnce  sync.Once

	streams map[Stream]*streamState
}

// New creates the engine: ensures the spool dir, adopts leftover files from a
// previous run as pending, and starts the intake goroutine. Call Start to
// begin shipping; call Close to flush and stop.
func New(cfg Config) (*Engine, error) {
	if cfg.SpoolDir == "" {
		return nil, fmt.Errorf("shiplog: SpoolDir is required")
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("shiplog: Store is required")
	}
	if cfg.Prefix == "" {
		cfg.Prefix = DefaultPrefix
	}
	if cfg.MaxEvents <= 0 {
		cfg.MaxEvents = DefaultMaxEvents
	}
	if cfg.MaxAge <= 0 {
		cfg.MaxAge = DefaultMaxAge
	}
	if cfg.SpoolMaxBytes <= 0 {
		cfg.SpoolMaxBytes = DefaultSpoolMaxBytes
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = DefaultQueueSize
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if err := os.MkdirAll(cfg.SpoolDir, 0o700); err != nil {
		return nil, fmt.Errorf("shiplog: create spool dir: %w", err)
	}
	idb := make([]byte, 4)
	if _, err := rand.Read(idb); err != nil {
		return nil, fmt.Errorf("shiplog: instance id: %w", err)
	}
	e := &Engine{
		cfg:        cfg,
		logger:     cfg.Logger.With(slog.String("subsystem", "shiplog")),
		instanceID: hex.EncodeToString(idb),
		ch:         make(chan event, cfg.QueueSize),
		flush:      make(chan chan struct{}),
		rotate:     make(chan struct{}, 1),
		intakeDone: make(chan struct{}),
		streams: map[Stream]*streamState{
			StreamActivity: {},
			StreamUsage:    {},
		},
	}
	if err := e.adoptLeftovers(); err != nil { // implemented in ship.go
		return nil, err
	}
	if !cfg.pauseIntake {
		go e.intakeLoop()
	}
	return e, nil
}

// Enqueue submits one NDJSON line (no trailing newline) to a stream.
// Never blocks: a full queue drops the event and increments the counter.
func (e *Engine) Enqueue(s Stream, line []byte) {
	select {
	case e.ch <- event{stream: s, line: line}:
	default:
		e.dropped.Add(1)
	}
}

// DroppedEvents reports events dropped due to a full intake queue.
func (e *Engine) DroppedEvents() int64 { return e.dropped.Load() }

// Flush blocks until every event enqueued before the call has been appended
// to its spool file. Test/shutdown helper; does not force a ship.
func (e *Engine) Flush(ctx context.Context) error {
	done := make(chan struct{})
	select {
	case e.flush <- done:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// tickRotate asks the intake goroutine to run an age-based rotation check.
func (e *Engine) tickRotate() {
	select {
	case e.rotate <- struct{}{}:
	default:
	}
}

// resumeIntake starts the intake goroutine for tests that constructed the
// engine with pauseIntake.
func (e *Engine) resumeIntake() { go e.intakeLoop() }

// intakeLoop is the single writer of spool files.
func (e *Engine) intakeLoop() {
	defer close(e.intakeDone)
	age := time.NewTicker(time.Minute) // age-trigger granularity
	defer age.Stop()
	for {
		select {
		case ev, ok := <-e.ch:
			if !ok {
				e.rotateAll() // final rotation on close (empty = no-op)
				return
			}
			e.append(ev)
		case done := <-e.flush:
			e.drainQueued()
			close(done)
		case <-e.rotate:
			e.checkAge()
		case <-age.C:
			e.checkAge()
		}
	}
}

// drainQueued applies everything currently buffered in the channel.
func (e *Engine) drainQueued() {
	for {
		select {
		case ev, ok := <-e.ch:
			if !ok {
				return
			}
			e.append(ev)
		default:
			return
		}
	}
}

func (e *Engine) append(ev event) {
	st := e.streams[ev.stream]
	if st.f == nil {
		if err := e.openActive(ev.stream, st); err != nil {
			e.dropped.Add(1)
			e.logger.Error("shiplog: open spool file", slog.Any("error", err))
			return
		}
	}
	if _, err := st.f.Write(append(ev.line, '\n')); err != nil {
		e.dropped.Add(1)
		e.logger.Error("shiplog: append", slog.Any("error", err))
		return
	}
	if st.count == 0 {
		st.firstAt = e.cfg.Now()
	}
	st.count++
	if st.count >= e.cfg.MaxEvents {
		e.rotateStream(ev.stream, st)
	}
}

func (e *Engine) checkAge() {
	for s, st := range e.streams {
		if st.count > 0 && e.cfg.Now().Sub(st.firstAt) >= e.cfg.MaxAge {
			e.rotateStream(s, st)
		}
	}
}

func (e *Engine) rotateAll() {
	for s, st := range e.streams {
		if st.count > 0 {
			e.rotateStream(s, st)
		}
	}
}

func (e *Engine) openActive(s Stream, st *streamState) error {
	st.seq++
	st.path = filepath.Join(e.cfg.SpoolDir,
		fmt.Sprintf("%s-%s-%06d.ndjson", s, e.instanceID, st.seq))
	f, err := os.OpenFile(st.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st.f = f
	st.count = 0
	return nil
}

// rotateStream closes the active file and renames it to a pending name that
// embeds the rotation timestamp (the ship loop derives the bucket key from
// the name alone, so leftovers from a crash stay self-describing).
func (e *Engine) rotateStream(s Stream, st *streamState) {
	if st.f == nil || st.count == 0 {
		return
	}
	_ = st.f.Close()
	ts := e.cfg.Now().UTC().Format("20060102T150405")
	pendingPath := filepath.Join(e.cfg.SpoolDir,
		fmt.Sprintf("%s-%s-%06d.pending.%s.ndjson", s, e.instanceID, st.seq, ts))
	if err := os.Rename(st.path, pendingPath); err != nil {
		e.logger.Error("shiplog: rotate rename", slog.Any("error", err))
		st.f, st.count = nil, 0
		return
	}
	e.mu.Lock()
	e.pending = append(e.pending, pendingPath)
	e.mu.Unlock()
	st.f, st.count = nil, 0
}
```

NOTE: `Config.pauseIntake`/`resumeIntake`/`tickRotate` are deliberate test seams in-package (lowercase field, methods used only from tests). `adoptLeftovers`, `Start`, `Close` land in Task 2's `ship.go` — to make THIS task compile, add temporary stubs at the bottom of engine.go and REMOVE them in Task 2:

```go
// Stubs replaced in ship.go (Task 2).
func (e *Engine) adoptLeftovers() error { return nil }
func (e *Engine) Close(ctx context.Context) error {
	e.closeOnce.Do(func() { close(e.ch); <-e.intakeDone })
	return nil
}
```

- [ ] **Step 4: Run tests**

```bash
go test ./internal/shiplog/ -run TestEngine -v -count=1
```
Expected: all PASS. If `TestEngine_DropsWhenChannelFull` flakes because Enqueue raced the paused intake, the `pauseIntake` seam isn't honored — verify New skips `go e.intakeLoop()` when set.

- [ ] **Step 5: Commit**

```bash
git add internal/shiplog/
git commit -m "feat(shiplog): engine core — intake, spool files, rotation triggers"
```

---

### Task 2: Ship loop, boot leftovers, bounded spool

**Files:**
- Create: `internal/shiplog/ship.go`
- Test: `internal/shiplog/ship_test.go`
- Modify: `internal/shiplog/engine.go` (remove Task 1 stubs)

- [ ] **Step 1: Write the failing tests**

`internal/shiplog/ship_test.go`:

```go
package shiplog

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// listKeys returns all object keys under prefix, paginated.
func listKeys(t *testing.T, st storage.ObjectStore, prefix string) []string {
	t.Helper()
	var keys []string
	var token string
	for {
		page, err := st.List(context.Background(), prefix, &storage.ListOptions{ContinuationToken: token})
		if err != nil {
			t.Fatal(err)
		}
		for _, om := range page.Objects {
			keys = append(keys, om.Key)
		}
		if page.NextToken == "" {
			return keys
		}
		token = page.NextToken
	}
}

func gunzipObject(t *testing.T, st storage.ObjectStore, key string) string {
	t.Helper()
	obj, err := st.Get(context.Background(), key, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Body.Close()
	zr, err := gzip.NewReader(obj.Body)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestShip_RotatedFileLandsInBucketAndLeavesSpool(t *testing.T) {
	e, st, spool := newTestEngine(t, nil) // MaxEvents=5
	for i := 0; i < 5; i++ {
		e.Enqueue(StreamActivity, []byte(fmt.Sprintf(`{"i":%d}`, i)))
	}
	drainAppends(t, e)
	if err := e.ShipPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	keys := listKeys(t, st, "sys/logs/activity/")
	if len(keys) != 1 || !strings.HasSuffix(keys[0], ".ndjson.gz") {
		t.Fatalf("want 1 gz key, got %v", keys)
	}
	content := gunzipObject(t, st, keys[0])
	if want := 5; strings.Count(content, "\n") != want {
		t.Fatalf("want %d lines, got %q", want, content)
	}
	if n := len(spoolFiles(t, spool, ".pending.")); n != 0 {
		t.Fatalf("pending file not deleted after ship: %d", n)
	}
}

func TestShip_LeftoversAdoptedOnBoot(t *testing.T) {
	st, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	spool := t.TempDir()
	// Simulate a crash: a pending file AND an orphaned active file.
	pend := filepath.Join(spool, "usage-deadbeef-000003.pending.20260605T210000.ndjson")
	if err := os.WriteFile(pend, []byte("{\"a\":1}\n{\"a\":2}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	act := filepath.Join(spool, "activity-deadbeef-000004.ndjson")
	if err := os.WriteFile(act, []byte("{\"b\":1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := New(Config{Store: st, SpoolDir: spool})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close(context.Background())
	if err := e.ShipPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(listKeys(t, st, "sys/logs/usage/")); n != 1 {
		t.Fatalf("crashed pending file not shipped: %d usage keys", n)
	}
	if n := len(listKeys(t, st, "sys/logs/activity/")); n != 1 {
		t.Fatalf("orphaned active file not shipped: %d activity keys", n)
	}
	ents, _ := os.ReadDir(spool)
	if len(ents) != 0 {
		t.Fatalf("spool not empty after leftover ship: %v", ents)
	}
}

// failNPuts wraps a store, failing the first n PutIfAbsent calls.
type failNPuts struct {
	storage.ObjectStore
	n int
}

func (s *failNPuts) PutIfAbsent(ctx context.Context, key string, body io.Reader, opts *storage.PutOptions) (storage.ObjectVersion, error) {
	if s.n > 0 {
		s.n--
		return storage.ObjectVersion{}, storage.ErrTransient
	}
	return s.ObjectStore.PutIfAbsent(ctx, key, body, opts)
}

func TestShip_FailedPutKeepsFileAndRetries(t *testing.T) {
	base, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fs := &failNPuts{ObjectStore: base, n: 1}
	e, _, spool := newTestEngine(t, func(c *Config) { c.Store = fs })
	for i := 0; i < 5; i++ {
		e.Enqueue(StreamUsage, []byte(`{"x":1}`))
	}
	drainAppends(t, e)
	if err := e.ShipPending(context.Background()); err == nil {
		t.Fatal("first ship should report the transient failure")
	}
	if n := len(spoolFiles(t, spool, ".pending.")); n != 1 {
		t.Fatalf("file must survive failed PUT, got %d pending", n)
	}
	if err := e.ShipPending(context.Background()); err != nil { // retry succeeds
		t.Fatal(err)
	}
	if n := len(spoolFiles(t, spool, ".pending.")); n != 0 {
		t.Fatalf("retry did not clear pending: %d", n)
	}
}

func TestShip_AlreadyExistsIsSuccess(t *testing.T) {
	// At-least-once: a crash between PUT and local delete re-ships the same
	// key; PutIfAbsent → ErrAlreadyExists must count as shipped.
	e, st, spool := newTestEngine(t, nil)
	for i := 0; i < 5; i++ {
		e.Enqueue(StreamActivity, []byte(`{"x":1}`))
	}
	drainAppends(t, e)
	// Pre-create the exact destination key.
	e.mu.Lock()
	pendingPath := e.pending[0]
	e.mu.Unlock()
	key, err := bucketKeyForPending(e.cfg.Prefix, filepath.Base(pendingPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutIfAbsent(context.Background(), key, strings.NewReader("gz-placeholder"), nil); err != nil {
		t.Fatal(err)
	}
	if err := e.ShipPending(context.Background()); err != nil {
		t.Fatalf("AlreadyExists must be success: %v", err)
	}
	if n := len(spoolFiles(t, spool, ".pending.")); n != 0 {
		t.Fatalf("file not deleted after AlreadyExists: %d", n)
	}
}

func TestShip_BoundedSpoolDropsOldest(t *testing.T) {
	base, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fs := &failNPuts{ObjectStore: base, n: 1 << 30} // PUTs always fail
	e, _, spool := newTestEngine(t, func(c *Config) {
		c.Store = fs
		c.SpoolMaxBytes = 64 // tiny cap
	})
	for batch := 0; batch < 4; batch++ {
		for i := 0; i < 5; i++ {
			e.Enqueue(StreamUsage, []byte(`{"pad":"0123456789"}`))
		}
		drainAppends(t, e)
		_ = e.ShipPending(context.Background()) // fails; triggers cap enforcement
	}
	if e.DroppedFiles() == 0 {
		t.Fatal("cap never dropped a file")
	}
	var total int64
	for _, name := range spoolFiles(t, spool, ".pending.") {
		fi, _ := os.Stat(filepath.Join(spool, name))
		total += fi.Size()
	}
	if total > 64+128 { // cap plus at most one file of slack
		t.Fatalf("pending bytes %d exceed cap", total)
	}
}

func TestClose_RotatesAndShipsFinal(t *testing.T) {
	st, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	spool := t.TempDir()
	e, err := New(Config{Store: st, SpoolDir: spool, MaxEvents: 1000})
	if err != nil {
		t.Fatal(err)
	}
	e.Enqueue(StreamActivity, []byte(`{"final":1}`)) // below MaxEvents
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(listKeys(t, st, "sys/logs/activity/")); n != 1 {
		t.Fatalf("shutdown flush did not ship: %d keys", n)
	}
	ents, _ := os.ReadDir(spool)
	if len(ents) != 0 {
		t.Fatalf("spool not empty after Close: %v", ents)
	}
}

func TestClose_IdleShipsNothing(t *testing.T) {
	st, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(Config{Store: st, SpoolDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(listKeys(t, st, "sys/logs/")); n != 0 {
		t.Fatalf("idle engine shipped %d objects", n)
	}
}
```

- [ ] **Step 2: Verify compile failure** (`ShipPending`, `DroppedFiles`, `bucketKeyForPending` undefined).

- [ ] **Step 3: Implement `internal/shiplog/ship.go`** (and DELETE the Task 1 stubs from engine.go)

```go
package shiplog

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// pendingNameRe parses "<stream>-<instance>-<seq>.pending.<ts>.ndjson".
var pendingNameRe = regexp.MustCompile(
	`^(activity|usage)-([0-9a-f]+)-(\d{6})\.pending\.(\d{8}T\d{6})\.ndjson$`)

// activeNameRe parses an orphaned active file "<stream>-<instance>-<seq>.ndjson".
var activeNameRe = regexp.MustCompile(
	`^(activity|usage)-([0-9a-f]+)-(\d{6})\.ndjson$`)

// bucketKeyForPending derives the destination key from a pending file name:
// sys/logs/<stream>/<YYYY>/<MM>/<DD>/<HHMMSS>-<instance>-<seq>.ndjson.gz
func bucketKeyForPending(prefix, name string) (string, error) {
	m := pendingNameRe.FindStringSubmatch(name)
	if m == nil {
		return "", fmt.Errorf("shiplog: unparseable pending name %q", name)
	}
	stream, instance, seq, ts := m[1], m[2], m[3], m[4]
	when, err := time.Parse("20060102T150405", ts)
	if err != nil {
		return "", fmt.Errorf("shiplog: bad timestamp in %q: %w", name, err)
	}
	return fmt.Sprintf("%s/%s/%s/%s-%s-%s.ndjson.gz",
		prefix, stream, when.Format("2006/01/02"), when.Format("150405"), instance, seq), nil
}

// adoptLeftovers runs at New: every file in the spool dir from a previous
// run becomes pending. Orphaned ACTIVE files (a crash before rotation) are
// renamed to pending names using their mtime so the key derivation works.
func (e *Engine) adoptLeftovers() error {
	ents, err := os.ReadDir(e.cfg.SpoolDir)
	if err != nil {
		return fmt.Errorf("shiplog: scan spool dir: %w", err)
	}
	for _, en := range ents {
		if en.IsDir() {
			continue
		}
		name := en.Name()
		full := filepath.Join(e.cfg.SpoolDir, name)
		switch {
		case pendingNameRe.MatchString(name):
			e.pending = append(e.pending, full)
		case activeNameRe.MatchString(name):
			fi, err := en.Info()
			if err != nil {
				continue
			}
			if fi.Size() == 0 { // empty orphan: just remove
				_ = os.Remove(full)
				continue
			}
			ts := fi.ModTime().UTC().Format("20060102T150405")
			renamed := strings.TrimSuffix(full, ".ndjson") + ".pending." + ts + ".ndjson"
			if err := os.Rename(full, renamed); err != nil {
				e.logger.Warn("shiplog: adopt orphan", slog.Any("error", err))
				continue
			}
			e.pending = append(e.pending, renamed)
		}
	}
	sort.Strings(e.pending) // oldest-first by name (ts embedded)
	return nil
}

// Start launches the ship loop. Separate from New so serve can install the
// slog tap between construction and shipping.
func (e *Engine) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	e.shipCancel = cancel
	e.shipDone = make(chan struct{})
	go func() {
		defer close(e.shipDone)
		t := time.NewTicker(shipTick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := e.ShipPending(ctx); err != nil {
					e.logger.Warn("shiplog: ship tick", slog.Any("error", err))
				}
			}
		}
	}()
}

// ShipPending gzips and uploads every pending file, deleting each local file
// only after a successful PUT (or ErrAlreadyExists — an identical re-ship).
// On failure the file stays pending; the bounded cap is enforced after.
func (e *Engine) ShipPending(ctx context.Context) error {
	e.mu.Lock()
	files := append([]string(nil), e.pending...)
	e.mu.Unlock()

	var firstErr error
	for _, f := range files {
		if err := e.shipOne(ctx, f); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		e.removePending(f)
	}
	e.enforceCap()
	return firstErr
}

func (e *Engine) shipOne(ctx context.Context, path string) error {
	key, err := bucketKeyForPending(e.cfg.Prefix, filepath.Base(path))
	if err != nil {
		return err
	}
	// gzip to a sibling temp file, then stream it (re-readable on retry is
	// unnecessary: we re-gzip per attempt; files are ≤ MaxEvents lines).
	raw, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("shiplog: open pending: %w", err)
	}
	defer raw.Close()
	pr, pw := io.Pipe()
	go func() {
		zw := gzip.NewWriter(pw)
		_, cerr := io.Copy(zw, raw)
		if cerr == nil {
			cerr = zw.Close()
		}
		pw.CloseWithError(cerr)
	}()
	if _, err := e.cfg.Store.PutIfAbsent(ctx, key, pr, nil); err != nil &&
		!errors.Is(err, storage.ErrAlreadyExists) {
		return fmt.Errorf("shiplog: put %s: %w", key, err)
	}
	e.shippedFiles.Add(1)
	return nil
}

func (e *Engine) removePending(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		e.logger.Warn("shiplog: remove shipped file", slog.Any("error", err))
	}
	e.mu.Lock()
	for i, p := range e.pending {
		if p == path {
			e.pending = append(e.pending[:i], e.pending[i+1:]...)
			break
		}
	}
	e.mu.Unlock()
}

// enforceCap drops the OLDEST pending files until total size fits the cap.
func (e *Engine) enforceCap() {
	e.mu.Lock()
	defer e.mu.Unlock()
	var total int64
	sizes := make([]int64, len(e.pending))
	for i, p := range e.pending {
		if fi, err := os.Stat(p); err == nil {
			sizes[i] = fi.Size()
			total += fi.Size()
		}
	}
	for i := 0; total > e.cfg.SpoolMaxBytes && i < len(e.pending); i++ {
		e.logger.Error("shiplog: spool cap exceeded — dropping oldest pending file",
			slog.String("file", filepath.Base(e.pending[i])), slog.Int64("bytes", sizes[i]))
		_ = os.Remove(e.pending[i])
		e.droppedFiles.Add(1)
		total -= sizes[i]
		e.pending[i] = "" // compact below
	}
	out := e.pending[:0]
	for _, p := range e.pending {
		if p != "" {
			out = append(out, p)
		}
	}
	e.pending = out
}

// DroppedFiles reports pending files dropped by the spool cap.
func (e *Engine) DroppedFiles() int64 { return e.droppedFiles.Load() }

// Close stops intake, rotates non-empty actives, ships everything pending
// (bounded by ctx), and stops the ship loop. Safe to call once.
func (e *Engine) Close(ctx context.Context) error {
	var shipErr error
	e.closeOnce.Do(func() {
		close(e.ch)     // intakeLoop drains, rotates non-empty actives, exits
		<-e.intakeDone
		if e.shipCancel != nil {
			e.shipCancel()
			<-e.shipDone
		}
		shipErr = e.ShipPending(ctx)
	})
	return shipErr
}
```

(Add `"io"` to imports. The engine.go stubs from Task 1 must be deleted; `Close` here replaces the stub.)

- [ ] **Step 4: Run tests**

```bash
go test ./internal/shiplog/ -count=1 -v 2>&1 | tail -15 && go vet ./internal/shiplog/
```
Expected: all Task 1+2 tests PASS. Likely issue: `TestShip_BoundedSpoolDropsOldest` slack — adjust the `64+128` tolerance to the actual single-file size if line lengths differ.

- [ ] **Step 5: Race check** — `go test ./internal/shiplog/ -count=1 -race` → PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/shiplog/
git commit -m "feat(shiplog): ship loop — gzip+PutIfAbsent, boot leftovers, bounded spool"
```

---

### Task 3: slog tap handler (activity feed)

**Files:**
- Create: `internal/shiplog/tap.go`
- Test: `internal/shiplog/tap_test.go`

- [ ] **Step 1: Write the failing tests**

`internal/shiplog/tap_test.go`:

```go
package shiplog

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// memHandler records every record it Handles (the "stderr" stand-in).
type memHandler struct{ msgs []string }

func (h *memHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *memHandler) Handle(_ context.Context, r slog.Record) error {
	h.msgs = append(h.msgs, r.Message)
	return nil
}
func (h *memHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *memHandler) WithGroup(string) slog.Handler      { return h }

func readActivitySpool(t *testing.T, e *Engine, spool string) []map[string]any {
	t.Helper()
	drainAppends(t, e)
	var out []map[string]any
	for _, name := range spoolFiles(t, spool, "activity-") {
		raw, err := os.ReadFile(filepath.Join(spool, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if line == "" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("non-JSON activity line %q: %v", line, err)
			}
			out = append(out, m)
		}
	}
	return out
}

func TestTap_RoutesAuditRecordsAndPassesThrough(t *testing.T) {
	e, _, spool := newTestEngine(t, nil)
	base := &memHandler{}
	logger := slog.New(NewTapHandler(base, e))

	logger.LogAttrs(context.Background(), slog.LevelInfo, "policy.ref.rejected",
		slog.Bool("audit", true),
		slog.String("event", "policy.ref.rejected"),
		slog.String("tenant", "acme"),
		slog.String("repo", "app"),
	)
	logger.LogAttrs(context.Background(), slog.LevelInfo, "metric",
		slog.String("metric_name", "x_total"), slog.Int64("value", 1))
	logger.Info("plain line")

	// Pass-through: ALL THREE records reached the base handler.
	if len(base.msgs) != 3 {
		t.Fatalf("pass-through broken: %v", base.msgs)
	}
	// Routing: only the audit record reached the spool.
	recs := readActivitySpool(t, e, spool)
	if len(recs) != 1 {
		t.Fatalf("want 1 shipped record, got %d", len(recs))
	}
	r := recs[0]
	if r["event"] != "policy.ref.rejected" || r["tenant"] != "acme" || r["repo"] != "app" {
		t.Fatalf("bad record: %v", r)
	}
	if _, ok := r["ts"]; !ok {
		t.Fatal("record missing ts")
	}
	if r["level"] != "INFO" {
		t.Fatalf("bad level: %v", r["level"])
	}
}

func TestTap_GroupedAttrsAreNested(t *testing.T) {
	e, _, spool := newTestEngine(t, nil)
	logger := slog.New(NewTapHandler(&memHandler{}, e))
	logger.LogAttrs(context.Background(), slog.LevelError, "x.failed",
		slog.Bool("audit", true), slog.String("event", "x.failed"),
		slog.Group("inner", slog.String("k", "v")),
	)
	recs := readActivitySpool(t, e, spool)
	inner, ok := recs[0]["inner"].(map[string]any)
	if !ok || inner["k"] != "v" {
		t.Fatalf("groups not nested: %v", recs[0])
	}
}

func TestTap_WithAttrsPreservesRouting(t *testing.T) {
	e, _, spool := newTestEngine(t, nil)
	logger := slog.New(NewTapHandler(&memHandler{}, e)).With(slog.String("region", "eu"))
	logger.LogAttrs(context.Background(), slog.LevelInfo, "y.event",
		slog.Bool("audit", true), slog.String("event", "y.event"))
	recs := readActivitySpool(t, e, spool)
	if len(recs) != 1 {
		t.Fatalf("With() broke routing: %d records", len(recs))
	}
	if recs[0]["region"] != "eu" {
		t.Fatalf("With() attrs missing from shipped record: %v", recs[0])
	}
}
```

- [ ] **Step 2: Verify compile failure** (`NewTapHandler` undefined).

- [ ] **Step 3: Implement `internal/shiplog/tap.go`**

```go
package shiplog

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"
)

// TapHandler is a fanout slog.Handler: every record passes through to the
// base handler unchanged (stderr behavior is untouched by construction), and
// records carrying audit=true are ADDITIONALLY serialized into the engine's
// activity stream. The engine's own logger must be built on the base handler
// directly, never on the tap (no self-feeding).
type TapHandler struct {
	base   slog.Handler
	engine *Engine
	// attrs accumulated via WithAttrs/WithGroup so shipped records include
	// logger-level context, not just per-record attrs.
	attrs  []slog.Attr
	groups []string
}

// NewTapHandler wraps base; install with slog.SetDefault(slog.New(NewTapHandler(base, engine))).
func NewTapHandler(base slog.Handler, engine *Engine) *TapHandler {
	return &TapHandler{base: base, engine: engine}
}

func (h *TapHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.base.Enabled(ctx, l)
}

func (h *TapHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &TapHandler{
		base:   h.base.WithAttrs(attrs),
		engine: h.engine,
		attrs:  append(append([]slog.Attr(nil), h.attrs...), attrs...),
		groups: h.groups,
	}
}

func (h *TapHandler) WithGroup(name string) slog.Handler {
	return &TapHandler{
		base:   h.base.WithGroup(name),
		engine: h.engine,
		attrs:  h.attrs,
		groups: append(append([]string(nil), h.groups...), name),
	}
}

func (h *TapHandler) Handle(ctx context.Context, rec slog.Record) error {
	err := h.base.Handle(ctx, rec) // pass-through FIRST, unconditionally
	if h.isAudit(rec) {
		if line, mErr := h.marshal(rec); mErr == nil {
			h.engine.Enqueue(StreamActivity, line)
		}
		// marshal failure: drop silently rather than recurse into logging.
	}
	return err
}

func (h *TapHandler) isAudit(rec slog.Record) bool {
	// The audit attr is always inline per-record at the emission sites.
	audit := false
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == "audit" && a.Value.Kind() == slog.KindBool && a.Value.Bool() {
			audit = true
			return false
		}
		return true
	})
	if audit {
		return true
	}
	// Also honor audit=true added via Logger.With (handler-level attrs).
	for _, a := range h.attrs {
		if a.Key == "audit" && a.Value.Kind() == slog.KindBool && a.Value.Bool() {
			return true
		}
	}
	return false
}

// marshal renders {ts, level, event(=message), ...attrs} with groups nested.
func (h *TapHandler) marshal(rec slog.Record) ([]byte, error) {
	root := map[string]any{
		"ts":    rec.Time.UTC().Format(time.RFC3339Nano),
		"level": rec.Level.String(),
		"event": rec.Message,
	}
	// Handler-level attrs first (record attrs win on key collision).
	cur := root
	for _, g := range h.groups {
		next := map[string]any{}
		cur[g] = next
		cur = next
	}
	for _, a := range h.attrs {
		putAttr(cur, a)
	}
	rec.Attrs(func(a slog.Attr) bool {
		putAttr(cur, a)
		return true
	})
	delete(root, "audit") // redundant in the activity stream
	return json.Marshal(root)
}

func putAttr(m map[string]any, a slog.Attr) {
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		inner := map[string]any{}
		for _, ga := range v.Group() {
			putAttr(inner, ga)
		}
		if a.Key == "" { // inline group
			for k, gv := range inner {
				m[k] = gv
			}
			return
		}
		m[a.Key] = inner
		return
	}
	m[a.Key] = v.Any()
}
```

NOTE: when `groups` is non-empty the `delete(root, "audit")` only covers the root level — acceptable (audit emission sites never use WithGroup). If `TestTap_GroupedAttrsAreNested` shows `time.Time`/`Duration` attr values marshaling oddly via `v.Any()`, normalize: `slog.KindTime → Format(RFC3339Nano)`, `slog.KindDuration → milliseconds` — add those two cases to `putAttr` and extend the test.

- [ ] **Step 4: Run** — `go test ./internal/shiplog/ -run TestTap -v -count=1` → PASS; whole package + `-race` → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/shiplog/
git commit -m "feat(shiplog): slog tap handler — audit records fan out to the activity stream"
```

---

### Task 4: UsageEvent API

**Files:**
- Create: `internal/shiplog/usage.go`
- Test: `internal/shiplog/usage_test.go`

- [ ] **Step 1: Write the failing test**

`internal/shiplog/usage_test.go`:

```go
package shiplog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUsage_MarshalsVersionedRecord(t *testing.T) {
	e, _, spool := newTestEngine(t, nil)
	e.Usage(UsageEvent{
		Kind: KindFetch, Tenant: "acme", Repo: "app", Actor: "alice",
		Transport: "https", Bytes: 1024, DurationMS: 250, Status: "ok",
	})
	drainAppends(t, e)
	files := spoolFiles(t, spool, "usage-")
	if len(files) != 1 {
		t.Fatalf("want 1 usage spool file, got %v", files)
	}
	raw, _ := os.ReadFile(filepath.Join(spool, files[0]))
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &m); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{
		"v": float64(1), "kind": "fetch", "tenant": "acme", "repo": "app",
		"actor": "alice", "transport": "https", "bytes": float64(1024),
		"duration_ms": float64(250), "status": "ok",
	} {
		if m[k] != want {
			t.Fatalf("field %s = %v, want %v", k, m[k], want)
		}
	}
	if _, ok := m["ts"]; !ok {
		t.Fatal("missing ts")
	}
}

func TestUsage_NilEngineIsNoOp(t *testing.T) {
	var e *Engine
	e.Usage(UsageEvent{Kind: KindPush}) // must not panic
}
```

- [ ] **Step 2: Verify compile failure.** Step 3: implement `internal/shiplog/usage.go`:

```go
package shiplog

import (
	"encoding/json"
	"time"
)

// Usage kinds. Note: clone is deliberately folded into fetch — they are
// indistinguishable at the transport layer (spec deviation, documented).
const (
	KindFetch       = "fetch"
	KindPush        = "push"
	KindLFSUpload   = "lfs_upload"
	KindLFSDownload = "lfs_download"
	KindBundleServe = "bundle_serve"
	KindPackServe   = "pack_serve"
)

// UsageEvent is one metering record (usage stream, schema v1).
type UsageEvent struct {
	Kind       string `json:"kind"`
	Tenant     string `json:"tenant"`
	Repo       string `json:"repo"`
	Actor      string `json:"actor"`
	Transport  string `json:"transport"` // https|ssh
	Bytes      int64  `json:"bytes"`
	DurationMS int64  `json:"duration_ms"`
	Status     string `json:"status"`            // ok|error|negotiated
	Objects    int    `json:"objects,omitempty"` // LFS batch object count
}

type usageWire struct {
	V  int    `json:"v"`
	TS string `json:"ts"`
	UsageEvent
}

// Usage enqueues one metering record. Nil-engine and marshal failures are
// silent no-ops: metering must never affect serving.
func (e *Engine) Usage(ev UsageEvent) {
	if e == nil {
		return
	}
	line, err := json.Marshal(usageWire{V: 1, TS: time.Now().UTC().Format(time.RFC3339Nano), UsageEvent: ev})
	if err != nil {
		return
	}
	e.Enqueue(StreamUsage, line)
}
```

- [ ] **Step 4: Run** — `go test ./internal/shiplog/ -count=1 && go vet ./internal/shiplog/` → PASS.
- [ ] **Step 5: Commit** — `git add internal/shiplog/ && git commit -m "feat(shiplog): typed UsageEvent API"`

---

### Task 5: serve wiring — flags, spool resolution, boot, shutdown

**Files:**
- Create: `cmd/bucketvcs/shiplog_resolve.go` + `cmd/bucketvcs/shiplog_resolve_test.go`
- Modify: `cmd/bucketvcs/serveflags.go`, `cmd/bucketvcs/serve.go`

- [ ] **Step 1 (TDD): failing test** `cmd/bucketvcs/shiplog_resolve_test.go`:

```go
package main

import (
	"path/filepath"
	"testing"
)

func TestResolveSpoolDir(t *testing.T) {
	// flag wins
	got, err := resolveSpoolDir("/explicit", fakeEnvLookup(map[string]string{}))
	if err != nil || got != "/explicit" {
		t.Fatalf("flag: got %q err %v", got, err)
	}
	// env second
	got, err = resolveSpoolDir("", fakeEnvLookup(map[string]string{"BUCKETVCS_LOG_SPOOL_DIR": "/envdir"}))
	if err != nil || got != "/envdir" {
		t.Fatalf("env: got %q err %v", got, err)
	}
	// XDG_STATE_HOME third
	got, err = resolveSpoolDir("", fakeEnvLookup(map[string]string{"XDG_STATE_HOME": "/xdg"}))
	if err != nil || got != filepath.Join("/xdg", "bucketvcs", "log-spool") {
		t.Fatalf("xdg: got %q err %v", got, err)
	}
	// HOME fallback
	got, err = resolveSpoolDir("", fakeEnvLookup(map[string]string{"HOME": "/home/u"}))
	if err != nil || got != filepath.Join("/home/u", ".local", "state", "bucketvcs", "log-spool") {
		t.Fatalf("home: got %q err %v", got, err)
	}
}
```

ADAPT: read cmd/bucketvcs/authdb.go first — `resolveAuthDB` uses an env-lookup struct (`realEnv()` at :13-27). Mirror its EXACT mechanism (the `fakeEnvLookup` above is a placeholder for however authdb's own tests fake env — grep authdb_test.go / resolve tests and copy that pattern; adjust the test accordingly).

- [ ] **Step 2: implement `cmd/bucketvcs/shiplog_resolve.go`** following resolveAuthDB's shape exactly: flag → `BUCKETVCS_LOG_SPOOL_DIR` → `$XDG_STATE_HOME/bucketvcs/log-spool` → `$HOME/.local/state/bucketvcs/log-spool` → error if no home. Run the test → PASS.

- [ ] **Step 3: flags in serveflags.go** (match house style; env-twin via os.Getenv default like `--auth-db-replica`):

```go
// struct fields:
logShipping      *string
logShipMaxEvents *int
logShipInterval  *time.Duration
logSpoolDir      *string
logSpoolMaxBytes *int64

// registrations:
sf.logShipping = fs.String("log-shipping", envOr("BUCKETVCS_LOG_SHIPPING", "on"),
	`Ship activity (audit) and usage logs to sys/logs/ in --store: "on" (default) or "off". Env: BUCKETVCS_LOG_SHIPPING`)
sf.logShipMaxEvents = fs.Int("log-ship-max-events", 1000,
	"Rotate and ship a log spool file after this many events")
sf.logShipInterval = fs.Duration("log-ship-interval", 15*time.Minute,
	"Rotate and ship a non-empty log spool file after this long since its first event")
sf.logSpoolDir = fs.String("log-spool-dir", "",
	"Local spool directory for unshipped logs (default: state dir; one per instance)")
sf.logSpoolMaxBytes = fs.Int64("log-spool-max-bytes", shiplog.DefaultSpoolMaxBytes,
	"Cap on unshipped spool bytes; oldest pending file is dropped at the cap")
```

(add `envOr(key, def string) string` helper if one doesn't already exist — grep first.)

- [ ] **Step 4: wire serve.go.** Read the current boot region first. Insert, in order:

(a) Validation with the other early checks: `--log-shipping` must be `on` or `off` (else exit 2); `on` without `--store` → exit 2 with `"serve: --log-shipping=on requires --store (or pass --log-shipping=off)"`.

(b) Engine construction + tap install — AFTER `openStore` succeeds and AFTER `defer closeStore(store)` is registered (LIFO ⇒ engine's shutdown defer below runs before the store closes), BEFORE any subsystem starts logging audit events:

```go
var shipEngine *shiplog.Engine // nil ⇒ shipping off; shipEngine.Usage is nil-safe
if *sf.logShipping == "on" {
	spoolDir, serr := resolveSpoolDir(*sf.logSpoolDir, realEnv())
	if serr != nil {
		fmt.Fprintf(stderr, "serve: --log-spool-dir: %v\n", serr)
		return 1
	}
	base := slog.Default().Handler()
	shipEngine, serr = shiplog.New(shiplog.Config{
		Store:         store,
		SpoolDir:      spoolDir,
		MaxEvents:     *sf.logShipMaxEvents,
		MaxAge:        *sf.logShipInterval,
		SpoolMaxBytes: *sf.logSpoolMaxBytes,
		Logger:        slog.New(base), // base handler — never the tap (no self-feed)
	})
	if serr != nil {
		fmt.Fprintf(stderr, "serve: log shipping: %v\n", serr)
		return 1
	}
	slog.SetDefault(slog.New(shiplog.NewTapHandler(base, shipEngine)))
	shipEngine.Start()
	defer func() {
		shCtx, shCancel := context.WithTimeout(context.Background(), *shutdownTimeout)
		defer shCancel()
		if cerr := shipEngine.Close(shCtx); cerr != nil {
			slog.New(base).Error("log shipping shutdown", slog.Any("error", cerr))
		}
	}()
}
```

ORDERING NOTES to verify while editing (explain final placement in the report): (1) this defer must run BEFORE `closeStore(store)` → register it AFTER the store's close-defer; (2) it should run AFTER the HTTP/SSH listeners drain (their shutdown happens in the function body at the end, not via defers, so any defer order satisfies this); (3) it must NOT depend on the authdb — independent of `authS.Close()` ordering. (4) `slog.SetDefault` affects the whole process — install before the M28 replication Prepare so replication audit events (`authdb.replica.restored`) are captured too; if the store opens after replication validation, keep validation order but move engine construction to just after `openStore`.

(c) Make the engine reachable by the gateways: add `UsageSink *shiplog.Engine` (or `Usage func(shiplog.UsageEvent)`) to `gateway.Options` and the sshd config — read both option structs and follow their style; pass `shipEngine` (nil is fine — `Usage` is nil-safe).

- [ ] **Step 5: build + existing tests** — `go build ./... && go test ./cmd/bucketvcs/ ./internal/shiplog/ -count=1` → green.

- [ ] **Step 6: manual sanity**

```bash
TMP=$(mktemp -d); go build -o /tmp/bvx ./cmd/bucketvcs
/tmp/bvx serve --addr=127.0.0.1:0 --store=localfs:$TMP/store --auth-db=$TMP/auth.db --lfs=false &
sleep 2; /tmp/bvx user add --auth-db=$TMP/auth.db --name=x --admin >/dev/null 2>&1 || true
kill -TERM %1; wait
find $TMP/store/objects/sys/logs -name '*.ndjson.gz' | head -3
```
Expected: at least one activity object (serve emits audit events at boot/shutdown — if none fired in this minimal run, trigger one via a repo registration; adapt). Also verify `--log-shipping=off` produces NO sys/logs/ writes.

- [ ] **Step 7: Commit** — `git add cmd/ internal/ && git commit -m "feat(serve): log shipping wiring — flags, tap install, ordered shutdown flush"`

---

### Task 6: usage instrumentation

**Files:**
- Modify: `internal/gateway/` (upload-pack + receive-pack HTTP handlers, LFS batch + verify handlers, proxied_routes.go), `internal/sshd/` (exec paths)
- Test: table-driven handler tests in the respective packages' existing test files + a capturing fake engine

**Survey-verified anchors:** upload-pack service streams via internal/gitproto/uploadpack/service.go:495-525 (instrument at the GATEWAY layer, not gitproto — wrap the http.ResponseWriter); receive-pack completes at internal/gitproto/receivepack/complete.go:603 (again instrument the gateway handler); LFS batch/verify HTTP handlers live in gateway routes; proxied bundle/pack handlers already track `bytesServed` (proxied_routes.go:339-341); actor via `gateway.ActorFromContext(ctx)` (`a.Name` → `a.UserID` → `"anonymous"`).

- [ ] **Step 1: add a counting response writer** in internal/gateway (none exists today; proxied_routes has its own local one — check whether it can be promoted/shared, prefer one shared unexported type in the gateway package):

```go
// countingResponseWriter counts bytes written through an http.ResponseWriter.
type countingResponseWriter struct {
	http.ResponseWriter
	n int64
}

func (w *countingResponseWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.n += int64(n)
	return n, err
}
```

CAVEAT: this drops http.Flusher — git smart-HTTP streaming may rely on Flush. Implement Flush passthrough:

```go
func (w *countingResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
```

- [ ] **Step 2: instrument, one site at a time (TDD per site — write the handler test asserting a UsageEvent lands in a fake sink, then wire):**

For each site emit via the Options sink (nil-safe):

| Site | Kind | Bytes | Status |
|---|---|---|---|
| POST git-upload-pack handler (HTTPS), after the response completes | `fetch` | countingResponseWriter.n | "ok" / "error" by handler outcome |
| POST git-receive-pack handler (HTTPS), after 200 report | `push` | counting READER on the request body (wrap r.Body) | "ok" / "error" |
| SSH upload-pack / receive-pack exec paths | `fetch` / `push` | counting writer on the session channel | per outcome |
| LFS batch handler, after Build returns | `lfs_download` (download op only) | sum of negotiated object sizes | "negotiated", Objects=N |
| LFS verify handler, on success | `lfs_upload` | verified object size | "ok" |
| proxied bundle handler (existing bytesServed) | `bundle_serve` | bytesServed | "ok" |
| proxied pack handler | `pack_serve` | bytesServed | "ok" |

Duration: `time.Since(start)` from handler entry. Tenant/repo: from the route parse already done in each handler. Actor: `ActorFromContext` (HTTPS) / engine request actor (SSH).

Example handler test shape (adapt names to the real handler test files — READ the existing gateway handler tests first and mirror their harness):

```go
type sinkRec struct{ evs []shiplog.UsageEvent }
func (s *sinkRec) Usage(ev shiplog.UsageEvent) { s.evs = append(s.evs, ev) }
// after driving a fetch through the test server:
// require len(sink.evs)==1, evs[0].Kind=="fetch", evs[0].Bytes>0, Tenant/Repo/Actor populated.
```

NOTE: if `gateway.Options` took `*shiplog.Engine` in Task 5, change to the small interface `interface{ Usage(shiplog.UsageEvent) }` now so tests can fake it (nil-check before call); keep `(*Engine).Usage` satisfying it. Update Task 5's wiring accordingly.

- [ ] **Step 3: run the gateway + sshd test suites** — `go test ./internal/gateway/ ./internal/sshd/ -count=1` green; `go build ./...`.

- [ ] **Step 4: Commit** — `git commit -m "feat(usage): metering instrumentation — fetch/push/LFS/bundle/pack serves" `

---

### Task 7: smoke + docs

**Files:**
- Create: `scripts/logship-smoke-localfs.sh`, `docs/operator-guides/log-shipping.md`
- Modify: `docs/upgrade-notes.md`, `README.md`

- [ ] **Step 1: smoke** — follow scripts/authdb-replica-smoke-localfs.sh conventions (port pick, wait_ready, traps, exit 77, marker lines). Phases:
  1. serve with defaults (`--log-ship-interval=5s --log-ship-max-events=5` to force quick rotation) → register user/repo, push a commit, clone it → SIGTERM → assert `objects/sys/logs/activity/**/*.ndjson.gz` contains a push-related audit event AND `objects/sys/logs/usage/**/*.ndjson.gz` contains a `"kind":"push"` and a `"kind":"fetch"` record (gunzip -c | grep). Echo `PHASE1_SHIP_OK`.
  2. kill -9 mid-run with spooled-but-unshipped events (set `--log-ship-interval=10m` so nothing ships, emit a few events, kill -9) → restart → graceful stop → assert the pre-crash events appear in the bucket (boot leftover shipping). Echo `PHASE2_LEFTOVER_OK`.
  3. idle serve (no operations) run for ~3s with `--log-ship-interval=1s` → stop → assert NO new objects beyond phase counts. Echo `PHASE3_IDLE_OK`.
  Marker: `LOGSHIP_SMOKE_OK`. Make executable, run until green.

- [ ] **Step 2: operator guide** `docs/operator-guides/log-shipping.md` (match house style; required sections): what it does (two streams, schemas with examples, the clone→fetch note); enabling/disabling + all 5 flags; the `sys/logs/` layout + lifecycle-rule retention guidance (mirror the sys/authdb wording); delivery semantics (at-least-once, identical-duplicate tolerance, bounded spool drop-oldest + the ERROR/metric signal, what a hard crash can lose); multinode (per-instance files, one spool dir per instance, no coordination); consuming the logs (gunzip + jq one-liners; athena/bigquery-friendly layout note); metrics table (the 5 shiplog_* names); limitations (no querying/UI, no general-log shipping, metering excludes direct-mode LFS transfer bytes — negotiated sizes only, documented honestly).

- [ ] **Step 3: upgrade-notes + README** — prepend a v-next section to docs/upgrade-notes.md: on-by-default behavior change (new writes under `sys/logs/`; `--log-shipping=off` restores old behavior; lifecycle-rule guidance). README: one feature bullet in the existing list style linking the guide.

- [ ] **Step 4: Commit** — `git commit -m "test(smoke)+docs: log shipping smoke + operator guide + upgrade note"`

---

### Task 8: full verification

- [ ] `go build ./... && go vet ./... && go test ./... 2>&1 | tail -5` — green (cloud conformance skips).
- [ ] `go test ./internal/shiplog/ -count=1 -race` — clean.
- [ ] `./scripts/logship-smoke-localfs.sh` → `LOGSHIP_SMOKE_OK`; `./scripts/authdb-replica-smoke-localfs.sh` → still `AUTHDB_REPLICA_SMOKE_OK` (the tap must not break the M28 smoke).
- [ ] Quick interplay check: serve with BOTH `--auth-db-replica=auto` and log shipping on; verify `authdb.replica.*` audit events appear in the shipped activity stream (the tap captures replication's events — listed in the report as evidence).
- [ ] Commit any final fixes.

---

## Plan self-review (done at authoring time)

- **Spec coverage:** engine/rotation/empty-no-op → T1; ship/at-least-once/leftovers/cap/gzip/key-layout → T2; tap+pass-through+no-self-feed → T3; usage schema v1 → T4; flags/defaults/on-by-default/validation/shutdown-ordering/boot → T5; the ~6 metering sites + actor/bytes/duration → T6; smoke incl. crash-leftover + idle-no-op, guide, upgrade note (on-by-default risk from spec §Risks) → T7; observability counters exist as Engine counters (T1/T2) — EXPOSURE as metric lines: emit them from serve at shutdown AND from the ship loop on change is overkill; simplest faithful approach: the engine logs metric lines via its base-handler logger inside ShipPending/enforceCap/drop paths — implementer: add `emitMetric`-style slog lines (`shiplog_shipped_files_total` etc., cumulative values) after each ShipPending in Start's tick, only when counts changed since last tick. Covered in T2's Start loop (add during implementation; assert one metric line in the smoke).
- **Spec deviations encoded:** `clone` folded into `fetch` (T4/T6, documented in guide); LFS download bytes are negotiated-not-transferred in direct mode (guide limitation).
- **Type consistency:** `shiplog.Engine`/`Config`/`Stream`/`UsageEvent`/`NewTapHandler` names consistent across T1-T6; T6 may narrow the Options dependency to an interface — instruction included to back-patch T5's wiring.
- **Placeholder scan:** none; all steps carry code or precise read-then-adapt instructions anchored to verified file:line facts.
