package sweeps_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/gc/sweeps"
)

func TestRecord_MarshalRoundTrip(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	r := sweeps.Record{
		SchemaVersion:                1,
		SweepID:                      "sw_01HZSAMPLE",
		MarkID:                       "mk_01HZSAMPLE",
		StartedAt:                    now,
		CompletedAt:                  now.Add(2 * time.Second),
		CurrentManifestVersion:       1234,
		CurrentManifestObjectVersion: "vtok",
		Deleted: sweeps.Deleted{
			TxRecords:      []string{"k1"},
			CanonicalPacks: []string{"k2"},
			Indexes:        []string{"k3"},
		},
		Skipped: []sweeps.SkippedEntry{
			{Key: "kx", Category: "canonical_packs", Reason: "revived"},
		},
		Errors: []sweeps.ErrorEntry{
			{Key: "ky", Category: "indexes", Error: "synthetic"},
		},
	}
	b, err := r.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	var got sweeps.Record
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.SweepID != r.SweepID || got.MarkID != r.MarkID {
		t.Fatalf("ID mismatch: got %+v want %+v", got, r)
	}

	// Verify on-disk key order matches spec (§8.2).
	idx := func(s, sub string) int {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return i
			}
		}
		return -1
	}
	jsonStr := string(b)
	wantOrder := []string{
		`"schema_version"`,
		`"sweep_id"`,
		`"mark_id"`,
		`"started_at"`,
		`"completed_at"`,
		`"current_manifest_version"`,
		`"current_manifest_object_version"`,
		`"deleted"`,
		`"skipped"`,
		`"errors"`,
	}
	for i := 1; i < len(wantOrder); i++ {
		if idx(jsonStr, wantOrder[i]) < idx(jsonStr, wantOrder[i-1]) {
			t.Errorf("key %s appears before %s on disk; want declaration order",
				wantOrder[i], wantOrder[i-1])
		}
	}
}
