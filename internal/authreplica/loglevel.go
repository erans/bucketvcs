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
