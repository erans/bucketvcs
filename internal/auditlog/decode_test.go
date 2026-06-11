package auditlog_test

import (
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auditlog"
)

// gzLines compresses newline-joined lines into gzip bytes.
func gzLines(lines ...string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	for _, l := range lines {
		gz.Write([]byte(l + "\n"))
	}
	gz.Close()
	return buf.Bytes()
}

func TestDecodeGz_TypedFieldsAndAttrs(t *testing.T) {
	ts := "2026-05-22T10:00:00.123456789Z"
	line := `{"ts":"` + ts + `","level":"INFO","event":"repo.created","tenant":"acme","repo":"myrepo","actor":"alice","extra":"val"}`
	events, skipped, err := auditlog.DecodeGz(bytes.NewReader(gzLines(line)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if skipped != 0 {
		t.Fatalf("expected 0 skipped, got %d", skipped)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	e := events[0]
	want, _ := time.Parse(time.RFC3339Nano, ts)
	if !e.Ts.Equal(want) {
		t.Errorf("Ts: got %v want %v", e.Ts, want)
	}
	if e.Level != "INFO" {
		t.Errorf("Level: got %q", e.Level)
	}
	if e.Event != "repo.created" {
		t.Errorf("Event: got %q", e.Event)
	}
	if e.Tenant != "acme" {
		t.Errorf("Tenant: got %q", e.Tenant)
	}
	if e.Repo != "myrepo" {
		t.Errorf("Repo: got %q", e.Repo)
	}
	if e.Actor != "alice" {
		t.Errorf("Actor: got %q", e.Actor)
	}
	// "extra" should be in Attrs
	if v, ok := e.Attrs["extra"]; !ok || v != "val" {
		t.Errorf("Attrs[extra]: got %v ok=%v", v, ok)
	}
	// lifted keys must NOT remain in Attrs
	for _, lifted := range []string{"ts", "level", "event", "tenant", "repo"} {
		if _, ok := e.Attrs[lifted]; ok {
			t.Errorf("Attrs should not contain lifted key %q", lifted)
		}
	}
	// actor/user remain in Attrs for details view
	if _, ok := e.Attrs["actor"]; !ok {
		t.Errorf("Attrs should retain actor key for details view")
	}
}

func TestDecodeGz_UserFallback(t *testing.T) {
	// When actor is absent, user field should be used as Actor.
	ts := "2026-05-22T10:00:00Z"
	line := `{"ts":"` + ts + `","level":"INFO","event":"auth.login","tenant":"t1","repo":"r1","user":"bob"}`
	events, skipped, err := auditlog.DecodeGz(bytes.NewReader(gzLines(line)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if skipped != 0 {
		t.Fatalf("expected 0 skipped, got %d", skipped)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Actor != "bob" {
		t.Errorf("Actor fallback: got %q want %q", events[0].Actor, "bob")
	}
	// user remains in Attrs
	if _, ok := events[0].Attrs["user"]; !ok {
		t.Errorf("Attrs should retain user key")
	}
}

func TestDecodeGz_ActorWinsOverUser(t *testing.T) {
	// actor wins regardless of JSON key order — test with user listed first.
	ts := "2026-05-22T10:00:00Z"
	line := `{"ts":"` + ts + `","level":"INFO","event":"push","user":"bob","actor":"alice"}`
	events, _, err := auditlog.DecodeGz(bytes.NewReader(gzLines(line)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Actor != "alice" {
		t.Errorf("Actor should win over user: got %q", events[0].Actor)
	}
}

func TestDecodeGz_MalformedLinesSkipped(t *testing.T) {
	ts := "2026-05-22T10:00:00Z"
	good := `{"ts":"` + ts + `","level":"INFO","event":"ok","tenant":"t","repo":"r"}`
	bad := `not-valid-json{{`
	events, skipped, err := auditlog.DecodeGz(bytes.NewReader(gzLines(good, bad, good)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if skipped != 1 {
		t.Fatalf("expected 1 skipped, got %d", skipped)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
}

func TestDecodeGz_EmptyLinesSkipped(t *testing.T) {
	ts := "2026-05-22T10:00:00Z"
	good := `{"ts":"` + ts + `","level":"INFO","event":"ok"}`
	// empty line between good lines
	events, skipped, err := auditlog.DecodeGz(bytes.NewReader(gzLines(good, "", good)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// empty lines are skipped but not counted as malformed
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	_ = skipped
}

func TestDecodeGz_NotGzip(t *testing.T) {
	_, _, err := auditlog.DecodeGz(bytes.NewReader([]byte("not gzip data")))
	if err == nil {
		t.Fatal("expected error for non-gzip input, got nil")
	}
}

// TestDecodeGz_OversizedObjectErrors: an object whose decompressed size exceeds
// the guard must return an error rather than silently truncating mid-stream —
// a partially decoded object must be distinguishable from a complete one.
func TestDecodeGz_OversizedObjectErrors(t *testing.T) {
	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	line := []byte(`{"ts":"2026-05-22T12:00:00Z","event":"x","pad":"` + strings.Repeat("p", 1<<20) + "\"}\n")
	for written := 0; written <= 65<<20; written += len(line) {
		if _, err := gz.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	_, _, err := auditlog.DecodeGz(bytes.NewReader(raw.Bytes()))
	if err == nil {
		t.Fatal("expected truncation error for oversized object, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error %q does not indicate the size guard", err)
	}
}
