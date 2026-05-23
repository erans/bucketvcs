package webhooks_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

func TestEmitDeliveryMetric(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	webhooks.EmitDeliveryMetric(context.Background(), logger, "delivered")
	out := buf.String()
	if !strings.Contains(out, "webhooks_delivery_total") {
		t.Errorf("metric name missing: %s", out)
	}
	if !strings.Contains(out, "outcome=delivered") {
		t.Errorf("outcome label missing: %s", out)
	}
}

func TestEmitQueueDepthGauge(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	webhooks.EmitQueueDepthGauge(context.Background(), logger, "pending", 42)
	out := buf.String()
	if !strings.Contains(out, "webhooks_queue_depth") || !strings.Contains(out, "status=pending") ||
		!strings.Contains(out, "value=42") {
		t.Errorf("queue depth gauge missing fields: %s", out)
	}
}
