package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
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

// TestHookAuditEmitters_AuditShape covers every hooks.Emit* audit helper and
// asserts the audit=true + event==msg shiplog contract. EmitHookLifecycle's
// message is dynamic, so the contract is verified for each lifecycle event.
func TestHookAuditEmitters_AuditShape(t *testing.T) {
	ctx := context.Background()
	type tc struct {
		name  string
		event string
		run   func(*bytes.Buffer)
	}
	tcs := []tc{
		{"rejected", "policy.hook.rejected", func(b *bytes.Buffer) {
			EmitHookRejected(ctx, jsonLogger(b), "t", "r", "pre-receive", "check.sh", 1, "pid", "actor", []byte("nope"))
		}},
		{"internal_error", "policy.hook.internal_error", func(b *bytes.Buffer) {
			EmitHookInternalError(ctx, jsonLogger(b), "t", "r", "pre-receive", "check.sh", "pid", errors.New("boom"))
		}},
		{"lifecycle_added", "policy.hook.added", func(b *bytes.Buffer) {
			EmitHookLifecycle(ctx, jsonLogger(b), "policy.hook.added", "t", "r", "pre-receive", "check.sh", "actor", 0)
		}},
		{"lifecycle_removed", "policy.hook.removed", func(b *bytes.Buffer) {
			EmitHookLifecycle(ctx, jsonLogger(b), "policy.hook.removed", "t", "r", "pre-receive", "check.sh", "actor", 0)
		}},
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
