package web

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func auditJSONLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func assertWebAuditShape(t *testing.T, line string) {
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

// TestWebAuditEmitters_AuditShape covers every web Emit* session/OIDC audit
// helper and asserts the audit=true + event==msg shiplog contract.
func TestWebAuditEmitters_AuditShape(t *testing.T) {
	ctx := context.Background()
	type tc struct {
		name  string
		event string
		run   func(*bytes.Buffer)
	}
	tcs := []tc{
		{"session_created", "auth.session.created", func(b *bytes.Buffer) { EmitSessionCreated(ctx, auditJSONLogger(b), "u", "alice", "oidc") }},
		{"session_destroyed", "auth.session.destroyed", func(b *bytes.Buffer) { EmitSessionDestroyed(ctx, auditJSONLogger(b), "u", "alice") }},
		{"oidc_login", "auth.oidc.login", func(b *bytes.Buffer) { EmitOIDCLogin(ctx, auditJSONLogger(b), "u", "alice", "iss", "sub") }},
		{"oidc_identity_linked", "auth.oidc.identity_linked", func(b *bytes.Buffer) {
			EmitOIDCIdentityLinked(ctx, auditJSONLogger(b), "u", "alice", "iss", "sub", "e@x")
		}},
		{"oidc_rejected", "auth.oidc.rejected", func(b *bytes.Buffer) { EmitOIDCRejected(ctx, auditJSONLogger(b), "iss", "no_rule", "e@x") }},
	}
	for _, c := range tcs {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			c.run(&buf)
			if !strings.Contains(buf.String(), c.event) {
				t.Errorf("event %q missing: %s", c.event, buf.String())
			}
			assertWebAuditShape(t, buf.String())
		})
	}
}
