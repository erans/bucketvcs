package lfs

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// captureLogger returns a slog.Logger that writes JSON to buf, suitable
// for asserting on metric attribute keys and values.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h)
}

func TestEmitBatchRequestMetric_OK(t *testing.T) {
	var buf bytes.Buffer
	logger := captureLogger(&buf)
	emitBatchRequestMetric(context.Background(), logger, "upload", "ok")
	line := buf.String()
	for _, want := range []string{
		`"metric_name":"lfs_batch_requests_total"`,
		`"value":1`,
		`"op":"upload"`,
		`"result":"ok"`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %s", want, line)
		}
	}
}

func TestEmitBatchObjectMetric_AllOutcomes(t *testing.T) {
	cases := []struct {
		op, result string
	}{
		{"upload", "new"},
		{"upload", "exists"},
		{"upload", "error"},
		{"download", "exists"},
		{"download", "missing"},
		{"download", "error"},
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		emitBatchObjectMetric(context.Background(), captureLogger(&buf), tc.op, tc.result)
		line := buf.String()
		if !strings.Contains(line, `"metric_name":"lfs_batch_objects_total"`) {
			t.Errorf("[%s/%s] missing metric_name in %s", tc.op, tc.result, line)
		}
		if !strings.Contains(line, `"op":"`+tc.op+`"`) {
			t.Errorf("[%s/%s] missing op label in %s", tc.op, tc.result, line)
		}
		if !strings.Contains(line, `"result":"`+tc.result+`"`) {
			t.Errorf("[%s/%s] missing result label in %s", tc.op, tc.result, line)
		}
	}
}

func TestEmitObjectServedMetric_OK(t *testing.T) {
	var buf bytes.Buffer
	emitObjectServedMetric(context.Background(), captureLogger(&buf), "upload", "ok")
	line := buf.String()
	for _, want := range []string{
		`"metric_name":"lfs_object_served_total"`,
		`"op":"upload"`,
		`"result":"ok"`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %s", want, line)
		}
	}
}

func TestEmitVerifyRequestMetric_AllOutcomes(t *testing.T) {
	cases := []string{"ok", "missing", "size_mismatch", "error"}
	for _, result := range cases {
		var buf bytes.Buffer
		emitVerifyRequestMetric(context.Background(), captureLogger(&buf), result)
		line := buf.String()
		for _, want := range []string{
			`"metric_name":"lfs_verify_requests_total"`,
			`"value":1`,
			`"result":"` + result + `"`,
		} {
			if !strings.Contains(line, want) {
				t.Errorf("[%s] missing %q in %s", result, want, line)
			}
		}
	}
}
