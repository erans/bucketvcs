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

// marshaledRecord is the on-disk JSON shape — fields appear in this
// order on disk. previous_mark_id uses *string so that a nil pointer
// emits JSON null and a non-nil pointer emits a quoted string.
type marshaledRecord struct {
	SchemaVersion                int        `json:"schema_version"`
	MarkID                       string     `json:"mark_id"`
	PreviousMarkID               *string    `json:"previous_mark_id"`
	StartedAt                    time.Time  `json:"started_at"`
	CompletedAt                  time.Time  `json:"completed_at"`
	CurrentManifestVersion       uint64     `json:"current_manifest_version"`
	CurrentManifestObjectVersion string     `json:"current_manifest_object_version"`
	RetentionSeconds             int        `json:"retention_seconds"`
	TxOrphanSweepArmed           bool       `json:"tx_orphan_sweep_armed"`
	Candidates                   Candidates `json:"candidates"`
}

// MarshalJSON emits canonical Record JSON, normalizing nil candidate
// slices to empty arrays and PreviousMarkID to JSON null when empty.
// Field order on disk matches the marshaledRecord declaration.
func (r Record) MarshalJSON() ([]byte, error) {
	m := marshaledRecord{
		SchemaVersion:                r.SchemaVersion,
		MarkID:                       r.MarkID,
		StartedAt:                    r.StartedAt,
		CompletedAt:                  r.CompletedAt,
		CurrentManifestVersion:       r.CurrentManifestVersion,
		CurrentManifestObjectVersion: r.CurrentManifestObjectVersion,
		RetentionSeconds:             r.RetentionSeconds,
		TxOrphanSweepArmed:           r.TxOrphanSweepArmed,
		Candidates:                   r.Candidates,
	}
	if r.PreviousMarkID != "" {
		s := r.PreviousMarkID
		m.PreviousMarkID = &s
	}
	if m.Candidates.TxRecords == nil {
		m.Candidates.TxRecords = []TxCandidate{}
	}
	if m.Candidates.CanonicalPacks == nil {
		m.Candidates.CanonicalPacks = []PackCandidate{}
	}
	if m.Candidates.Indexes == nil {
		m.Candidates.Indexes = []IndexCandidate{}
	}
	return json.MarshalIndent(m, "", "  ")
}

// UnmarshalJSON parses the canonical Record JSON.
func (r *Record) UnmarshalJSON(b []byte) error {
	var m marshaledRecord
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	*r = Record{
		SchemaVersion:                m.SchemaVersion,
		MarkID:                       m.MarkID,
		StartedAt:                    m.StartedAt,
		CompletedAt:                  m.CompletedAt,
		CurrentManifestVersion:       m.CurrentManifestVersion,
		CurrentManifestObjectVersion: m.CurrentManifestObjectVersion,
		RetentionSeconds:             m.RetentionSeconds,
		TxOrphanSweepArmed:           m.TxOrphanSweepArmed,
		Candidates:                   m.Candidates,
	}
	if m.PreviousMarkID != nil {
		r.PreviousMarkID = *m.PreviousMarkID
	}
	return nil
}
