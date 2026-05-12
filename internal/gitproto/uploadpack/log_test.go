package uploadpack

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestEmitMetric_HasMetricNameAndLabels(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	emitMetric(context.Background(), logger, "bundle_advertised_total", 1, "repo_id", "t/r", "freshness", "current")
	line := buf.String()
	if !bytes.Contains(buf.Bytes(), []byte(`"metric_name":"bundle_advertised_total"`)) {
		t.Errorf("missing metric_name: %s", line)
	}
	if !bytes.Contains(buf.Bytes(), []byte(`"value":1`)) {
		t.Errorf("missing value: %s", line)
	}
	if !bytes.Contains(buf.Bytes(), []byte(`"repo_id":"t/r"`)) {
		t.Errorf("missing repo_id label: %s", line)
	}
	if !bytes.Contains(buf.Bytes(), []byte(`"freshness":"current"`)) {
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

func TestClassifyVia_Direct(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"", "direct"},
		{"https://s3.amazonaws.com/bucket/path/to/bundle.git", "direct"},
		{"https://storage.googleapis.com/bucket/key", "direct"},
		{"https://cdn.example.com/bundle.git?X-Amz-Signature=abc", "direct"},
	}
	for _, c := range cases {
		if got := classifyVia(c.url); got != c.want {
			t.Errorf("classifyVia(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}

func TestClassifyVia_Proxied_Bundle(t *testing.T) {
	url := "https://gw.example.com/_bundle/abc?token=xyz"
	if got := classifyVia(url); got != "proxied" {
		t.Errorf("classifyVia(%q) = %q, want proxied", url, got)
	}
}

func TestClassifyVia_Proxied_Pack(t *testing.T) {
	url := "https://gw.example.com/_pack/def?token=xyz"
	if got := classifyVia(url); got != "proxied" {
		t.Errorf("classifyVia(%q) = %q, want proxied", url, got)
	}
}

func TestClassifyVia_DirectURLWithMisleadingQueryParam(t *testing.T) {
	// S3-shaped URL whose query string contains the proxied marker substring
	// but the URL path does NOT — must classify as direct.
	raw := "https://s3.amazonaws.com/mybucket/pack-abc.bundle?response-content-disposition=attachment;filename=foo/_bundle/x.bundle"
	if got := classifyVia(raw); got != "direct" {
		t.Errorf("classifyVia(%q) = %q, want \"direct\"", raw, got)
	}
}

func TestClassifyVia_MalformedURL(t *testing.T) {
	// url.Parse errors on a URL with an unescaped control character in the
	// authority; the classifier conservatively returns "direct" rather than
	// false-positive matching against the raw string.
	if got := classifyVia("http://[::1%\x00"); got != "direct" {
		t.Errorf("classifyVia returned %q for malformed URL, want \"direct\"", got)
	}
}
