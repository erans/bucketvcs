package doctor_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/doctor"
)

func checks() []doctor.Check {
	return []doctor.Check{
		{Name: "alpha.ok", Run: func(context.Context) doctor.Result {
			return doctor.Result{Status: doctor.StatusOK, Detail: "fine"}
		}},
		{Name: "beta.warn", Run: func(context.Context) doctor.Result {
			return doctor.Result{Status: doctor.StatusWarn, Detail: "meh"}
		}},
		{Name: "gamma.fail", Run: func(context.Context) doctor.Result {
			return doctor.Result{Status: doctor.StatusFail, Detail: "broken thing"}
		}},
		{Name: "delta.skip", Run: func(context.Context) doctor.Result {
			return doctor.Result{Status: doctor.StatusSkip, Detail: "disabled"}
		}},
	}
}

func TestRunHumanOutput(t *testing.T) {
	var buf bytes.Buffer
	failed := doctor.Run(context.Background(), &buf, false, checks())
	if failed != 1 {
		t.Fatalf("failed=%d, want 1 (warn/skip must not count)", failed)
	}
	out := buf.String()
	for _, want := range []string{"OK", "alpha.ok", "WARN", "FAIL", "gamma.fail", "broken thing", "SKIP"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRunJSONOutput(t *testing.T) {
	var buf bytes.Buffer
	_ = doctor.Run(context.Background(), &buf, true, checks())
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("want 4 NDJSON lines, got %d:\n%s", len(lines), buf.String())
	}
	var first struct {
		Check  string `json:"check"`
		Status string `json:"status"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line 1 not JSON: %v", err)
	}
	if first.Check != "alpha.ok" || first.Status != "ok" || first.Detail != "fine" {
		t.Fatalf("unexpected first line: %+v", first)
	}
}
