package gc

import (
	"encoding/json"
	"fmt"

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
// (ref-shards for M12) are tolerated. Bundles are fully supported as of M11.
//
// On a JSON parse failure the header-derived keys are returned together
// with a non-nil error. The caller must treat this error as fatal and
// abort GC for the affected repo — continuing without body-derived keys
// risks sweeping live packs.
func BuildLiveSet(k *keys.Repo, header manifest.RootHeader, bodyJSON []byte) (LiveSet, error) {
	live := LiveSet{}
	live[k.RootManifestKey()] = struct{}{}
	if header.LatestTx != "" {
		live[k.TxRecordKey(header.LatestTx)] = struct{}{}
		live[k.CommitMarkerKey(header.LatestTx)] = struct{}{}
	}

	var body manifest.Body
	if len(bodyJSON) > 0 {
		if err := json.Unmarshal(bodyJSON, &body); err != nil {
			// Return the header-derived keys plus the error. The caller
			// must abort GC — continuing without body-derived pack/index
			// keys risks sweeping live data.
			return live, fmt.Errorf("gc: parse manifest body: %w", err)
		}
	}

	for _, p := range body.Packs {
		if p.PackKey != "" {
			live[p.PackKey] = struct{}{}
		}
		if p.IdxKey != "" {
			live[p.IdxKey] = struct{}{}
		}
		if p.BitmapKey != "" {
			live[p.BitmapKey] = struct{}{}
		}
	}
	if body.Indexes.ObjectMap != nil && body.Indexes.ObjectMap.Key != "" {
		live[body.Indexes.ObjectMap.Key] = struct{}{}
	}
	if body.Indexes.CommitGraph != nil && body.Indexes.CommitGraph.Key != "" {
		live[body.Indexes.CommitGraph.Key] = struct{}{}
	}
	if body.Indexes.Reachability != nil {
		for _, ref := range body.Indexes.Reachability.Deltas {
			if ref.Key != "" {
				live[ref.Key] = struct{}{}
			}
		}
	}
	for _, b := range body.Bundles {
		if b.BundleKey != "" {
			live[b.BundleKey] = struct{}{}
		}
		if b.SidecarKey != "" {
			live[b.SidecarKey] = struct{}{}
		}
	}
	return live, nil
}
