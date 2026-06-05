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

// Stubs replaced in ship.go (Task 2).
func (e *Engine) adoptLeftovers() error { return nil }
func (e *Engine) Close(ctx context.Context) error {
	e.closeOnce.Do(func() { close(e.ch); <-e.intakeDone })
	return nil
}
