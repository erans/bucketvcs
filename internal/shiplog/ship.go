package shiplog

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
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
		tick := e.cfg.shipTick
		if tick <= 0 {
			tick = shipTick
		}
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := e.ShipPending(ctx); err != nil {
					e.logger.Warn("shiplog: ship tick", slog.Any("error", err))
				}
				e.emitMetricsIfChanged(ctx)
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
			e.shipErrors.Add(1)
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
	// Fully buffer the gzipped file in memory (files are ≤ MaxEvents lines, so
	// bounded) and hand a re-readable bytes.Reader to PutIfAbsent. Buffering —
	// rather than an io.Pipe — guarantees no dangling goroutine when the store
	// returns without draining the body (e.g. a transient PUT error).
	raw, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("shiplog: open pending: %w", err)
	}
	defer raw.Close()

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	cr := &countingReader{r: raw}
	if _, err := io.Copy(zw, cr); err != nil {
		return fmt.Errorf("shiplog: gzip %s: %w", filepath.Base(path), err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("shiplog: gzip close %s: %w", filepath.Base(path), err)
	}

	if _, err := e.cfg.Store.PutIfAbsent(ctx, key, bytes.NewReader(buf.Bytes()), nil); err != nil &&
		!errors.Is(err, storage.ErrAlreadyExists) {
		return fmt.Errorf("shiplog: put %s: %w", key, err)
	}
	e.shippedFiles.Add(1)
	e.shippedEvents.Add(cr.newlines)
	return nil
}

// countingReader counts newline bytes as data flows through it.
type countingReader struct {
	r        io.Reader
	newlines int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	for _, b := range p[:n] {
		if b == '\n' {
			c.newlines++
		}
	}
	return n, err
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

// ShipErrors reports the cumulative count of per-file ship failures.
func (e *Engine) ShipErrors() int64 { return e.shipErrors.Load() }

// ShippedEvents reports the cumulative count of NDJSON lines shipped.
func (e *Engine) ShippedEvents() int64 { return e.shippedEvents.Load() }

// metricSnapshot holds the cumulative counters last emitted as metric lines.
type metricSnapshot struct {
	shippedFiles  int64
	shippedEvents int64
	shipErrors    int64
	droppedEvents int64
	droppedFiles  int64
}

// emitMetricsIfChanged logs cumulative shiplog_* metric lines via the base
// (non-tap) logger, but only when a counter moved since the last tick. Called
// from the ship loop, so it is the single reader/writer of lastMetrics.
func (e *Engine) emitMetricsIfChanged(ctx context.Context) {
	cur := metricSnapshot{
		shippedFiles:  e.shippedFiles.Load(),
		shippedEvents: e.shippedEvents.Load(),
		shipErrors:    e.shipErrors.Load(),
		droppedEvents: e.dropped.Load(),
		droppedFiles:  e.droppedFiles.Load(),
	}
	if cur == e.lastMetrics {
		return
	}
	emit := func(name string, v int64) {
		e.logger.LogAttrs(ctx, slog.LevelInfo, "metric",
			slog.String("metric_name", name), slog.Int64("value", v))
	}
	if cur.shippedFiles != e.lastMetrics.shippedFiles {
		emit("shiplog_shipped_files_total", cur.shippedFiles)
	}
	if cur.shippedEvents != e.lastMetrics.shippedEvents {
		emit("shiplog_shipped_events_total", cur.shippedEvents)
	}
	if cur.shipErrors != e.lastMetrics.shipErrors {
		emit("shiplog_ship_errors_total", cur.shipErrors)
	}
	if cur.droppedEvents != e.lastMetrics.droppedEvents {
		emit("shiplog_dropped_events_total", cur.droppedEvents)
	}
	if cur.droppedFiles != e.lastMetrics.droppedFiles {
		emit("shiplog_dropped_files_total", cur.droppedFiles)
	}
	e.lastMetrics = cur
}

// Close stops intake, rotates non-empty actives, ships everything pending
// (bounded by ctx), and stops the ship loop. Safe to call once.
func (e *Engine) Close(ctx context.Context) error {
	var shipErr error
	e.closeOnce.Do(func() {
		e.closed.Store(true)
		close(e.done) // intakeLoop drains, rotates non-empty actives, exits
		<-e.intakeDone
		if e.shipCancel != nil {
			e.shipCancel()
			<-e.shipDone
		}
		shipErr = e.ShipPending(ctx)
	})
	return shipErr
}
