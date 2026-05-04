package manifest_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

func TestRootHeader_JSONRoundTrip(t *testing.T) {
	h := manifest.RootHeader{
		SchemaVersion:    1,
		MinReaderVersion: "0.1.0",
		RepoID:           "r_123",
		RepoFormat: manifest.Format{
			ObjectFormat:  "sha1",
			Compatibility: []string{"sha1"},
		},
		ManifestVersion: 42,
		LatestTx:        "tx_01HW7",
		CreatedAt:       time.Date(2026, 5, 3, 20, 0, 0, 0, time.UTC),
		UpdatedAt:       time.Date(2026, 5, 3, 20, 1, 0, 0, time.UTC),
	}
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	var got manifest.RootHeader
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, h) {
		t.Errorf("round trip mismatch:\nwant %+v\ngot  %+v", h, got)
	}
}

func TestRootHeader_TopLevelKeys(t *testing.T) {
	h := manifest.RootHeader{
		SchemaVersion:    1,
		MinReaderVersion: "0.1.0",
		RepoID:           "r",
		RepoFormat:       manifest.Format{ObjectFormat: "sha1"},
		ManifestVersion:  1,
		LatestTx:         "tx_x",
	}
	b, _ := json.Marshal(h)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{
		"schema_version", "min_reader_version", "repo_id",
		"repo_format", "manifest_version", "latest_tx",
		"created_at", "updated_at",
	} {
		if _, ok := raw[k]; !ok {
			t.Errorf("missing top-level key %q in %s", k, b)
		}
	}
}

func TestHeaderKeyList_ReturnsCopy(t *testing.T) {
	a := manifest.HeaderKeyList()
	if len(a) == 0 {
		t.Fatal("HeaderKeyList must not be empty")
	}
	original := a[0]
	a[0] = "HIJACK"
	b := manifest.HeaderKeyList()
	if b[0] != original {
		t.Errorf("mutation of one slice leaked into another: got %q, want %q", b[0], original)
	}
}
