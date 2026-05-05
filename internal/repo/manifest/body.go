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
}

// PackEntry references one pack uploaded under packs/canonical/.
type PackEntry struct {
	PackID      string `json:"pack_id"`
	PackKey     string `json:"pack_key"`
	IdxKey      string `json:"idx_key"`
	SizeBytes   int64  `json:"size_bytes"`
	ObjectCount int    `json:"object_count"`
}

// Indexes carries pointers to M2 reachability index objects.
type Indexes struct {
	ObjectMap   *IndexRef `json:"object_map,omitempty"`
	CommitGraph *IndexRef `json:"commit_graph,omitempty"`
}

// IndexRef is a key + content-hash pair.
type IndexRef struct {
	Key  string `json:"key"`
	Hash string `json:"hash"`
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
	out, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("manifest: marshal body: %w", err)
	}
	return out, nil
}
