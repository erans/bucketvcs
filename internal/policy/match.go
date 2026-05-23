package policy

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// MatchPath reports whether p matches pattern. Patterns extend stdlib
// path.Match with `**`:
//   - `**`    matches one or more path segments greedily. (Non-trailing
//             `**` matches zero or more — only trailing `**` requires at
//             least one segment, so `secrets/**` matches files IN secrets/
//             but not the bare directory entry.)
//   - `*`     matches anything within one segment (no `/`)
//   - `?`     matches one byte (no `/`)
//   - `[abc]` character class
//
// See spec §4 for examples. Returns an error only if pattern is malformed
// (use ValidatePathPattern to pre-check).
func MatchPath(pattern, p string) (bool, error) {
	if err := ValidatePathPattern(pattern); err != nil {
		return false, err
	}
	if p == "" {
		return false, errors.New("empty path")
	}
	patSegs := strings.Split(pattern, "/")
	pathSegs := strings.Split(p, "/")
	return matchSegments(patSegs, pathSegs)
}

// matchSegments matches a list of pattern segments against a list of path
// segments. `**` is the only multi-segment pattern element; everything
// else matches exactly one path segment via stdlib path.Match.
func matchSegments(patSegs, pathSegs []string) (bool, error) {
	for len(patSegs) > 0 {
		head := patSegs[0]
		if head == "**" {
			rest := patSegs[1:]
			if len(rest) == 0 {
				// Trailing `**`: when preceded by a literal segment
				// (e.g. `secrets/**`), require at least one remaining
				// path segment. A bare leading `**` (no preceding
				// segments consumed) matches anything including a
				// single segment.
				return len(pathSegs) > 0, nil
			}
			for i := 0; i <= len(pathSegs); i++ {
				ok, err := matchSegments(rest, pathSegs[i:])
				if err != nil {
					return false, err
				}
				if ok {
					return true, nil
				}
			}
			return false, nil
		}
		if len(pathSegs) == 0 {
			return false, nil
		}
		ok, err := path.Match(head, pathSegs[0])
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		patSegs = patSegs[1:]
		pathSegs = pathSegs[1:]
	}
	return len(pathSegs) == 0, nil
}

// ValidatePathPattern returns an error if pattern is malformed. Rejects:
//   - empty pattern
//   - leading `/` (rooted absolute paths)
//   - trailing `/` (paths don't end in /; rules should match files)
//   - consecutive `/` (e.g. `a//b`)
//   - any segment for which stdlib path.Match would return ErrBadPattern
//     (e.g. `a/[unclosed`)
func ValidatePathPattern(pattern string) error {
	if pattern == "" {
		return errors.New("empty pattern")
	}
	if strings.HasPrefix(pattern, "/") {
		return fmt.Errorf("leading / not allowed: %q", pattern)
	}
	if strings.HasSuffix(pattern, "/") {
		return fmt.Errorf("trailing / not allowed: %q", pattern)
	}
	if strings.Contains(pattern, "//") {
		return fmt.Errorf("consecutive // not allowed: %q", pattern)
	}
	for _, seg := range strings.Split(pattern, "/") {
		if seg == "" {
			return fmt.Errorf("empty segment in %q", pattern)
		}
		if seg == "**" {
			continue
		}
		if _, err := path.Match(seg, ""); err != nil {
			return fmt.Errorf("invalid segment %q in pattern %q: %w", seg, pattern, err)
		}
	}
	return nil
}
