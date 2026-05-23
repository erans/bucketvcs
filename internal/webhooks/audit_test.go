package webhooks_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

func TestEmitDelivered(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	webhooks.EmitDelivered(context.Background(), logger,
		"deliv-1", 42, "push", 1, 87)
	out := buf.String()
	if !strings.Contains(out, "webhooks.delivered") {
		t.Errorf("event name missing: %s", out)
	}
	for _, key := range []string{"delivery_id=deliv-1", "endpoint_id=42", "event_type=push",
		"attempts=1", "duration_ms=87"} {
		if !strings.Contains(out, key) {
			t.Errorf("missing %q in output: %s", key, out)
		}
	}
}

func TestEmitDeadLetter(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	webhooks.EmitDeadLetter(context.Background(), logger,
		"deliv-1", 42, "push", 5, 500)
	out := buf.String()
	if !strings.Contains(out, "webhooks.dead_letter") {
		t.Errorf("event name missing: %s", out)
	}
	if !strings.Contains(out, "total_attempts=5") || !strings.Contains(out, "final_status_code=500") {
		t.Errorf("dead_letter fields missing: %s", out)
	}
}

func TestEmitEnqueueFailed(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError}))
	webhooks.EmitEnqueueFailed(context.Background(), logger,
		"acme", "site", "push", "disk full")
	out := buf.String()
	if !strings.Contains(out, "webhooks.enqueue_failed") {
		t.Errorf("event name missing: %s", out)
	}
	if !strings.Contains(out, "tenant=acme") || !strings.Contains(out, "repo=site") ||
		!strings.Contains(out, "event_type=push") {
		t.Errorf("attrs missing: %s", out)
	}
}
