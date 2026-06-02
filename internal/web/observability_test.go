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
	EmitLoginMetric(context.Background(), logger, "success")
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
