package localfs

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestSidecarRoundTrip(t *testing.T) {
	in := sidecar{
		Version:     1,
		Sha256:      "deadbeef",
		Size:        1234,
		ContentType: "application/octet-stream",
		ModifiedAt:  time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC),
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out sidecar
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("round-trip mismatch:\nwant %+v\ngot  %+v", in, out)
	}
}

func TestSidecarRejectsUnknownVersion(t *testing.T) {
	b := []byte(`{"version":99,"sha256":"x","size":1,"content_type":"","modified_at":"2026-05-03T12:00:00Z"}`)
	_, err := parseSidecar(b)
	if err == nil {
		t.Fatal("parseSidecar accepted version=99, want error")
	}
	if !errors.Is(err, ErrUnsupportedSidecarSchema) {
		t.Errorf("parseSidecar(version=99) error = %v, want ErrUnsupportedSidecarSchema", err)
	}
}

// TestSidecarParseErrorsAreDistinguishable asserts that a JSON parse
// failure does NOT match ErrUnsupportedSidecarSchema, so headLocked can
// safely self-heal corrupt JSON while failing closed on schema-version
// mismatch.
func TestSidecarParseErrorsAreDistinguishable(t *testing.T) {
	_, err := parseSidecar([]byte("not json"))
	if err == nil {
		t.Fatal("parseSidecar accepted invalid JSON, want error")
	}
	if errors.Is(err, ErrUnsupportedSidecarSchema) {
		t.Errorf("parseSidecar(invalid JSON) wrongly matched ErrUnsupportedSidecarSchema: %v", err)
	}
}
