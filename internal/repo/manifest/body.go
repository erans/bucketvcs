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

// BundleEntry references one bundle file (default-branch full bundle in
// M11; rolling-base / release-tag entries land in successor milestones).
// Stored under body.Bundles; freshness is computed at advertise time and
// is NOT persisted here.
type BundleEntry struct {
	// ID is "bundle_<repo>_<version>_<sha256[:8]>". Unique within the manifest.
	ID string `json:"id"`

	// Kind discriminates bundle variants. M11 only writes "full_default".
	// Future kinds: "full_tag", "rolling_base", "rolling_increment".
	Kind string `json:"kind"`

	// BundleKey is the storage key under tenants/.../bundles/<sha256>.bundle.
	BundleKey string `json:"bundle_key"`

	// SidecarKey is the storage key for the JSON sidecar (mirror of these
	// fields plus a SHA-256 trailer of the bundle file). Present so an
	// out-of-band tool can reconstruct BundleEntry if the manifest is lost.
	SidecarKey string `json:"sidecar_key"`

	// BundleHash is the SHA-256 of the bundle file body, hex-encoded
	// ("sha256-<64-hex>" form matches IndexRef.Hash convention).
	BundleHash string `json:"bundle_hash"`

	// Ref is the bundle's covered ref (M11: always refs/heads/<default>).
	Ref string `json:"ref"`

	// TipOID is the 40-hex SHA-1 the bundle's tip resolves to.
	TipOID string `json:"tip_oid"`

	// CoversManifestVersion records the header's ManifestVersion at
	// generation time. Used by the freshness state machine; never
	// used as a key.
	CoversManifestVersion uint64 `json:"covers_manifest_version"`

	// ByteSize is the on-disk bundle size. Reported in audit + metrics.
	ByteSize int64 `json:"byte_size"`

	// GeneratedAt is RFC3339 UTC. Bundle freshness uses this for the
	// age-threshold check.
	GeneratedAt string `json:"generated_at"`
}

// PackEntry references one pack uploaded under packs/canonical/.
type PackEntry struct {
	PackID      string `json:"pack_id"`
	PackKey     string `json:"pack_key"`
	IdxKey      string `json:"idx_key"`
	SizeBytes   int64  `json:"size_bytes"`
	ObjectCount int    `json:"object_count"`

	// PackChecksum is the 40-hex SHA-1 of the pack's trailer (Git's
	// pack-checksum, distinct from the SHA-256 storage hash). Required
	// for §16.4 packfile-uri advertisement so the gateway can populate
	// the `packfile-uri=<sha1>` packet stanza without re-reading the
	// pack trailer at advertise time. Empty for legacy (pre-M11) packs;
	// M11 maintenance backfills lazily.
	PackChecksum string `json:"pack_checksum,omitempty"`
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
