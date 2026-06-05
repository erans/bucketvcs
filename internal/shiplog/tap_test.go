package shiplog

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestTap_WithAttrsPreservesRouting(t *testing.T) {	e, _, spool := newTestEngine(t, nil)
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

func TestTap_NormalizesTimeAndDuration(t *testing.T) {
	e, _, spool := newTestEngine(t, nil)
	logger := slog.New(NewTapHandler(&memHandler{}, e))
	logger.LogAttrs(context.Background(), slog.LevelInfo, "z.event",
		slog.Bool("audit", true), slog.String("event", "z.event"),
		slog.Time("when", time.Date(2026, 6, 5, 12, 30, 0, 123456789, time.UTC)),
		slog.Duration("took", 1500*time.Millisecond),
	)
	recs := readActivitySpool(t, e, spool)
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	// time.Time → RFC3339Nano string (sub-second precision preserved, matching ts).
	if got := recs[0]["when"]; got != "2026-06-05T12:30:00.123456789Z" {
		t.Fatalf("time not normalized to RFC3339Nano: %#v", got)
	}
	// time.Duration → milliseconds, decoded by JSON as float64.
	if got := recs[0]["took"]; got != float64(1500) {
		t.Fatalf("duration not normalized to milliseconds: %#v", got)
	}
}
