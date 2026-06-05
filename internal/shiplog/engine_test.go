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
