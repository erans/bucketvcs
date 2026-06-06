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

// TestMetricNameKeyIsCanonical is a drift guard: every webhooks metric must
// carry its name under the canonical metric_name attribute, never under the
// legacy name key. See docs/upgrade-notes.md (Unreleased) for the normalization.
func TestMetricNameKeyIsCanonical(t *testing.T) {
	emit := func(logger *slog.Logger) {
		ctx := context.Background()
		webhooks.EmitDeliveryMetric(ctx, logger, "delivered")
		webhooks.EmitQueueDepthGauge(ctx, logger, "pending", 1)
		webhooks.EmitAttemptDuration(ctx, logger, "delivered", 5)
		webhooks.EmitEndpointsActiveGauge(ctx, logger, 1)
		webhooks.EmitWebhookPrunedMetric(ctx, logger, "delivered", 1)
		webhooks.EmitRepoRenamedMetric(ctx, logger, "ok")
		webhooks.EmitEgressDeniedMetric(ctx, logger)
	}
	var buf bytes.Buffer
	emit(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if !strings.Contains(line, "msg=metric") {
			continue
		}
		if !strings.Contains(line, "metric_name=") {
			t.Errorf("metric line missing metric_name= key: %s", line)
		}
		if strings.Contains(line, " name=") {
			t.Errorf("metric line uses legacy name= key: %s", line)
		}
	}
}
