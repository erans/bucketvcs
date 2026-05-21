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

func TestEmitSSHAuthenticateMetric_AllOutcomes(t *testing.T) {
	cases := []string{"ok", "forbidden", "disabled", "anon", "error", "client_disconnected"}
	for _, op := range []string{"upload", "download"} {
		for _, result := range cases {
			var buf bytes.Buffer
			EmitSSHAuthenticateMetric(context.Background(), captureLogger(&buf), op, result)
			line := buf.String()
			for _, want := range []string{
				`"metric_name":"lfs_ssh_authenticate_total"`,
				`"value":1`,
				`"op":"` + op + `"`,
				`"result":"` + result + `"`,
			} {
				if !strings.Contains(line, want) {
					t.Errorf("[%s/%s] missing %q in %s", op, result, want, line)
				}
			}
		}
	}
}

func TestEmitLockCreateMetric_OK(t *testing.T) {
	for _, outcome := range []string{"created", "conflict", "error"} {
		var buf bytes.Buffer
		emitLockCreateMetric(context.Background(), captureLogger(&buf), outcome)
		line := buf.String()
		for _, want := range []string{
			`"metric_name":"lfs_locks_created_total"`,
			`"value":1`,
			`"outcome":"` + outcome + `"`,
		} {
			if !strings.Contains(line, want) {
				t.Errorf("[%s] missing %q in %s", outcome, want, line)
			}
		}
	}
}

func TestEmitLockListMetric_OK(t *testing.T) {
	for _, outcome := range []string{"success", "error"} {
		var buf bytes.Buffer
		emitLockListMetric(context.Background(), captureLogger(&buf), outcome)
		line := buf.String()
		for _, want := range []string{
			`"metric_name":"lfs_locks_listed_total"`,
			`"value":1`,
			`"outcome":"` + outcome + `"`,
		} {
			if !strings.Contains(line, want) {
				t.Errorf("[%s] missing %q in %s", outcome, want, line)
			}
		}
	}
}

func TestEmitLockVerifyMetric_OK(t *testing.T) {
	for _, outcome := range []string{"success", "error"} {
		var buf bytes.Buffer
		emitLockVerifyMetric(context.Background(), captureLogger(&buf), outcome)
		line := buf.String()
		for _, want := range []string{
			`"metric_name":"lfs_locks_verified_total"`,
			`"value":1`,
			`"outcome":"` + outcome + `"`,
		} {
			if !strings.Contains(line, want) {
				t.Errorf("[%s] missing %q in %s", outcome, want, line)
			}
		}
	}
}

func TestEmitLockDeleteMetric_OK(t *testing.T) {
	cases := []struct {
		force   bool
		outcome string
	}{
		{false, "owner"},
		{false, "denied"},
		{false, "not_found"},
		{false, "error"},
		{true, "forced"},
		{true, "owner"},
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		emitLockDeleteMetric(context.Background(), captureLogger(&buf), tc.force, tc.outcome)
		line := buf.String()
		forceStr := "false"
		if tc.force {
			forceStr = "true"
		}
		for _, want := range []string{
			`"metric_name":"lfs_locks_deleted_total"`,
			`"value":1`,
			`"force":"` + forceStr + `"`,
			`"outcome":"` + tc.outcome + `"`,
		} {
			if !strings.Contains(line, want) {
				t.Errorf("[force=%v/%s] missing %q in %s", tc.force, tc.outcome, want, line)
			}
		}
	}
}

func TestEmitGCObjectsMarkedMetric_OK(t *testing.T) {
	var buf bytes.Buffer
	EmitGCObjectsMarkedMetric(context.Background(), captureLogger(&buf), "candidate", 42)
	line := buf.String()
	for _, want := range []string{
		`"metric_name":"lfs_gc_objects_marked_total"`,
		`"value":42`,
		`"outcome":"candidate"`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %s", want, line)
		}
	}
}

func TestEmitGCObjectsSweptMetric_AllOutcomes(t *testing.T) {
	cases := []struct {
		outcome string
		count   int64
	}{
		{"deleted", 7},
		{"skipped_retention", 3},
		{"skipped_concurrent", 1},
		{"error", 0},
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		EmitGCObjectsSweptMetric(context.Background(), captureLogger(&buf), tc.outcome, tc.count)
		line := buf.String()
		for _, want := range []string{
			`"metric_name":"lfs_gc_objects_swept_total"`,
			`"outcome":"` + tc.outcome + `"`,
		} {
			if !strings.Contains(line, want) {
				t.Errorf("[%s] missing %q in %s", tc.outcome, want, line)
			}
		}
	}
}

func TestEmitGCBytesSweptMetric_OK(t *testing.T) {
	var buf bytes.Buffer
	EmitGCBytesSweptMetric(context.Background(), captureLogger(&buf), 4096)
	line := buf.String()
	for _, want := range []string{
		`"metric_name":"lfs_gc_bytes_swept_total"`,
		`"value":4096`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %s", want, line)
		}
	}
}
