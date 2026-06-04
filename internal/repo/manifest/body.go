package manifest

import (
	"encoding/json"
	"fmt"
)

// Body is the typed view of M2-owned root-manifest body fields. JSON
// tags must match the on-the-wire shape exactly; M3 reads buckets
// produced by M2 and the wire format is the contract.
type Body struct {
	DefaultBranch string `json:"default_branch"`
	// Refs is the v1 inline ref map (refname → 40-hex OID). Mutually
	// exclusive with RefShards. As of M12 (schema_version=2), an empty
	// map serializes to NO `refs` field on the wire — the omitempty
	// JSON tag is required so v2 producers can elide Refs entirely.
	// Pre-M12 producers always emitted `"refs":{}` for an empty repo;
	// the delta is benign because Go's json.Unmarshal handles both
	// (absent → nil map; `{}` → empty map; both yield zero-iteration
	// `range` and the zero value for any lookup).
	Refs        map[string]string `json:"refs,omitempty"`         // v1; mutually exclusive with RefShards
	RefShards   []RefShard        `json:"ref_shards,omitempty"`   // v2; mutually exclusive with Refs
	RefSharding string            `json:"ref_sharding,omitempty"` // v2; "hash_v1" today
	Packs       []PackEntry       `json:"packs"`
	Indexes     Indexes           `json:"indexes"`
	Bundles     []BundleEntry     `json:"bundles"`
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

// RefShard references one immutable ref-shard object under
// manifest/ref-shards/<hash>.json. Present in v2 manifests only;
// absent (nil slice) in v1.
//
// Content-addressing: Key includes Hash, so a PutIfAbsent on Key is
// idempotent — two writers minting the same shard contents collapse
// to a single object.
//
// Order: Body.RefShards is sorted strictly ascending by Shard ID.
// This is enforced by manifest.UnmarshalBody and MarshalBody (via
// validateBody) so the root manifest's bytes are deterministic for
// a given shard set. Producers MUST sort before marshaling.
type RefShard struct {
	// Shard is the 2-hex shard identifier ("00".."ff"), the first byte
	// of sha256(refname) for every ref this shard contains.
	Shard string `json:"shard"`

	// Key is the full object-store key for this shard
	// (tenants/<t>/repos/<r>/manifest/ref-shards/<hash>.json).
	// Already includes the content hash so it round-trips through GC.
	Key string `json:"key"`

	// Hash is the sha256 of the shard's canonical JSON, formatted
	// "sha256-<64-lowercase-hex>". Verified at read time.
	Hash string `json:"hash"`

	// RefCount is informational (paper-trail; not load-bearing). Used
	// by operators to gauge shard distribution after a reshard.
	RefCount int `json:"ref_count"`
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

	// BitmapKey is the object-store key of the .bitmap sidecar for
	// this pack (under packs/canonical/<id>.bitmap). Set when M9.5+
	// maintenance produced a bitmap; empty for packs written by
	// receive-pack (which never emits bitmaps) or pre-M9.5 maintenance
	// runs. A repo can have a mix of bitmapped and non-bitmapped
	// canonical packs; the §15.3 coverage trigger drives them toward
	// uniformity over time.
	BitmapKey string `json:"bitmap_key,omitempty"`
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
// Packs, Bundles, and Indexes.Reachability.Deltas are normalized from
// nil to empty (not null) so the JSON shape is stable across Body{}
// default and explicitly-empty values. (Refs has the `omitempty` JSON
// tag, so nil and empty maps both elide the field and need no
// normalization here. Read-side nil-map normalization for downstream
// mutation lives in UnmarshalBody — MarshalBody receives Body by value,
// so any normalization here would not reach the caller.)
func MarshalBody(b Body) ([]byte, error) {
	// Validate producer-side: a Body that violates the v1/v2
	// invariants would round-trip unreadable through UnmarshalBody.
	// Catching at marshal time prevents the "writer emits an
	// unreadable manifest" footgun.
	if err := validateBody(&b); err != nil {
		return nil, err
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
