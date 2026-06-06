package shiplog

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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
	// key; PutIfAbsent → ErrAlreadyExists must count as shipped (file removed)
	// but must NOT increment shippedFiles/shippedEvents (no double-counting).
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
	// Duplicate re-ship must NOT double-count metrics.
	if got := e.ShippedEvents(); got != 0 {
		t.Fatalf("shippedEvents must not advance on ErrAlreadyExists re-ship, got %d", got)
	}
	// shippedFiles is not exported directly; verify via the internal field.
	if got := e.shippedFiles.Load(); got != 0 {
		t.Fatalf("shippedFiles must not advance on ErrAlreadyExists re-ship, got %d", got)
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

// TestEnforceCap_DropsChronologicallyOldest is the M-final regression: when the
// spool holds pending files from two instance IDs whose lexical order disagrees
// with their rotation timestamps, enforceCap must drop the chronologically
// OLDEST file (smallest ts), not the lexically-first one. The instance segment
// precedes the ts in the name, so a naive sort.Strings would key on instance-id
// and drop the wrong (newer) file after a crash+restart.
func TestEnforceCap_DropsChronologicallyOldest(t *testing.T) {
	st, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	spool := t.TempDir()

	// "ffffffff" instance, OLDEST ts (Jan) — lexically sorts LAST.
	oldName := "usage-ffffffff-000001.pending.20260101T000000.ndjson"
	// "00000000" instance, NEWEST ts (Jun) — lexically sorts FIRST.
	newName := "usage-00000000-000001.pending.20260601T000000.ndjson"

	// Each file is 50 bytes so the pair (100B) exceeds a 60B cap and exactly
	// one drop restores the invariant.
	body := []byte(strings.Repeat("x", 49) + "\n")
	for _, n := range []string{oldName, newName} {
		if err := os.WriteFile(filepath.Join(spool, n), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	e, err := New(Config{
		Store:         st,
		SpoolDir:      spool,
		SpoolMaxBytes: 60, // fits one 50B file, not both
		pauseIntake:   true,
	})
	if err != nil {
		t.Fatal(err)
	}

	e.enforceCap()

	// The chronologically OLDEST (Jan / ffffffff) must be gone; the NEWEST
	// (Jun / 00000000) must survive.
	if _, err := os.Stat(filepath.Join(spool, oldName)); !os.IsNotExist(err) {
		t.Fatalf("chronologically oldest file should have been dropped, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(spool, newName)); err != nil {
		t.Fatalf("chronologically newest file must survive, stat err=%v", err)
	}
	if got := e.DroppedFiles(); got != 1 {
		t.Fatalf("want exactly 1 dropped file, got %d", got)
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

func TestShip_ErrorCounterIncrementsOnFailedPut(t *testing.T) {
	base, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fs := &failNPuts{ObjectStore: base, n: 2} // both files in the first ship fail
	e, _, _ := newTestEngine(t, func(c *Config) { c.Store = fs })
	for i := 0; i < 5; i++ {
		e.Enqueue(StreamUsage, []byte(`{"x":1}`))
	}
	for i := 0; i < 5; i++ {
		e.Enqueue(StreamActivity, []byte(`{"y":1}`))
	}
	drainAppends(t, e)
	if err := e.ShipPending(context.Background()); err == nil {
		t.Fatal("ship should report the transient failures")
	}
	if got := e.ShipErrors(); got != 2 {
		t.Fatalf("want 2 ship errors, got %d", got)
	}
}

// nonDrainingFailStore fails every PutIfAbsent WITHOUT reading the body — the
// exact condition that leaked an io.Pipe goroutine before the I1 buffering fix.
type nonDrainingFailStore struct {
	storage.ObjectStore
}

func (s *nonDrainingFailStore) PutIfAbsent(ctx context.Context, key string, body io.Reader, opts *storage.PutOptions) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, storage.ErrTransient
}

// TestShip_NoGoroutineLeakOnUndrainedBody is the I1 regression: when the store
// returns without consuming the body, repeated ShipPending calls must not
// accumulate goroutines (the old io.Pipe writer blocked forever on pw.Write).
func TestShip_NoGoroutineLeakOnUndrainedBody(t *testing.T) {
	base, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fs := &nonDrainingFailStore{ObjectStore: base}
	e, _, _ := newTestEngine(t, func(c *Config) { c.Store = fs })
	for i := 0; i < 5; i++ {
		e.Enqueue(StreamUsage, []byte(`{"x":1}`))
	}
	drainAppends(t, e)

	// Prime once so any lazily-spawned runtime goroutines settle.
	_ = e.ShipPending(context.Background())
	runtime.GC()
	before := runtime.NumGoroutine()
	for i := 0; i < 25; i++ {
		_ = e.ShipPending(context.Background()) // each fails without draining
	}
	runtime.GC()
	time.Sleep(20 * time.Millisecond) // let any leaked goroutines register
	after := runtime.NumGoroutine()
	if after > before+2 { // small slack for the runtime
		t.Fatalf("goroutine leak: before=%d after=%d", before, after)
	}
}

// TestShip_ShippedEventsAdvances is the I2 regression: a successful ship counts
// the file's NDJSON lines into shippedEvents.
func TestShip_ShippedEventsAdvances(t *testing.T) {
	e, _, _ := newTestEngine(t, nil) // MaxEvents=5
	for i := 0; i < 5; i++ {
		e.Enqueue(StreamActivity, []byte(fmt.Sprintf(`{"i":%d}`, i)))
	}
	drainAppends(t, e)
	if got := e.ShippedEvents(); got != 0 {
		t.Fatalf("shippedEvents should start at 0, got %d", got)
	}
	if err := e.ShipPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := e.ShippedEvents(); got != 5 {
		t.Fatalf("want 5 shipped events, got %d", got)
	}
}

// TestShipLoop_StartTickShipsAndCloseRaces is the M1 regression: drive the ship
// loop with a fast tick, prove an object lands, then Close concurrently with
// ongoing Enqueues. Run the whole package under -race.
func TestShipLoop_StartTickShipsAndCloseRaces(t *testing.T) {
	st, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(Config{
		Store:     st,
		SpoolDir:  t.TempDir(),
		MaxEvents: 5,
		MaxAge:    time.Hour,
		shipTick:  10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.Start()

	// Background Enqueues keep pushing past MaxEvents so files rotate + ship.
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				e.Enqueue(StreamUsage, []byte(`{"loop":1}`))
				time.Sleep(time.Millisecond)
			}
		}
	}()

	// Bounded poll until the ship loop lands at least one object.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if len(listKeys(t, st, "sys/logs/usage/")) > 0 {
			break
		}
		if time.Now().After(deadline) {
			close(stop)
			wg.Wait()
			t.Fatal("ship loop never produced an object")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Close concurrently with ongoing Enqueues (the C1 lifecycle path).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond) // a few post-close Enqueues
	close(stop)
	wg.Wait()
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

// metricRecorder is a slog.Handler that collects every metric_name attr value
// emitted on "metric" messages. Used to verify emitMetricsIfChanged behaviour.
type metricRecorder struct {
	mu      sync.Mutex
	emitted []string // metric_name values in emission order
}

func (r *metricRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (r *metricRecorder) WithAttrs([]slog.Attr) slog.Handler       { return r }
func (r *metricRecorder) WithGroup(string) slog.Handler            { return r }
func (r *metricRecorder) Handle(_ context.Context, rec slog.Record) error {
	if rec.Message != "metric" {
		return nil
	}
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == "metric_name" {
			r.mu.Lock()
			r.emitted = append(r.emitted, a.Value.String())
			r.mu.Unlock()
			return false
		}
		return true
	})
	return nil
}

func (r *metricRecorder) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.emitted))
	copy(out, r.emitted)
	return out
}

func (r *metricRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.emitted)
}

// TestEmitMetricsIfChanged_OnlyChangedCountersEmit verifies the diff-gate:
// emitMetricsIfChanged emits ONLY the counters that moved since the last call.
func TestEmitMetricsIfChanged_OnlyChangedCountersEmit(t *testing.T) {
	rec := &metricRecorder{}
	logger := slog.New(rec)
	e, _, _ := newTestEngine(t, func(c *Config) { c.Logger = logger })
	ctx := context.Background()

	// --- first call: no counters moved → nothing emitted ---
	e.emitMetricsIfChanged(ctx)
	if n := rec.count(); n != 0 {
		t.Fatalf("call on zero counters: want 0 emissions, got %d: %v", n, rec.names())
	}

	// --- bump shippedFiles + shippedEvents via a real ship ---
	for i := 0; i < 5; i++ {
		e.Enqueue(StreamActivity, []byte(`{"i":1}`))
	}
	drainAppends(t, e)
	if err := e.ShipPending(ctx); err != nil {
		t.Fatalf("ship failed: %v", err)
	}
	if e.shippedFiles.Load() == 0 {
		t.Fatal("ship did not advance shippedFiles")
	}

	// --- second call: shippedFiles + shippedEvents changed → both emitted ---
	before := rec.count()
	e.emitMetricsIfChanged(ctx)
	after := rec.count()
	gotNames := rec.names()[before:after]
	hasShippedFiles := false
	hasShippedEvents := false
	for _, n := range gotNames {
		switch n {
		case "shiplog_shipped_files_total":
			hasShippedFiles = true
		case "shiplog_shipped_events_total":
			hasShippedEvents = true
		}
	}
	if !hasShippedFiles {
		t.Errorf("second call: shiplog_shipped_files_total not emitted (got %v)", gotNames)
	}
	if !hasShippedEvents {
		t.Errorf("second call: shiplog_shipped_events_total not emitted (got %v)", gotNames)
	}

	// --- third call: counters unchanged → nothing new emitted ---
	stable := rec.count()
	e.emitMetricsIfChanged(ctx)
	if n := rec.count(); n != stable {
		t.Fatalf("third call (no movement): want 0 new emissions, got %d new: %v",
			n-stable, rec.names()[stable:])
	}

	// --- bump dropped counter directly, then call again ---
	e.dropped.Add(1)
	dropBase := rec.count()
	e.emitMetricsIfChanged(ctx)
	dropNames := rec.names()[dropBase:]
	if len(dropNames) != 1 || dropNames[0] != "shiplog_dropped_events_total" {
		t.Fatalf("fourth call (only dropped bumped): want [shiplog_dropped_events_total], got %v", dropNames)
	}
}
