// Package manifest defines the M1-owned root-manifest header struct and
// the §43.7 schema gate. Body fields (refs, packs, indexes, bundles,
// default_branch) are M2's concern and are passed through this package
// as opaque json.RawMessage.
package manifest

import "time"

// RootHeader is the M1-owned subset of the §7 root manifest. Every
// field in this struct is set or validated by M1 on every commit.
type RootHeader struct {
	SchemaVersion    int       `json:"schema_version"`
	MinReaderVersion string    `json:"min_reader_version"`
	RepoID           string    `json:"repo_id"`
	RepoFormat       Format    `json:"repo_format"`
	ManifestVersion  uint64    `json:"manifest_version"`
	LatestTx         string    `json:"latest_tx"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// Format describes the on-disk Git object format for this repository.
// M1 ships only "sha1"; "sha256" is reserved for a future milestone.
type Format struct {
	ObjectFormat  string   `json:"object_format"`
	Compatibility []string `json:"compatibility,omitempty"`
}

// HeaderKeys lists the JSON field names M1 owns at the top level of the
// root manifest. Used by the body-merge helper to ensure callers do not
// emit duplicate or conflicting header fields in their body bytes.
var HeaderKeys = []string{
	"schema_version", "min_reader_version", "repo_id", "repo_format",
	"manifest_version", "latest_tx", "created_at", "updated_at",
}
