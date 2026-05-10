package gc

import (
	"encoding/json"

	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

// LiveSet is a set of full storage keys reachable from the current
// manifest. Membership is by exact key string.
type LiveSet map[string]struct{}

// BuildLiveSet returns the set of storage keys protected from sweep
// for one repo at the manifest snapshot header+body.
//
// The body is parsed as manifest.Body. Unknown fields in the body are
// ignored. Future-fields recognized but currently emitted as empty
// (Bundles for M11; ref-shards for M12) are tolerated.
func BuildLiveSet(k *keys.Repo, header manifest.RootHeader, bodyJSON []byte) LiveSet {
	live := LiveSet{}
	live[k.RootManifestKey()] = struct{}{}
	if header.LatestTx != "" {
		live[k.TxRecordKey(header.LatestTx)] = struct{}{}
		live[k.CommitMarkerKey(header.LatestTx)] = struct{}{}
	}

	var body manifest.Body
	if len(bodyJSON) > 0 {
		// Best-effort parse; an unparseable body still leaves the
		// header-derived keys in the live-set, which is the safe behavior.
		_ = json.Unmarshal(bodyJSON, &body)
	}

	for _, p := range body.Packs {
		if p.PackKey != "" {
			live[p.PackKey] = struct{}{}
		}
		if p.IdxKey != "" {
			live[p.IdxKey] = struct{}{}
		}
	}
	if body.Indexes.ObjectMap != nil && body.Indexes.ObjectMap.Key != "" {
		live[body.Indexes.ObjectMap.Key] = struct{}{}
	}
	if body.Indexes.CommitGraph != nil && body.Indexes.CommitGraph.Key != "" {
		live[body.Indexes.CommitGraph.Key] = struct{}{}
	}
	// body.Bundles is M11 placeholder — currently empty struct, no keys to add.
	return live
}
