package refstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/repoerrs"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Mode discriminates inline vs sharded staging. Encoded in Stage so
// downstream code (Repo.Commit buildBody) can branch on the layout
// without re-inspecting Body fields.
type Mode int

const (
	// ModeInline indicates a v1 layout — refs live directly in Body.Refs.
	ModeInline Mode = 1 + iota

	// ModeSharded indicates a v2 layout — refs live in
	// manifest/ref-shards/<hash>.json objects referenced by Body.RefShards.
	ModeSharded
)

// String renders Mode in a human-readable form for logs and errors.
func (m Mode) String() string {
	switch m {
	case ModeInline:
		return "inline"
	case ModeSharded:
		return "sharded"
	default:
		return fmt.Sprintf("mode(%d)", int(m))
	}
}

// RefStore is the read+stage interface every M12+ ref consumer uses.
// Implementations capture the body snapshot at construction time;
// callers re-construct against a fresh body after CAS retries.
type RefStore interface {
	// Mode reports which layout this store wraps. Stable for the
	// store's lifetime.
	Mode() Mode

	// Lookup returns the OID for refname, or (exists=false) when not
	// present. For inline stores this is O(1); for sharded stores
	// this loads exactly one shard (the one whose key hashes refname).
	Lookup(ctx context.Context, refname string) (oid string, exists bool, err error)

	// List returns every ref this store covers as a flat map. For
	// inline stores the returned map is a fresh copy of Body.Refs;
	// for sharded stores it is a fresh merge of every shard's
	// contents. Both implementations return a freshly-allocated map,
	// so mutating the returned map does NOT affect the store.
	List(ctx context.Context) (map[string]string, error)

	// Stage computes the layout-aware delta required to publish a
	// new ref state. updates uses the receive-pack convention: an
	// empty OID or the 40-zero oidconst.NullOIDHex means delete; any other
	// 40-hex value means upsert. The caller must validate refnames
	// separately (Stage does NOT enforce ref-name syntax).
	//
	// For inline stores, the returned Stage has Mode=ModeInline,
	// no NewShardObjects, and NewInlineRefs populated with the
	// final ref map (merged old + updates). The caller assigns
	// that map to Body.Refs in the new body.
	//
	// For sharded stores, the returned Stage has Mode=ModeSharded,
	// NewShardObjects covering every shard whose content changed
	// (PutIfAbsent these before the root CAS), and NewRefShards
	// populated with the final []RefShard slice for the new body.
	// NewInlineRefs is nil for sharded stores.
	Stage(ctx context.Context, updates map[string]string) (Stage, error)
}

// Stage is the pre-commit delta returned by RefStore.Stage. Lifetime
// is one Repo.Commit attempt; recompute against a fresh RefStore on
// CAS retry.
type Stage struct {
	Mode Mode

	// NewInlineRefs is the merged ref map (final state) when Mode ==
	// ModeInline. Nil when Mode == ModeSharded.
	NewInlineRefs map[string]string

	// NewShardObjects lists every shard whose content this push
	// generates. The caller PutIfAbsent's each one (content-
	// addressed, so concurrent identical writes are idempotent
	// via storage.ErrAlreadyExists swallowed by errors.Is) before
	// the root CAS. Empty when Mode == ModeInline.
	NewShardObjects []ShardWrite

	// NewRefShards is the final []manifest.RefShard slice for the
	// new body. Empty when Mode == ModeInline. Shards whose
	// content became empty after the update (e.g., a deletion that
	// removed the last ref in a bucket) are NOT included here.
	NewRefShards []manifest.RefShard
}

// ShardWrite is one shard object to PutIfAbsent before the root CAS.
// Key is the full storage key (includes the content hash so concurrent
// writers with the same content collapse to a single object). Shard
// is the 2-hex shard ID this object covers; callers (the staged-write
// lookup helper added in Phase 5, plus any future tooling that needs
// to route an in-memory refname against a staged-but-not-yet-committed
// shard) use it to find the right ShardWrite without re-parsing the
// contents twice or back-deriving the shard ID from the storage key.
type ShardWrite struct {
	Shard    string // "00".."ff"
	Key      string
	Hash     string // "sha256-<64hex>"; matches manifest.RefShard.Hash
	Contents []byte
}

// Sentinel errors returned by RefStore implementations. All are
// wrapped via fmt.Errorf("%w: ...", X) with enough context for
// the caller to log; use errors.Is to detect class.
var (
	// ErrShardCorrupt indicates a shard object's bytes hashed to a
	// value different from the body's recorded RefShard.Hash.
	// Treated as a tampering canary by callers — never retry.
	ErrShardCorrupt = errors.New("refstore: shard content hash mismatch")

	// ErrStaleRef indicates a Stage call where one of updates'
	// old-OID prechecks (when wired in via Phase 5) found a value
	// different from the on-store ref. Caller surfaces the per-ref
	// conflict on the wire.
	ErrStaleRef = errors.New("refstore: ref old-OID precheck failed")

	// ErrLookupNotInStage signals that Stage.Lookup could not answer
	// the refname from in-memory data alone — the refname's shard is
	// not among NewShardObjects, so its post-stage value equals the
	// pre-stage value. The caller should consult the original
	// RefStore.Lookup (which is cheap — one shard read) for an
	// authoritative answer. Inline-mode Stages never return this
	// sentinel because NewInlineRefs covers the full ref space.
	ErrLookupNotInStage = errors.New("refstore: refname not covered by stage")
)

// Lookup resolves refname against the post-stage state in memory.
//
// Inline mode: O(1) map lookup against NewInlineRefs. exists=false
// means the refname is not in the new state. err is always nil and
// the sentinel ErrLookupNotInStage is never returned.
//
// Sharded mode: scans NewShardObjects for the refname's shard ID.
//
//   - If a matching ShardWrite is found, parses its Contents JSON and
//     returns the lookup result from that map. (exists=false means the
//     ref was deleted by this stage or never existed in that shard.)
//
//   - If no matching ShardWrite exists, the refname's shard was not
//     modified by this stage. Returns ("", false, ErrLookupNotInStage)
//     so the caller knows to fall back to the original RefStore.Lookup.
//     The pre-stage value (whatever it was) is still the post-stage
//     value.
//
// The shard JSON is parsed inline on every call; sharded BuildAndCommit
// callers issue at most one Lookup per attempt (default-branch deletion
// guard), so caching would not pay for itself.
func (s *Stage) Lookup(refname string) (oid string, exists bool, err error) {
	if s.Mode == ModeInline {
		oid, exists = s.NewInlineRefs[refname]
		return oid, exists, nil
	}
	sid := ShardKey(refname)
	for i := range s.NewShardObjects {
		if s.NewShardObjects[i].Shard != sid {
			continue
		}
		refs, perr := s.parseShardWrite(&s.NewShardObjects[i])
		if perr != nil {
			return "", false, perr
		}
		oid, exists = refs[refname]
		return oid, exists, nil
	}
	return "", false, ErrLookupNotInStage
}

// parseShardWrite json-unmarshals a staged shard's Contents into a
// refname→OID map. Called from Stage.Lookup; surfaced as a method
// rather than a free function so future work (caching, alternative
// shard formats) can extend it without changing call sites.
func (s *Stage) parseShardWrite(w *ShardWrite) (map[string]string, error) {
	var m map[string]string
	if err := json.Unmarshal(w.Contents, &m); err != nil {
		return nil, fmt.Errorf("refstore: parse staged shard %s: %w", w.Key, err)
	}
	return m, nil
}

// ShardKey returns the 2-hex shard identifier for refname.
//
// Hashing: sha256 of the UTF-8 bytes of refname; the first byte is
// rendered as 2 lowercase hex characters ("00".."ff"). Stable across
// builds; do NOT change without bumping the ref_sharding strategy
// string (and writing the migration).
//
// Public surface for tests, the conformance suite, the reshard CLI,
// and any future observability tool that wants to know which shard a
// refname lands in.
func ShardKey(refname string) string {
	sum := sha256.Sum256([]byte(refname))
	return hex.EncodeToString(sum[:1])
}

// New is the dispatch factory. It inspects body and returns an
// InlineRefStore when Body.RefShards is empty, otherwise a
// ShardedRefStore. The store reference and tenant/repo keys are
// only consulted by the sharded path; passing zero values is fine
// for inline-only callers.
//
// New does NOT re-validate body's structural invariants — callers
// should route bytes through manifest.UnmarshalBody first, which
// catches hybrid state, unknown sharding strategies, and malformed
// shard fields. New does enforce one final defensive check: a body
// with RefSharding != "hash_v1" returns an error wrapping
// repoerrs.ErrInvalidManifest. Callers can errors.Is for the
// sentinel.
func New(ctx context.Context, s storage.ObjectStore, k *keys.Repo, body *manifest.Body) (RefStore, error) {
	if body == nil {
		return nil, fmt.Errorf("refstore.New: nil body")
	}
	if len(body.RefShards) == 0 {
		return newInlineRefStore(body), nil
	}
	if !manifest.IsSupportedRefSharding(body.RefSharding) {
		return nil, fmt.Errorf("%w: refstore.New: ref_sharding=%q is not supported by this build", repoerrs.ErrInvalidManifest, body.RefSharding)
	}
	if s == nil {
		return nil, fmt.Errorf("refstore.New: sharded body requires non-nil ObjectStore")
	}
	if k == nil {
		return nil, fmt.Errorf("refstore.New: sharded body requires non-nil keys.Repo")
	}
	return newShardedRefStore(s, k, body), nil
}
