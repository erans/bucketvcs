package maintenance

import (
	"fmt"

	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

// ResolveDefaultBranch returns the ref the bundle should cover.
//
//   - If override is non-empty, it MUST exist in m.Refs.
//   - Otherwise, m.DefaultBranch is used if non-empty AND present in m.Refs.
//   - Otherwise, refs/heads/main is tried, then refs/heads/master.
//   - Otherwise an error is returned (caller should skip bundle-refresh).
//
// The returned ref is always of the form refs/heads/<name>.
func ResolveDefaultBranch(m manifest.Body, override string) (string, error) {
	if override != "" {
		if _, ok := m.Refs[override]; !ok {
			return "", fmt.Errorf("maintenance: bundle default-branch override %q not in manifest refs", override)
		}
		return override, nil
	}
	if m.DefaultBranch != "" {
		if _, ok := m.Refs[m.DefaultBranch]; ok {
			return m.DefaultBranch, nil
		}
	}
	for _, fallback := range []string{"refs/heads/main", "refs/heads/master"} {
		if _, ok := m.Refs[fallback]; ok {
			return fallback, nil
		}
	}
	return "", fmt.Errorf("maintenance: cannot resolve default branch: HEAD unset and neither refs/heads/main nor refs/heads/master present")
}
