// Package marks defines the immutable mark-record schema produced by
// the M8 GC mark phase per spec §M8 §7.1 / spec §43.6.
package marks

import (
	"encoding/json"
	"time"
)

// SchemaVersion is the current mark-record schema version.
const SchemaVersion = 1

// Record is the on-disk shape of one immutable GC mark record.
type Record struct {
	SchemaVersion                int        `json:"schema_version"`
	MarkID                       string     `json:"mark_id"`
	PreviousMarkID               string     `json:"-"` // emitted via custom marshal
	StartedAt                    time.Time  `json:"started_at"`
	CompletedAt                  time.Time  `json:"completed_at"`
	CurrentManifestVersion       uint64     `json:"current_manifest_version"`
	CurrentManifestObjectVersion string     `json:"current_manifest_object_version"`
	RetentionSeconds             int        `json:"retention_seconds"`
	TxOrphanSweepArmed           bool       `json:"tx_orphan_sweep_armed"`
	Candidates                   Candidates `json:"candidates"`
}

// Candidates groups per-category candidate lists.
type Candidates struct {
	TxRecords      []TxCandidate    `json:"tx_records"`
	CanonicalPacks []PackCandidate  `json:"canonical_packs"`
	Indexes        []IndexCandidate `json:"indexes"`
}

// TxCandidate is one orphan-tx-record candidate.
type TxCandidate struct {
	Key                    string    `json:"key"`
	FirstSeenUnreachableAt time.Time `json:"first_seen_unreachable_at"`
}

// PackCandidate is one canonical-pack candidate.
type PackCandidate struct {
	Key                    string     `json:"key"`
	FirstSeenUnreachableAt time.Time  `json:"first_seen_unreachable_at"`
	LastSeenReachableAt    *time.Time `json:"last_seen_reachable_at,omitempty"`
	MarkManifestVersion    uint64     `json:"mark_manifest_version"`
}

// IndexCandidate is one stale-index candidate.
type IndexCandidate struct {
	Key                    string    `json:"key"`
	FirstSeenUnreachableAt time.Time `json:"first_seen_unreachable_at"`
}

// MarshalJSON emits canonical Record JSON, normalizing nil candidate
// slices to empty arrays and PreviousMarkID to JSON null when empty.
func (r Record) MarshalJSON() ([]byte, error) {
	type alias Record
	a := alias(r)
	if a.Candidates.TxRecords == nil {
		a.Candidates.TxRecords = []TxCandidate{}
	}
	if a.Candidates.CanonicalPacks == nil {
		a.Candidates.CanonicalPacks = []PackCandidate{}
	}
	if a.Candidates.Indexes == nil {
		a.Candidates.Indexes = []IndexCandidate{}
	}
	prev := json.RawMessage("null")
	if r.PreviousMarkID != "" {
		b, err := json.Marshal(r.PreviousMarkID)
		if err != nil {
			return nil, err
		}
		prev = b
	}
	out, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	// Splice previous_mark_id into the output object.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(out, &top); err != nil {
		return nil, err
	}
	top["previous_mark_id"] = prev
	return json.MarshalIndent(top, "", "  ")
}

// UnmarshalJSON parses the canonical Record JSON.
func (r *Record) UnmarshalJSON(b []byte) error {
	type alias Record
	var a struct {
		alias
		PreviousMarkID *string `json:"previous_mark_id"`
	}
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*r = Record(a.alias)
	if a.PreviousMarkID != nil {
		r.PreviousMarkID = *a.PreviousMarkID
	}
	return nil
}
