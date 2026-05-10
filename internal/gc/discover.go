package gc

import (
	"context"
	"fmt"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// DiscoverCanonicalPacks lists all objects under packs/canonical/ for the
// repo and returns the keys NOT in live.
func DiscoverCanonicalPacks(ctx context.Context, s storage.ObjectStore, k *keys.Repo, live LiveSet) ([]string, error) {
	prefix := k.Prefix() + "packs/canonical/"
	return listExcludingLive(ctx, s, prefix, live)
}

// DiscoverIndexes lists all objects under indexes/{object-map,commit-graph,reachability}/
// and returns keys NOT in live.
func DiscoverIndexes(ctx context.Context, s storage.ObjectStore, k *keys.Repo, live LiveSet) ([]string, error) {
	var out []string
	for _, sub := range []string{"object-map/", "commit-graph/", "reachability/"} {
		got, err := listExcludingLive(ctx, s, k.Prefix()+"indexes/"+sub, live)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	return out, nil
}

// DiscoverTxRecords lists all tx records for the repo and returns:
//   - the keys of tx records that are orphan candidates (no .commit
//     marker, not the current latest_tx),
//   - the per-repo tx_orphan_sweep_armed flag (true if at least one
//     .commit marker exists in the listing).
//
// Caller is responsible for OR-ing armed with any prior mark record's
// armed value (sticky once true).
func DiscoverTxRecords(ctx context.Context, s storage.ObjectStore, k *keys.Repo, live LiveSet) (candidates []string, armed bool, err error) {
	prefix := k.Prefix() + "tx/"
	records := map[string]struct{}{}
	markers := map[string]struct{}{}

	var token string
	for {
		page, err := s.List(ctx, prefix, &storage.ListOptions{ContinuationToken: token})
		if err != nil {
			return nil, false, fmt.Errorf("gc: list tx: %w", err)
		}
		for _, obj := range page.Objects {
			rest := strings.TrimPrefix(obj.Key, prefix)
			switch {
			case strings.HasSuffix(rest, ".json.commit"):
				markers[obj.Key] = struct{}{}
			case strings.HasSuffix(rest, ".json"):
				records[obj.Key] = struct{}{}
			}
		}
		if page.NextToken == "" {
			break
		}
		token = page.NextToken
	}

	armed = len(markers) > 0

	for recKey := range records {
		if _, ok := live[recKey]; ok {
			continue // current latest_tx
		}
		// Marker key is the tx record key + ".commit".
		if _, hasMarker := markers[recKey+".commit"]; hasMarker {
			continue // winner
		}
		candidates = append(candidates, recKey)
	}
	return candidates, armed, nil
}

// listExcludingLive enumerates all objects under prefix and returns those
// not present in live.
func listExcludingLive(ctx context.Context, s storage.ObjectStore, prefix string, live LiveSet) ([]string, error) {
	var out []string
	var token string
	for {
		page, err := s.List(ctx, prefix, &storage.ListOptions{ContinuationToken: token})
		if err != nil {
			return nil, fmt.Errorf("gc: list %s: %w", prefix, err)
		}
		for _, obj := range page.Objects {
			if _, ok := live[obj.Key]; ok {
				continue
			}
			out = append(out, obj.Key)
		}
		if page.NextToken == "" {
			break
		}
		token = page.NextToken
	}
	return out, nil
}
