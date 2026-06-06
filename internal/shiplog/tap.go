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
	// Known limitation: handler-level attrs are all written into the deepest
	// WithGroup scope, but slog semantics place attrs added BEFORE a
	// WithGroup at the outer scope (.With("a",1).WithGroup("g") should keep
	// "a" at root). Latent: audit emission sites use inline per-record attrs
	// and never combine With + WithGroup. Fix by tracking attrs per group if
	// that ever changes.
	for _, a := range h.attrs {
		putAttr(cur, a)
	}
	rec.Attrs(func(a slog.Attr) bool {
		putAttr(cur, a)
		return true
	})
	// Strip the "audit" routing key from whichever map holds it.
	// When no WithGroup is active, cur == root so both cases are covered.
	delete(cur, "audit")
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
	switch v.Kind() {
	case slog.KindTime:
		// Consistent with the root "ts" field (v.Any() yields a time.Time,
		// which marshals as RFC3339 but drops sub-second precision).
		m[a.Key] = v.Time().UTC().Format(time.RFC3339Nano)
	case slog.KindDuration:
		// v.Any() marshals a Duration as a raw nanosecond number (e.g.
		// 1.5e+09); milliseconds is the readable, queryable form.
		m[a.Key] = v.Duration().Milliseconds()
	default:
		m[a.Key] = v.Any()
	}
}
