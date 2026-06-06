package auth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// assertAuditShape decodes a single JSON log line and asserts the
// log-shipping contract: audit==true and event==message. This is what the
// internal/shiplog tap keys on; every genuine audit emitter must satisfy it.
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

func jsonLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// TestAuthAuditEmitters_AuditShape covers every auth.Emit* audit helper and
// asserts the audit=true + event==msg contract that the shiplog tap requires.
func TestAuthAuditEmitters_AuditShape(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		emit  func(*slog.Logger)
		event string
	}{
		{"token_rotated", func(l *slog.Logger) {
			auth.EmitTokenRotated(ctx, l, "id", "u", "actor")
		}, "auth.token.rotated"},
		{"scope_denied", func(l *slog.Logger) {
			auth.EmitScopeDenied(ctx, l, "u", "pre", "t", "r", "op", auth.ScopeRepoWrite, auth.ScopeRepoRead)
		}, "auth.scope.denied"},
		{"ratelimit_hit", func(l *slog.Logger) {
			auth.EmitRateLimitHit(ctx, l, "1.2.3.4", "u", "ip", 60, "https")
		}, "auth.ratelimit.hit"},
		{"oidc_exchanged", func(l *slog.Logger) {
			auth.EmitOIDCExchanged(ctx, l, "iss", "sub", "t", "r", auth.ScopeRepoRead, 900)
		}, "auth.oidc.exchanged"},
		{"oidc_rejected", func(l *slog.Logger) {
			auth.EmitOIDCRejected(ctx, l, "iss", "1.2.3.4", "no_rule")
		}, "auth.oidc.rejected"},
		{"password_set", func(l *slog.Logger) {
			auth.EmitPasswordSet(ctx, l, "u")
		}, "auth.password.set"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			tc.emit(jsonLogger(&buf))
			if !strings.Contains(buf.String(), tc.event) {
				t.Errorf("event name %q missing: %s", tc.event, buf.String())
			}
			assertAuditShape(t, buf.String())
		})
	}
}

func TestEmitTokenRotated(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	auth.EmitTokenRotated(context.Background(), logger,
		"bvtk_abc123", "user-1", "alice")
	out := buf.String()
	if !strings.Contains(out, "auth.token.rotated") {
		t.Errorf("event name missing: %s", out)
	}
	for _, key := range []string{"token_id=bvtk_abc123", "user_id=user-1", "actor=alice"} {
		if !strings.Contains(out, key) {
			t.Errorf("missing %q in output: %s", key, out)
		}
	}
}

func TestEmitScopeDenied(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	auth.EmitScopeDenied(context.Background(), logger,
		"user-1", "bvtk_abc", "acme", "site", "receive-pack",
		auth.ScopeRepoWrite, auth.ScopeRepoRead)
	out := buf.String()
	if !strings.Contains(out, "auth.scope.denied") {
		t.Errorf("event name missing: %s", out)
	}
	for _, key := range []string{"user_id=user-1", "token_id_prefix=bvtk_abc",
		"tenant=acme", "repo=site", "operation=receive-pack",
		"required_scope=repo:write", "granted_scopes=repo:read"} {
		if !strings.Contains(out, key) {
			t.Errorf("missing %q in output: %s", key, out)
		}
	}
}

func TestEmitRateLimitHit(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	auth.EmitRateLimitHit(context.Background(), logger,
		"1.2.3.4", "alice", "ip", 60, "https")
	out := buf.String()
	if !strings.Contains(out, "auth.ratelimit.hit") {
		t.Errorf("event name missing: %s", out)
	}
	for _, key := range []string{"ip=1.2.3.4", "user=alice", "bucket=ip",
		"retry_after_sec=60", "transport=https"} {
		if !strings.Contains(out, key) {
			t.Errorf("missing %q in output: %s", key, out)
		}
	}
}
