package gc_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gc"
)

func TestLog_AuditTagged(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	gc.LogMarkCompleted(logger, "acme/site", "mk_01HZ", 1234, 8, 2, 1)
	if !strings.Contains(buf.String(), `"audit":true`) {
		t.Fatalf("expected audit:true in log line, got: %s", buf.String())
	}
	var top map[string]any
	if err := json.Unmarshal(buf.Bytes(), &top); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if top["repo_id"] != "acme/site" {
		t.Errorf("repo_id = %v, want acme/site", top["repo_id"])
	}
}
