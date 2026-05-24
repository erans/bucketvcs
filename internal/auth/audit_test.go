package auth_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

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
