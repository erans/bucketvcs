package maintenance

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// RepoRef identifies one repo under a store by its (tenant_id, repo_id).
type RepoRef struct {
	TenantID string
	RepoID   string
}

// DiscoverRepos enumerates all repos at "tenants/<t>/repos/<r>/" under
// the store. Uses delimiter listing to avoid downloading object keys
// for the actual repo contents.
func DiscoverRepos(ctx context.Context, s storage.ObjectStore) ([]RepoRef, error) {
	tenantPrefix := "tenants/"
	tenants, err := listCommonPrefixes(ctx, s, tenantPrefix)
	if err != nil {
		return nil, fmt.Errorf("maintenance: list tenants: %w", err)
	}
	var out []RepoRef
	for _, tenantPath := range tenants {
		// tenantPath = "tenants/<t>/"
		t := strings.TrimSuffix(strings.TrimPrefix(tenantPath, tenantPrefix), "/")
		if t == "" {
			continue
		}
		repoPrefix := tenantPath + "repos/"
		repos, err := listCommonPrefixes(ctx, s, repoPrefix)
		if err != nil {
			return nil, fmt.Errorf("maintenance: list repos under %s: %w", tenantPath, err)
		}
		for _, repoPath := range repos {
			r := strings.TrimSuffix(strings.TrimPrefix(repoPath, repoPrefix), "/")
			if r == "" {
				continue
			}
			out = append(out, RepoRef{TenantID: t, RepoID: r})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TenantID != out[j].TenantID {
			return out[i].TenantID < out[j].TenantID
		}
		return out[i].RepoID < out[j].RepoID
	})
	return out, nil
}

func listCommonPrefixes(ctx context.Context, s storage.ObjectStore, prefix string) ([]string, error) {
	seen := map[string]struct{}{}
	var token string
	for {
		page, err := s.List(ctx, prefix, &storage.ListOptions{Delimiter: "/", ContinuationToken: token})
		if err != nil {
			return nil, err
		}
		for _, p := range page.CommonPrefixes {
			seen[p] = struct{}{}
		}
		if page.NextToken == "" {
			break
		}
		token = page.NextToken
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	return out, nil
}
