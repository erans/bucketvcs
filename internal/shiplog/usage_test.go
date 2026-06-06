package shiplog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUsage_MarshalsVersionedRecord(t *testing.T) {
	e, _, spool := newTestEngine(t, nil)
	e.Usage(UsageEvent{
		Kind: KindFetch, Tenant: "acme", Repo: "app", Actor: "alice",
		Transport: "https", Bytes: 1024, DurationMS: 250, Status: "ok",
	})
	drainAppends(t, e)
	files := spoolFiles(t, spool, "usage-")
	if len(files) != 1 {
		t.Fatalf("want 1 usage spool file, got %v", files)
	}
	raw, _ := os.ReadFile(filepath.Join(spool, files[0]))
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &m); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{
		"v": float64(1), "kind": "fetch", "tenant": "acme", "repo": "app",
		"actor": "alice", "transport": "https", "bytes": float64(1024),
		"duration_ms": float64(250), "status": "ok",
	} {
		if m[k] != want {
			t.Fatalf("field %s = %v, want %v", k, m[k], want)
		}
	}
	if _, ok := m["ts"]; !ok {
		t.Fatal("missing ts")
	}
}

func TestUsage_NilEngineIsNoOp(t *testing.T) {
	var e *Engine
	e.Usage(UsageEvent{Kind: KindPush}) // must not panic
}
