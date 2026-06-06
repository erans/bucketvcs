package webhooks_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

func jsonLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func assertAuditShape(t *testing.T, line string) {
	t.Helper()
	var rec map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &rec); err != nil {
		t.Fatalf("not JSON: %v (%s)", err, line)
	}
	if a, ok := rec["audit"].(bool); !ok || !a {
		t.Errorf("audit attr missing or not true: %s", line)
	}
	if rec["event"] != rec["msg"] {
		t.Errorf("event (%v) != msg (%v): %s", rec["event"], rec["msg"], line)
	}
}

// TestWebhookAuditEmitters_AuditShape covers every webhooks.Emit* audit helper
// and asserts the audit=true + event==msg shiplog contract.
func TestWebhookAuditEmitters_AuditShape(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1700000000, 0)
	type tc struct {
		name  string
		event string
		run   func(*bytes.Buffer)
	}
	tcs := []tc{
		{"delivered", "webhooks.delivered", func(b *bytes.Buffer) { webhooks.EmitDelivered(ctx, jsonLogger(b), "d", 1, "push", 1, 1) }},
		{"failed", "webhooks.failed", func(b *bytes.Buffer) { webhooks.EmitFailed(ctx, jsonLogger(b), "d", 1, "push", 1, 500, "boom", 1) }},
		{"dead_letter", "webhooks.dead_letter", func(b *bytes.Buffer) { webhooks.EmitDeadLetter(ctx, jsonLogger(b), "d", 1, "push", 5, 500) }},
		{"enqueue_failed", "webhooks.enqueue_failed", func(b *bytes.Buffer) { webhooks.EmitEnqueueFailed(ctx, jsonLogger(b), "t", "r", "push", "boom") }},
		{"endpoint_created", "webhooks.endpoint_created", func(b *bytes.Buffer) {
			webhooks.EmitEndpointCreated(ctx, jsonLogger(b), 1, "t", "r", "https://x", "push")
		}},
		{"endpoint_removed", "webhooks.endpoint_removed", func(b *bytes.Buffer) { webhooks.EmitEndpointRemoved(ctx, jsonLogger(b), 1, "t", "r") }},
		{"endpoint_secret_rotated", "webhooks.endpoint_secret_rotated", func(b *bytes.Buffer) { webhooks.EmitEndpointSecretRotated(ctx, jsonLogger(b), 1, "t", "r", "actor") }},
		{"pruned", "webhooks.pruned", func(b *bytes.Buffer) { webhooks.EmitWebhookPruned(ctx, jsonLogger(b), 1, 0, now, now, false, "actor") }},
		{"egress_denied", "webhooks.egress_denied", func(b *bytes.Buffer) { webhooks.EmitEgressDenied(ctx, jsonLogger(b), "d", 1, "h", "1.2.3.4", "ip", "") }},
	}
	for _, c := range tcs {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			c.run(&buf)
			if !strings.Contains(buf.String(), c.event) {
				t.Errorf("event %q missing: %s", c.event, buf.String())
			}
			assertAuditShape(t, buf.String())
		})
	}
}

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
