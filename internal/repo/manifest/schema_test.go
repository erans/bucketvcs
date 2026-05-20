package manifest_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

func TestSchemaGate_Accepts(t *testing.T) {
	cases := []manifest.RootHeader{
		{SchemaVersion: 1, MinReaderVersion: "0.0.0"},
		{SchemaVersion: 1, MinReaderVersion: manifest.SupportedReaderVersion},
		{SchemaVersion: 1, MinReaderVersion: ""}, // empty == accept
	}
	for _, h := range cases {
		if err := manifest.SchemaGate(h); err != nil {
			t.Errorf("SchemaGate(%+v) want nil, got %v", h, err)
		}
	}
}

func TestSchemaGate_AcceptsV2(t *testing.T) {
	h := manifest.RootHeader{SchemaVersion: 2, MinReaderVersion: ""}
	if err := manifest.SchemaGate(h); err != nil {
		t.Errorf("want nil, got %v", err)
	}
}

func TestSchemaGate_RejectsFutureSchemaVersion(t *testing.T) {
	h := manifest.RootHeader{SchemaVersion: 3, MinReaderVersion: "0.1.0"}
	if err := manifest.SchemaGate(h); !errors.Is(err, repo.ErrUnsupportedSchema) {
		t.Errorf("want ErrUnsupportedSchema, got %v", err)
	}
}

func TestSchemaGate_RejectsFutureMinReader(t *testing.T) {
	h := manifest.RootHeader{SchemaVersion: 1, MinReaderVersion: "999.0.0"}
	if err := manifest.SchemaGate(h); !errors.Is(err, repo.ErrUnsupportedSchema) {
		t.Errorf("want ErrUnsupportedSchema, got %v", err)
	}
}

func TestSchemaGate_RejectsZeroSchemaVersion(t *testing.T) {
	h := manifest.RootHeader{SchemaVersion: 0, MinReaderVersion: "0.1.0"}
	if err := manifest.SchemaGate(h); !errors.Is(err, repo.ErrUnsupportedSchema) {
		t.Errorf("want ErrUnsupportedSchema, got %v", err)
	}
}

func TestPackEntry_BitmapKey_RoundTrip(t *testing.T) {
	body := manifest.Body{
		Packs: []manifest.PackEntry{
			{
				PackID:    "0123abcd",
				PackKey:   "tenants/t/repos/r/packs/canonical/0123abcd.pack",
				IdxKey:    "tenants/t/repos/r/packs/canonical/0123abcd.idx",
				BitmapKey: "tenants/t/repos/r/packs/canonical/0123abcd.bitmap",
			},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"bitmap_key":"tenants/t/repos/r/packs/canonical/0123abcd.bitmap"`) {
		t.Errorf("expected bitmap_key in JSON; got %s", raw)
	}
	var got manifest.Body
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Packs[0].BitmapKey != body.Packs[0].BitmapKey {
		t.Errorf("BitmapKey round-trip mismatch: got %q, want %q", got.Packs[0].BitmapKey, body.Packs[0].BitmapKey)
	}
}

func TestPackEntry_BitmapKey_OmittedWhenEmpty(t *testing.T) {
	body := manifest.Body{
		Packs: []manifest.PackEntry{
			{PackID: "0123abcd", PackKey: "p", IdxKey: "i"},
		},
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "bitmap_key") {
		t.Errorf("bitmap_key should be omitted when empty; got %s", raw)
	}
}

func TestSchemaGate_RejectsV2WithFutureMinReaderVersion(t *testing.T) {
	// Belt-and-suspenders: a v2 manifest with a MinReaderVersion higher
	// than this build supports must still reject, even though the
	// SchemaVersion check passes. Guards against a future regression
	// where v2-specific logic accidentally short-circuits the
	// MinReaderVersion gate.
	h := manifest.RootHeader{SchemaVersion: 2, MinReaderVersion: "999.0.0"}
	if err := manifest.SchemaGate(h); !errors.Is(err, repo.ErrUnsupportedSchema) {
		t.Errorf("want ErrUnsupportedSchema, got %v", err)
	}
}
