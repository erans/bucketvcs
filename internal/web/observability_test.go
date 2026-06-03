package web

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestEmitLoginMetric(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	EmitLoginMetric(context.Background(), logger, "success", "password")
	out := buf.String()
	if !strings.Contains(out, "web_login_total") || !strings.Contains(out, "success") {
		t.Fatalf("metric not emitted: %s", out)
	}
}

func TestEmitSessionCreated(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	EmitSessionCreated(context.Background(), logger, "u1", "alice", "password")
	if !strings.Contains(buf.String(), "auth.session.created") {
		t.Fatalf("audit not emitted: %s", buf.String())
	}
}

func TestEmitLoginMetric_Provider(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	EmitLoginMetric(context.Background(), logger, "success", "oidc")
	out := buf.String()
	if !strings.Contains(out, "web_login_total") || !strings.Contains(out, "oidc") {
		t.Fatalf("metric missing provider: %s", out)
	}
}

func TestEmitOIDCEvents(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	EmitOIDCLogin(context.Background(), logger, "u1", "alice", "https://i", "sub")
	EmitOIDCIdentityLinked(context.Background(), logger, "u1", "alice", "https://i", "sub", "a@x.com")
	EmitOIDCRejected(context.Background(), logger, "https://i", "no_user", "a@x.com")
	out := buf.String()
	for _, want := range []string{"auth.oidc.login", "auth.oidc.identity_linked", "auth.oidc.rejected", "no_user"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q: %s", want, out)
		}
	}
}
