package policy

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func captureLogger(buf *bytes.Buffer) *slog.Logger {
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h)
}

func TestEmitRefCheckMetric_AllOutcomes(t *testing.T) {
	for _, outcome := range []string{"ok", "blocked_deletion", "blocked_force_push", "internal_error"} {
		var buf bytes.Buffer
		EmitRefCheckMetric(context.Background(), captureLogger(&buf), outcome)
		line := buf.String()
		for _, want := range []string{
			`"metric_name":"policy_refs_check_total"`,
			`"value":1`,
			`"outcome":"` + outcome + `"`,
		} {
			if !strings.Contains(line, want) {
				t.Errorf("[%s] missing %q in %s", outcome, want, line)
			}
		}
	}
}
