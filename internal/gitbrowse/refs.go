package gitbrowse

import (
	"context"
	"sort"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/browsemodel"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/refstore"
)

// loadRefs resolves the full ref map and default-branch name via the manifest
// path (no mirror). Returned map is refname->40-hex OID (e.g. "refs/heads/main").
func (s *Service) loadRefs(ctx context.Context, tenant, repoID string) (refMap map[string]string, defaultBranch string, err error) {
	r, err := repo.Open(ctx, s.store, tenant, repoID)
	if err != nil {
		return nil, "", err
	}
	view, err := r.ReadRoot(ctx)
	if err != nil {
		return nil, "", err
	}
	body, err := manifest.UnmarshalBody(view.Body)
	if err != nil {
		return nil, "", err
	}
	k, err := keys.NewRepo(tenant, repoID)
	if err != nil {
		return nil, "", err
	}
	rs, err := refstore.New(ctx, s.store, k, &body)
	if err != nil {
		return nil, "", err
	}
	m, err := rs.List(ctx)
	if err != nil {
		return nil, "", err
	}
	return m, body.DefaultBranch, nil
}

// ListRefs returns branches, tags, and the default-branch short name.
func (s *Service) ListRefs(ctx context.Context, tenant, repoID string) (browsemodel.Refs, error) {
	m, def, err := s.loadRefs(ctx, tenant, repoID)
	if err != nil {
		return browsemodel.Refs{}, err
	}
	var out browsemodel.Refs
	for name, oid := range m {
		switch {
		case strings.HasPrefix(name, "refs/heads/"):
			out.Branches = append(out.Branches, browsemodel.RefInfo{
				Name: strings.TrimPrefix(name, "refs/heads/"), OID: oid,
			})
		case strings.HasPrefix(name, "refs/tags/"):
			out.Tags = append(out.Tags, browsemodel.RefInfo{
				Name: strings.TrimPrefix(name, "refs/tags/"), OID: oid,
			})
		}
	}
	sort.Slice(out.Branches, func(i, j int) bool { return out.Branches[i].Name < out.Branches[j].Name })
	sort.Slice(out.Tags, func(i, j int) bool { return out.Tags[i].Name < out.Tags[j].Name })
	out.Default = defaultBranchName(def, out.Branches)
	return out, nil
}

// defaultBranchName picks the display default branch: the manifest's
// DefaultBranch (stripped) if it is a real branch; else main; else master; else
// the first sorted branch; else "" (empty repo).
func defaultBranchName(manifestDefault string, branches []browsemodel.RefInfo) string {
	has := func(n string) bool {
		for _, b := range branches {
			if b.Name == n {
				return true
			}
		}
		return false
	}
	d := strings.TrimPrefix(manifestDefault, "refs/heads/")
	if d != "" && has(d) {
		return d
	}
	if has("main") {
		return "main"
	}
	if has("master") {
		return "master"
	}
	if len(branches) > 0 {
		return branches[0].Name
	}
	return ""
}
