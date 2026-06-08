package buildtrigger

import "github.com/bucketvcs/bucketvcs/internal/policy"

// RefMatches reports whether ref should fire a trigger with the given include
// and exclude glob lists. Rule: fire if (include empty OR any include matches)
// AND no exclude matches. Exclude wins. Globs use the M16 `**`-aware matcher
// (policy.MatchPath) against the full refname (e.g. "refs/heads/main").
func RefMatches(include, exclude []string, ref string) (bool, error) {
	for _, pat := range exclude {
		ok, err := policy.MatchPath(pat, ref)
		if err != nil {
			return false, err
		}
		if ok {
			return false, nil
		}
	}
	if len(include) == 0 {
		return true, nil
	}
	for _, pat := range include {
		ok, err := policy.MatchPath(pat, ref)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}
