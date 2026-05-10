package marks_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/gc/marks"
)

func TestRecord_MarshalRoundTrip(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	r := marks.Record{
		SchemaVersion:                1,
		MarkID:                       "mk_01HZSAMPLE",
		PreviousMarkID:               "mk_01HZPREV",
		StartedAt:                    now,
		CompletedAt:                  now.Add(2 * time.Second),
		CurrentManifestVersion:       1234,
		CurrentManifestObjectVersion: "vtok",
		RetentionSeconds:             604800,
		TxOrphanSweepArmed:           true,
		Candidates: marks.Candidates{
			TxRecords: []marks.TxCandidate{
				{Key: "tenants/a/repos/r/tx/tx_x.json", FirstSeenUnreachableAt: now},
			},
			CanonicalPacks: []marks.PackCandidate{
				{
					Key:                    "tenants/a/repos/r/packs/canonical/abc.pack",
					FirstSeenUnreachableAt: now,
					LastSeenReachableAt:    timePtr(now.Add(-time.Hour)),
					MarkManifestVersion:    1233,
				},
			},
			Indexes: []marks.IndexCandidate{
				{Key: "tenants/a/repos/r/indexes/object-map/x.bvom", FirstSeenUnreachableAt: now},
			},
		},
	}
	b, err := r.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	var got marks.Record
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.MarkID != r.MarkID {
		t.Fatalf("MarkID: got %q want %q", got.MarkID, r.MarkID)
	}
	if len(got.Candidates.CanonicalPacks) != 1 {
		t.Fatalf("CanonicalPacks len: got %d want 1", len(got.Candidates.CanonicalPacks))
	}
	if got.Candidates.CanonicalPacks[0].LastSeenReachableAt == nil {
		t.Fatal("LastSeenReachableAt round-tripped as nil")
	}
}

func TestRecord_PreviousMarkID_NullWhenEmpty(t *testing.T) {
	r := marks.Record{SchemaVersion: 1, MarkID: "mk_01HZ"}
	b, err := r.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	var top map[string]any
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if top["previous_mark_id"] != nil {
		t.Fatalf("previous_mark_id = %v, want JSON null", top["previous_mark_id"])
	}
}

func timePtr(t time.Time) *time.Time { return &t }
