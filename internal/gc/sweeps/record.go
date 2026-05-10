// Package sweeps defines the immutable sweep-record schema produced by
// the M8 GC sweep phase per spec §M8 §8.2.
package sweeps

import (
	"encoding/json"
	"time"
)

// SchemaVersion is the current sweep-record schema version.
const SchemaVersion = 1

// Record is the on-disk shape of one immutable GC sweep record.
type Record struct {
	SchemaVersion                int            `json:"schema_version"`
	SweepID                      string         `json:"sweep_id"`
	MarkID                       string         `json:"mark_id"`
	StartedAt                    time.Time      `json:"started_at"`
	CompletedAt                  time.Time      `json:"completed_at"`
	CurrentManifestVersion       uint64         `json:"current_manifest_version"`
	CurrentManifestObjectVersion string         `json:"current_manifest_object_version"`
	Deleted                      Deleted        `json:"deleted"`
	Skipped                      []SkippedEntry `json:"skipped"`
	Errors                       []ErrorEntry   `json:"errors"`
}

// Deleted lists keys removed in the sweep, grouped by category.
type Deleted struct {
	TxRecords      []string `json:"tx_records"`
	CanonicalPacks []string `json:"canonical_packs"`
	Indexes        []string `json:"indexes"`
}

// SkippedEntry records one candidate that was not deleted.
type SkippedEntry struct {
	Key      string `json:"key"`
	Category string `json:"category"`
	Reason   string `json:"reason"` // revived | retention_not_met | version_mismatch | not_found | tx_sweep_disarmed
}

// ErrorEntry records one candidate the sweep tried to delete and failed on.
type ErrorEntry struct {
	Key      string `json:"key"`
	Category string `json:"category"`
	Error    string `json:"error"`
}

// marshaledRecord is the on-disk JSON shape — fields appear in this
// declaration order on disk (matching spec §8.2).
type marshaledRecord struct {
	SchemaVersion                int            `json:"schema_version"`
	SweepID                      string         `json:"sweep_id"`
	MarkID                       string         `json:"mark_id"`
	StartedAt                    time.Time      `json:"started_at"`
	CompletedAt                  time.Time      `json:"completed_at"`
	CurrentManifestVersion       uint64         `json:"current_manifest_version"`
	CurrentManifestObjectVersion string         `json:"current_manifest_object_version"`
	Deleted                      Deleted        `json:"deleted"`
	Skipped                      []SkippedEntry `json:"skipped"`
	Errors                       []ErrorEntry   `json:"errors"`
}

// MarshalJSON emits canonical Record JSON, normalizing nil slices to
// empty arrays. Field order on disk matches the marshaledRecord
// declaration.
func (r Record) MarshalJSON() ([]byte, error) {
	m := marshaledRecord(r)
	if m.Deleted.TxRecords == nil {
		m.Deleted.TxRecords = []string{}
	}
	if m.Deleted.CanonicalPacks == nil {
		m.Deleted.CanonicalPacks = []string{}
	}
	if m.Deleted.Indexes == nil {
		m.Deleted.Indexes = []string{}
	}
	if m.Skipped == nil {
		m.Skipped = []SkippedEntry{}
	}
	if m.Errors == nil {
		m.Errors = []ErrorEntry{}
	}
	return json.MarshalIndent(m, "", "  ")
}
