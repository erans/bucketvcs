package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestEmitMetric_HasMetricNameAndLabels(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	emitMetric(context.Background(), logger, "bundle_advertised_total", 1, "repo_id", "t/r", "freshness", "current")
	line := buf.String()
	if !strings.Contains(line, `"metric_name":"bundle_advertised_total"`) {
		t.Errorf("missing metric_name: %s", line)
	}
	if !strings.Contains(line, `"value":1`) {
		t.Errorf("missing value: %s", line)
	}
	if !strings.Contains(line, `"repo_id":"t/r"`) {
		t.Errorf("missing repo_id label: %s", line)
	}
	if !strings.Contains(line, `"freshness":"current"`) {
		t.Errorf("missing freshness label: %s", line)
	}
}

func TestEmitMetric_NilLoggerDoesNotPanic(t *testing.T) {
	// Must not panic when logger is nil; falls back to slog.Default().
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("emitMetric panicked with nil logger: %v", r)
		}
	}()
	emitMetric(context.Background(), nil, "x", 1)
}

func TestEmitBundleURIAdvertised_AuditFields(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	emitBundleURIAdvertised(context.Background(), logger, "t/r", "current", "proxied", 1, "abc123")
	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if entry["audit"] != true {
		t.Errorf("missing audit=true")
	}
	if entry["event"] != "bundle.uri.advertised" {
		t.Errorf("event = %v, want bundle.uri.advertised", entry["event"])
	}
	if entry["repo_id"] != "t/r" {
		t.Errorf("repo_id = %v", entry["repo_id"])
	}
	if entry["freshness"] != "current" {
		t.Errorf("freshness = %v", entry["freshness"])
	}
	if entry["via"] != "proxied" {
		t.Errorf("via = %v", entry["via"])
	}
	if int(entry["bundle_count"].(float64)) != 1 {
		t.Errorf("bundle_count = %v", entry["bundle_count"])
	}
	if entry["first_tip_oid"] != "abc123" {
		t.Errorf("first_tip_oid = %v", entry["first_tip_oid"])
	}
}

func TestEmitProxiedURLServed_AuditFields(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	emitProxiedURLServed(context.Background(), logger, "bundle", "sha256-aa", 12345, 206, true)
	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if entry["audit"] != true {
		t.Errorf("missing audit=true")
	}
	if entry["event"] != "proxied.url.served" {
		t.Errorf("event = %v, want proxied.url.served", entry["event"])
	}
	if entry["kind"] != "bundle" {
		t.Errorf("kind = %v", entry["kind"])
	}
	if entry["hash"] != "sha256-aa" {
		t.Errorf("hash = %v", entry["hash"])
	}
	if int64(entry["bytes_served"].(float64)) != 12345 {
		t.Errorf("bytes_served = %v", entry["bytes_served"])
	}
	if int(entry["status_code"].(float64)) != 206 {
		t.Errorf("status_code = %v", entry["status_code"])
	}
	if entry["range_request"] != true {
		t.Errorf("range_request = %v", entry["range_request"])
	}
}
