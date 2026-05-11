package manifest

import (
	"encoding/json"
	"fmt"
)

// Body is the typed view of M2-owned root-manifest body fields. JSON
// tags must match the on-the-wire shape exactly; M3 reads buckets
// produced by M2 and the wire format is the contract.
type Body struct {
	DefaultBranch string            `json:"default_branch"`
	Refs          map[string]string `json:"refs"`
	Packs         []PackEntry       `json:"packs"`
	Indexes       Indexes           `json:"indexes"`
	Bundles       []BundleEntry     `json:"bundles"`
}

// BundleEntry is reserved for M11 — placeholder type so the field exists
// in the wire format from M1 onwards.
type BundleEntry struct{}

// PackEntry references one pack uploaded under packs/canonical/.
type PackEntry struct {
	PackID      string `json:"pack_id"`
	PackKey     string `json:"pack_key"`
	IdxKey      string `json:"idx_key"`
	SizeBytes   int64  `json:"size_bytes"`
	ObjectCount int    `json:"object_count"`
}

// Indexes carries pointers to reachability index objects. ObjectMap
// and CommitGraph form the base; Reachability lists deltas since the
// base. Legacy (pre-M10) manifests have Reachability == nil.
type Indexes struct {
	ObjectMap    *IndexRef        `json:"object_map,omitempty"`
	CommitGraph  *IndexRef        `json:"commit_graph,omitempty"`
	Reachability *ReachabilityRef `json:"reachability,omitempty"`
}

// IndexRef is a key + content-hash pair. SizeBytes is populated when
// the producer knows the on-disk size (receive-pack for .bvrd,
// maintenance for .bvom/.bvcg). Consumers MAY use it for O(1)
// threshold evaluation; omit on legacy values.
type IndexRef struct {
	Key       string `json:"key"`
	Hash      string `json:"hash"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
}

// ReachabilityRef lists the delta chain layered on top of the base
// (ObjectMap + CommitGraph). BaseManifest records the manifest version
// that produced the base — paper-trail field, never used as a key.
type ReachabilityRef struct {
	BaseManifest string     `json:"base_manifest"`
	Deltas       []IndexRef `json:"deltas"`
}


// MarshalBody emits canonical Body JSON. Pretty-printed (2-space indent)
// to keep on-disk diffs readable; the indent is part of the wire format.
//
// Nil Refs and Packs are normalized to empty (not null) so the JSON
// shape is stable across Body{} default and explicitly-empty values.
func MarshalBody(b Body) ([]byte, error) {
	if b.Refs == nil {
		b.Refs = map[string]string{}
	}
	if b.Packs == nil {
		b.Packs = []PackEntry{}
	}
	if b.Bundles == nil {
		b.Bundles = []BundleEntry{}
	}
	if b.Indexes.Reachability != nil && b.Indexes.Reachability.Deltas == nil {
		rcopy := *b.Indexes.Reachability
		rcopy.Deltas = []IndexRef{}
		b.Indexes.Reachability = &rcopy
	}
	out, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("manifest: marshal body: %w", err)
	}
	return out, nil
}
