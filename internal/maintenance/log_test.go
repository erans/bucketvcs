package maintenance

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestEmitStarted_AuditTagged(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	emitStarted(context.Background(), logger, Report{RepoID: "a/b", ManifestVersionAt: 7}, false)

	line := buf.String()
	if !strings.Contains(line, `"audit":true`) {
		t.Errorf("missing audit=true tag: %s", line)
	}
	if !strings.Contains(line, `"event":"maintenance.started"`) {
		t.Errorf("missing event tag: %s", line)
	}
	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if entry["repo_id"] != "a/b" {
		t.Errorf("repo_id = %v, want a/b", entry["repo_id"])
	}
}

func TestEmitMetric_HasMetricNameField(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	emitMetric(context.Background(), logger, "maintenance_runs_total", 1, "outcome", "success")
	line := buf.String()
	if !strings.Contains(line, `"metric_name":"maintenance_runs_total"`) {
		t.Errorf("missing metric_name: %s", line)
	}
	if !strings.Contains(line, `"value":1`) {
		t.Errorf("missing value: %s", line)
	}
	if !strings.Contains(line, `"outcome":"success"`) {
		t.Errorf("missing outcome label: %s", line)
	}
}

func TestEmitCompleted_IncludesPackCounts(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	emitCompleted(context.Background(), logger, Report{
		RepoID: "a/b", Outcome: "success",
		BeforePackCount: 5, AfterPackCount: 1,
	})
	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["audit"] != true {
		t.Errorf("missing audit=true")
	}
	if entry["event"] != "maintenance.completed" {
		t.Errorf("event = %v", entry["event"])
	}
	if int(entry["before_pack_count"].(float64)) != 5 {
		t.Errorf("before_pack_count = %v", entry["before_pack_count"])
	}
}
