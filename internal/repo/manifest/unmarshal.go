package manifest

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/repo/repoerrs"
)

// supportedRefShardingStrategies lists the ref_sharding strategy strings
// this build recognizes. M12 ships only "hash_v1". Future strategies
// (e.g., "namespace_hash_v1") extend this list, gated by the
// ref_sharding string at read time so old binaries fail-closed.
//
// Unexported on purpose: an exported mutable map would let any
// importer delete an entry and silently weaken the validation gate.
// Callers query via IsSupportedRefSharding.
var supportedRefShardingStrategies = map[string]struct{}{
	"hash_v1": {},
}

// IsSupportedRefSharding reports whether s is a ref_sharding strategy
// string this build accepts. Read-only view of supportedRefShardingStrategies;
// the underlying map is unexported so external code cannot mutate it.
func IsSupportedRefSharding(s string) bool {
	_, ok := supportedRefShardingStrategies[s]
	return ok
}

// UnmarshalBody parses a root-manifest body, then enforces M12's
// structural invariants:
//
//   - Refs and RefShards are mutually exclusive (no hybrid v1/v2 state).
//   - A v2 body (RefShards non-empty) must have RefSharding set to a
//     supported strategy string.
//   - A v1 body (Refs populated or both empty) must have RefSharding == "".
//   - Each RefShard.Shard is 2 lowercase hex ("00".."ff"); shard IDs
//     are unique within the slice.
//   - Each RefShard.Hash matches "sha256-" + 64 lowercase hex.
//
// Returns repoerrs.ErrInvalidManifest (wrapped with detail) for any
// violation, INCLUDING raw JSON parse failures — a body that fails to
// parse is structurally an invalid manifest body. Callers can use
// errors.Is(err, repoerrs.ErrInvalidManifest) as the canonical
// boundary check; the underlying json.UnmarshalTypeError is still
// unwrappable via errors.As for diagnostic purposes.
//
// UnmarshalBody is the canonical body-parse entry point. Consumers
// SHOULD use it in preference to json.Unmarshal(view.Body, &body) so
// invariant violations are caught at the read boundary.
//
// Status: M12.1+. UnmarshalBody is the canonical body-parse entry
// point for all production paths: exporter, uploadpack (top-level
// dispatcher + advertise + bundle-uri Lookup), receivepack (advertise
// + complete precheck), importer (BuildAndCommit + Import callback),
// reachability.Load, gc.BuildLiveSet, maintenance pipeline + reshard
// + bundle-casmerge + backfill, cmd/bucketvcs negotiate + inspect.
// Conformance test helpers in internal/gc/conformance, internal/
// reachability/conformance, and internal/diffharness still call
// json.Unmarshal directly for fixture purposes.
func UnmarshalBody(raw []byte) (Body, error) {
	var b Body
	if err := json.Unmarshal(raw, &b); err != nil {
		return Body{}, fmt.Errorf("%w: unmarshal body: %w", repoerrs.ErrInvalidManifest, err)
	}
	if err := validateBody(&b); err != nil {
		return Body{}, err
	}
	// Normalize: for v1 bodies the wire form may omit the "refs" key
	// entirely (M12+ producers do so for empty repos because of
	// omitempty). Downstream callers expecting body.Refs to be
	// assignable (`body.Refs[name] = oid`) would panic on a nil map,
	// so initialize to an empty map for v1 bodies. v2 bodies keep
	// Refs nil — the mutual-exclusion invariant requires it.
	if len(b.RefShards) == 0 && b.Refs == nil {
		b.Refs = map[string]string{}
	}
	return b, nil
}

func validateBody(b *Body) error {
	hasRefs := len(b.Refs) > 0
	hasShards := len(b.RefShards) > 0
	hasShardingTag := b.RefSharding != ""

	// Hybrid state.
	if hasRefs && hasShards {
		return fmt.Errorf("%w: hybrid v1/v2 ref state (both refs and ref_shards populated)", repoerrs.ErrInvalidManifest)
	}

	if hasShards {
		// v2 path.
		if !hasShardingTag {
			return fmt.Errorf("%w: v2 body has ref_shards but ref_sharding is empty", repoerrs.ErrInvalidManifest)
		}
		if !IsSupportedRefSharding(b.RefSharding) {
			return fmt.Errorf("%w: unsupported ref_sharding strategy %q", repoerrs.ErrInvalidManifest, b.RefSharding)
		}
		if err := validateRefShards(b.RefShards); err != nil {
			return err
		}
	} else {
		// v1 path (or empty repo). RefSharding without RefShards is malformed.
		if hasShardingTag {
			return fmt.Errorf("%w: ref_sharding=%q set without ref_shards", repoerrs.ErrInvalidManifest, b.RefSharding)
		}
	}
	return nil
}

func validateRefShards(shards []RefShard) error {
	// Strict-ascending order on Shard ID gives us "no duplicates" for
	// free (duplicates would violate s.Shard > prev). Tracking a separate
	// seen-map would be redundant. Hash collisions across distinct shards
	// are independently checked: two shards with the same content hash
	// would mean the same Key (content-addressed), which can't legitimately
	// happen for distinct shard IDs.
	var prev string
	seenHash := make(map[string]int, len(shards))
	for i, s := range shards {
		if !isShardID(s.Shard) {
			return fmt.Errorf("%w: ref_shards[%d].shard = %q (want 2 lowercase hex)", repoerrs.ErrInvalidManifest, i, s.Shard)
		}
		if !isShardHash(s.Hash) {
			return fmt.Errorf("%w: ref_shards[%d].hash = %q (want sha256-<64hex>)", repoerrs.ErrInvalidManifest, i, s.Hash)
		}
		if s.Key == "" {
			return fmt.Errorf("%w: ref_shards[%d].key is empty", repoerrs.ErrInvalidManifest, i)
		}
		if s.RefCount < 0 {
			return fmt.Errorf("%w: ref_shards[%d].ref_count = %d (must be >= 0)", repoerrs.ErrInvalidManifest, i, s.RefCount)
		}
		if i > 0 && s.Shard <= prev {
			if s.Shard == prev {
				return fmt.Errorf("%w: ref_shards[%d].shard = %q duplicates the previous entry", repoerrs.ErrInvalidManifest, i, s.Shard)
			}
			return fmt.Errorf("%w: ref_shards[%d].shard = %q is not strictly ascending (previous = %q)", repoerrs.ErrInvalidManifest, i, s.Shard, prev)
		}
		if prevIdx, dup := seenHash[s.Hash]; dup {
			return fmt.Errorf("%w: ref_shards[%d].hash = %q duplicates ref_shards[%d] (distinct shard IDs cannot share content)", repoerrs.ErrInvalidManifest, i, s.Hash, prevIdx)
		}
		seenHash[s.Hash] = i
		prev = s.Shard
	}
	return nil
}

// isShardID reports whether s is exactly two lowercase hex characters.
func isShardID(s string) bool {
	if len(s) != 2 {
		return false
	}
	if _, err := hex.DecodeString(s); err != nil {
		return false
	}
	// Disallow uppercase. hex.DecodeString accepts both cases; we want
	// lowercase only so shard IDs are canonical.
	if s != strings.ToLower(s) {
		return false
	}
	return true
}

// isShardHash reports whether h is "sha256-" + 64 lowercase hex.
func isShardHash(h string) bool {
	const prefix = "sha256-"
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	rest := h[len(prefix):]
	if len(rest) != 64 {
		return false
	}
	if rest != strings.ToLower(rest) {
		return false
	}
	if _, err := hex.DecodeString(rest); err != nil {
		return false
	}
	return true
}
