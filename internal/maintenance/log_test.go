package maintenance

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
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

// TestEmitBundleResultMetrics_Generated verifies that a successful bundle
// generation emits all three metric lines with the correct labels and values.
func TestEmitBundleResultMetrics_Generated(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	br := &BundleResult{
		Generated:     true,
		ByteSize:      1234,
		DurationMS:    2500,
		TriggerReason: "missing",
	}
	emitBundleResultMetrics(context.Background(), logger, "t/r", br)

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 metric lines, got %d: %q", len(lines), buf.String())
	}

	// Line 0: bundle_generated_total
	var e0 map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &e0); err != nil {
		t.Fatalf("parse line 0: %v", err)
	}
	if e0["metric_name"] != "bundle_generated_total" {
		t.Errorf("line 0 metric_name = %v, want bundle_generated_total", e0["metric_name"])
	}
	if e0["outcome"] != "success" {
		t.Errorf("outcome = %v, want success", e0["outcome"])
	}
	if e0["repo_id"] != "t/r" {
		t.Errorf("repo_id = %v, want t/r", e0["repo_id"])
	}
	if e0["trigger_reason"] != "missing" {
		t.Errorf("trigger_reason = %v, want missing", e0["trigger_reason"])
	}

	// Line 1: bundle_generation_duration_seconds (2500/1000 = 2)
	var e1 map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &e1); err != nil {
		t.Fatalf("parse line 1: %v", err)
	}
	if e1["metric_name"] != "bundle_generation_duration_seconds" {
		t.Errorf("line 1 metric_name = %v, want bundle_generation_duration_seconds", e1["metric_name"])
	}
	if int(e1["value"].(float64)) != 2 {
		t.Errorf("duration value = %v, want 2", e1["value"])
	}
	if e1["repo_id"] != "t/r" {
		t.Errorf("line 1 repo_id = %v, want t/r", e1["repo_id"])
	}

	// Line 2: bundle_bytes (1234)
	var e2 map[string]any
	if err := json.Unmarshal([]byte(lines[2]), &e2); err != nil {
		t.Fatalf("parse line 2: %v", err)
	}
	if e2["metric_name"] != "bundle_bytes" {
		t.Errorf("line 2 metric_name = %v, want bundle_bytes", e2["metric_name"])
	}
	if int(e2["value"].(float64)) != 1234 {
		t.Errorf("byte_size value = %v, want 1234", e2["value"])
	}
	if e2["repo_id"] != "t/r" {
		t.Errorf("line 2 repo_id = %v, want t/r", e2["repo_id"])
	}
}

// TestEmitBundleResultMetrics_Failure_OmitsByteSize verifies that a failure
// outcome emits bundle_generated_total and bundle_generation_duration_seconds
// but NOT bundle_bytes (ByteSize is 0 and Generated is false).
func TestEmitBundleResultMetrics_Failure_OmitsByteSize(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	br := &BundleResult{
		Generated:    false,
		ErrorMessage: "boom",
		ByteSize:     0,
		DurationMS:   100,
	}
	emitBundleResultMetrics(context.Background(), logger, "t/r", br)

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 metric lines (no byte_size), got %d: %q", len(lines), buf.String())
	}

	// Line 0: bundle_generated_total with outcome=failure
	var e0 map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &e0); err != nil {
		t.Fatalf("parse line 0: %v", err)
	}
	if e0["metric_name"] != "bundle_generated_total" {
		t.Errorf("line 0 metric_name = %v, want bundle_generated_total", e0["metric_name"])
	}
	if e0["outcome"] != "failure" {
		t.Errorf("outcome = %v, want failure", e0["outcome"])
	}

	// Confirm bundle_bytes is NOT anywhere in the output.
	if strings.Contains(buf.String(), "bundle_bytes") {
		t.Errorf("bundle_bytes should not be emitted on failure, got: %s", buf.String())
	}
}

// TestEmitBundleResultMetrics_Noop_OmitsByteSize verifies that a noop outcome
// (no trigger fired) emits bundle_generated_total and
// bundle_generation_duration_seconds but NOT bundle_bytes.
func TestEmitBundleResultMetrics_Noop_OmitsByteSize(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	br := &BundleResult{
		Generated:     false,
		ErrorMessage:  "",
		TriggerReason: "no_trigger",
	}
	emitBundleResultMetrics(context.Background(), logger, "t/r", br)

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 metric lines (no byte_size), got %d: %q", len(lines), buf.String())
	}

	// Line 0: bundle_generated_total with outcome=noop and trigger_reason=no_trigger
	var e0 map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &e0); err != nil {
		t.Fatalf("parse line 0: %v", err)
	}
	if e0["metric_name"] != "bundle_generated_total" {
		t.Errorf("line 0 metric_name = %v, want bundle_generated_total", e0["metric_name"])
	}
	if e0["outcome"] != "noop" {
		t.Errorf("outcome = %v, want noop", e0["outcome"])
	}
	if e0["trigger_reason"] != "no_trigger" {
		t.Errorf("trigger_reason = %v, want no_trigger", e0["trigger_reason"])
	}

	// Confirm bundle_bytes is NOT anywhere in the output.
	if strings.Contains(buf.String(), "bundle_bytes") {
		t.Errorf("bundle_bytes should not be emitted on noop, got: %s", buf.String())
	}
}

// TestEmitBundleResultMetrics_GeneratedWinsOverError locks in the contract
// that Generated=true always yields outcome="success" even when ErrorMessage
// is also non-empty. This combination does not occur in production today, but
// the classifier switch order must be stable for any future code path.
func TestEmitBundleResultMetrics_GeneratedWinsOverError(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	br := &BundleResult{
		Generated:     true,
		ErrorMessage:  "shouldn't happen but lock the contract",
		ByteSize:      100,
		DurationMS:    1000,
		TriggerReason: "force",
	}
	emitBundleResultMetrics(context.Background(), logger, "t/r", br)

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 metric lines (Generated=true + ByteSize>0), got %d: %q", len(lines), buf.String())
	}

	// Line 0: bundle_generated_total with outcome=success (Generated wins).
	var e0 map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &e0); err != nil {
		t.Fatalf("parse line 0: %v", err)
	}
	if e0["metric_name"] != "bundle_generated_total" {
		t.Errorf("line 0 metric_name = %v, want bundle_generated_total", e0["metric_name"])
	}
	if e0["outcome"] != "success" {
		t.Errorf("outcome = %v, want success (Generated must win over ErrorMessage)", e0["outcome"])
	}

	// Line 2: bundle_bytes must be emitted (Generated && ByteSize > 0 both hold).
	var e2 map[string]any
	if err := json.Unmarshal([]byte(lines[2]), &e2); err != nil {
		t.Fatalf("parse line 2: %v", err)
	}
	if e2["metric_name"] != "bundle_bytes" {
		t.Errorf("line 2 metric_name = %v, want bundle_bytes", e2["metric_name"])
	}
}

// TestEmitBundleResultMetrics_NoopWithByteSize_GatesByteSize locks in that the
// bundle_bytes gate requires BOTH Generated=true AND ByteSize>0. A result
// with ByteSize>0 but Generated=false (invalid by construction in production)
// must NOT emit bundle_bytes.
func TestEmitBundleResultMetrics_NoopWithByteSize_GatesByteSize(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	br := &BundleResult{
		Generated:     false,
		ErrorMessage:  "",
		ByteSize:      999,
		TriggerReason: "no_trigger",
	}
	emitBundleResultMetrics(context.Background(), logger, "t/r", br)

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 metric lines (no byte_size gate), got %d: %q", len(lines), buf.String())
	}

	// Line 0: bundle_generated_total with outcome=noop.
	var e0 map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &e0); err != nil {
		t.Fatalf("parse line 0: %v", err)
	}
	if e0["metric_name"] != "bundle_generated_total" {
		t.Errorf("line 0 metric_name = %v, want bundle_generated_total", e0["metric_name"])
	}
	if e0["outcome"] != "noop" {
		t.Errorf("outcome = %v, want noop", e0["outcome"])
	}

	// bundle_bytes must NOT appear: Generated=false gates the emission even
	// when ByteSize is non-zero.
	if strings.Contains(buf.String(), "bundle_bytes") {
		t.Errorf("bundle_bytes should not be emitted when Generated=false, got: %s", buf.String())
	}
}

// TestEmitBundleGenerated_AuditFields verifies that emitBundleGenerated
// emits a flat-attrs audit line with audit=true, event="bundle.generated",
// and all expected per-bundle fields.
func TestEmitBundleGenerated_AuditFields(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	entry := manifest.BundleEntry{
		ID:                    "bundle_t_r_7_abcd1234",
		BundleHash:            "sha256-aabbccdd",
		TipOID:                "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		CoversManifestVersion: 42,
		ByteSize:              8192,
	}
	art := BundleArtifact{Entry: entry}
	emitBundleGenerated(context.Background(), logger, "t/r", art, 123)

	var e map[string]any
	if err := json.Unmarshal(buf.Bytes(), &e); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if e["audit"] != true {
		t.Errorf("audit = %v, want true", e["audit"])
	}
	if e["event"] != "bundle.generated" {
		t.Errorf("event = %v, want bundle.generated", e["event"])
	}
	if e["repo_id"] != "t/r" {
		t.Errorf("repo_id = %v, want t/r", e["repo_id"])
	}
	if e["bundle_id"] != "bundle_t_r_7_abcd1234" {
		t.Errorf("bundle_id = %v, want bundle_t_r_7_abcd1234", e["bundle_id"])
	}
	if e["bundle_hash"] != "sha256-aabbccdd" {
		t.Errorf("bundle_hash = %v, want sha256-aabbccdd", e["bundle_hash"])
	}
	if e["tip_oid"] != "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef" {
		t.Errorf("tip_oid = %v", e["tip_oid"])
	}
	if int(e["covers_manifest_version"].(float64)) != 42 {
		t.Errorf("covers_manifest_version = %v, want 42", e["covers_manifest_version"])
	}
	if int(e["byte_size"].(float64)) != 8192 {
		t.Errorf("byte_size = %v, want 8192", e["byte_size"])
	}
	if int(e["duration_ms"].(float64)) != 123 {
		t.Errorf("duration_ms = %v, want 123", e["duration_ms"])
	}
}

// TestEmitBundleRetired_AuditFields verifies that emitBundleRetired emits a
// flat-attrs audit line with audit=true, event="bundle.retired", and the
// bundle_id/reason/replaced_by correlation fields.
func TestEmitBundleRetired_AuditFields(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	emitBundleRetired(context.Background(), logger, "t/r", "bundle_t_r_3_oldid", "replaced", "bundle_t_r_7_abcd1234")

	var e map[string]any
	if err := json.Unmarshal(buf.Bytes(), &e); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if e["audit"] != true {
		t.Errorf("audit = %v, want true", e["audit"])
	}
	if e["event"] != "bundle.retired" {
		t.Errorf("event = %v, want bundle.retired", e["event"])
	}
	if e["repo_id"] != "t/r" {
		t.Errorf("repo_id = %v, want t/r", e["repo_id"])
	}
	if e["bundle_id"] != "bundle_t_r_3_oldid" {
		t.Errorf("bundle_id = %v, want bundle_t_r_3_oldid", e["bundle_id"])
	}
	if e["reason"] != "replaced" {
		t.Errorf("reason = %v, want replaced", e["reason"])
	}
	if e["replaced_by"] != "bundle_t_r_7_abcd1234" {
		t.Errorf("replaced_by = %v, want bundle_t_r_7_abcd1234", e["replaced_by"])
	}
}

// TestEmitBundleResultMetrics_SkippedReachability_IsNoop directly covers I-1:
// a skipped_* TriggerReason with a non-empty ErrorMessage must classify as
// "noop", not "failure".
func TestEmitBundleResultMetrics_SkippedReachability_IsNoop(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	br := &BundleResult{
		Generated:     false,
		ErrorMessage:  "load failed",
		TriggerReason: "skipped_reachability_load_error",
	}
	emitBundleResultMetrics(context.Background(), logger, "t/r", br)

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 metric lines, got %d: %q", len(lines), buf.String())
	}

	var e0 map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &e0); err != nil {
		t.Fatalf("parse line 0: %v", err)
	}
	if e0["metric_name"] != "bundle_generated_total" {
		t.Errorf("line 0 metric_name = %v, want bundle_generated_total", e0["metric_name"])
	}
	if e0["outcome"] != "noop" {
		t.Errorf("outcome = %v, want noop (skipped_* must not become failure)", e0["outcome"])
	}
	if e0["trigger_reason"] != "skipped_reachability_load_error" {
		t.Errorf("trigger_reason = %v, want skipped_reachability_load_error", e0["trigger_reason"])
	}
}
