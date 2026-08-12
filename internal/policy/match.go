package policy

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// U-13 input caps: `**` segments and total segments are bounded so the
// memoized matcher's (patternIdx × pathIdx) state space stays small.
// Generous by design — real rules use ≤2 `**` and a handful of segments.
const maxPathPatternDblStars = 2
const maxPathPatternSegments = 64

// maxPathMatchStates bounds evaluation of legacy persisted rules that predate
// creation caps. New rules are far smaller; this ceiling prevents a historical
// large rule combined with a deep attacker-controlled path from exhausting
// memory or CPU during receive-pack.
const maxPathMatchStates = 64 * 1024

// maxCheckPathsMatchStates bounds the aggregate work across all rules and
// changed paths in one push. Without an aggregate ceiling, an attacker could
// repeat many individually sub-budget paths and multiply receive-pack work.
const maxCheckPathsMatchStates = 256 * 1024

type pathMatchBudget struct {
	used  int
	limit int
}

// MatchPath reports whether p matches pattern. Patterns extend stdlib
// path.Match with `**`:
//   - `**`    matches one or more path segments greedily. (Non-trailing
//     `**` matches zero or more — only trailing `**` requires at
//     least one segment, so `secrets/**` matches files IN secrets/
//     but not the bare directory entry.)
//   - `*`     matches anything within one segment (no `/`)
//   - `?`     matches one byte (no `/`)
//   - `[abc]` character class
//
// See spec §4 for examples. Returns an error only if pattern is malformed
// (use ValidatePathPattern to pre-check).
func MatchPath(pattern, p string) (bool, error) {
	return matchPathWithBudget(pattern, p, nil)
}

func matchPathWithBudget(pattern, p string, aggregate *pathMatchBudget) (bool, error) {
	// Evaluate syntactically valid legacy rows even when they exceed today's
	// creation caps. The memoized matcher makes those persisted rules safe;
	// ValidatePathPattern applies stricter limits only at write boundaries.
	if err := validatePathPatternSyntax(pattern); err != nil {
		return false, err
	}
	if p == "" {
		return false, errors.New("empty path")
	}
	patSegs := strings.Split(pattern, "/")
	pathSegs := strings.Split(p, "/")
	return matchSegments(patSegs, pathSegs, aggregate)
}

// matchSegments matches a list of pattern segments against a list of path
// segments. `**` is the only multi-segment pattern element; everything
// else matches exactly one path segment via stdlib path.Match.
//
// Wave-4 (U-13): the recursion is memoized over (patternIdx, pathIdx) —
// each state is computed once, so multi-`**` patterns cost O(states) instead
// of exponential recomputation on the receive-pack path. Semantics are
// bit-identical to the naive recursion (pinned by the reference-matcher
// battery in match_u13_test.go); ValidatePathPattern additionally caps
// `**` count and segment count, bounding the state space at the boundary.
func matchSegments(patSegs, pathSegs []string, aggregate *pathMatchBudget) (bool, error) {
	type result struct {
		ok  bool
		err error
	}
	// Allocate lazily: persisted pre-cap patterns may be large, and an eager
	// pattern×path capacity can itself be a denial of service even when only a
	// handful of states are reachable.
	memo := make(map[[2]int]result)
	states := 0
	var walk func(patIdx, pathIdx int) (bool, error)
	walk = func(patIdx, pathIdx int) (bool, error) {
		key := [2]int{patIdx, pathIdx}
		if v, ok := memo[key]; ok {
			return v.ok, v.err
		}
		states++
		if states > maxPathMatchStates {
			return false, fmt.Errorf("path pattern evaluation budget exceeded (%d states)", maxPathMatchStates)
		}
		if aggregate != nil {
			aggregate.used++
			if aggregate.used > aggregate.limit {
				return false, fmt.Errorf("cumulative path pattern evaluation budget exceeded (%d states)", aggregate.limit)
			}
		}
		var res result
		switch {
		case patIdx == len(patSegs):
			res.ok = pathIdx == len(pathSegs)
		case patSegs[patIdx] == "**":
			if patIdx == len(patSegs)-1 {
				// Trailing `**`: when preceded by a literal segment
				// (e.g. `secrets/**`), require at least one remaining
				// path segment. A bare leading `**` (no preceding
				// segments consumed) matches anything including a
				// single segment.
				res.ok = pathIdx < len(pathSegs)
			} else {
				// Zero segments, or consume one segment and remain on `**`.
				// With memoization these are constant-work transitions per state,
				// rather than scanning every remaining suffix from every state.
				ok, err := walk(patIdx+1, pathIdx)
				if err != nil || ok {
					res = result{ok, err}
				} else if pathIdx < len(pathSegs) {
					ok, err = walk(patIdx, pathIdx+1)
					res = result{ok, err}
				}
			}
		default:
			if pathIdx == len(pathSegs) {
				res = result{false, nil}
			} else {
				ok, err := path.Match(patSegs[patIdx], pathSegs[pathIdx])
				if err != nil {
					res = result{false, err}
				} else if !ok {
					res = result{false, nil}
				} else {
					ok, err := walk(patIdx+1, pathIdx+1)
					res = result{ok, err}
				}
			}
		}
		memo[key] = res
		return res.ok, res.err
	}
	return walk(0, 0)
}

// ValidatePathPattern returns an error if pattern is malformed. Rejects:
//   - empty pattern
//   - leading `/` (rooted absolute paths)
//   - trailing `/` (paths don't end in /; rules should match files)
//   - consecutive `/` (e.g. `a//b`)
//   - any segment for which stdlib path.Match would return ErrBadPattern
//     (e.g. `a/[unclosed`)
//   - more than maxPathPatternDblStars `**` segments or more than
//     maxPathPatternSegments total segments (U-13: bounds the memoized
//     matcher's state space so a rule can't plant combinatorial CPU on the
//     receive path; over-limit patterns fail at rule-creation time)
func ValidatePathPattern(pattern string) error {
	if err := validatePathPatternSyntax(pattern); err != nil {
		return err
	}
	segs := strings.Split(pattern, "/")
	if len(segs) > maxPathPatternSegments {
		return fmt.Errorf("too many segments in %q: %d (max %d)", pattern, len(segs), maxPathPatternSegments)
	}
	dblStars := 0
	for _, seg := range segs {
		if seg == "**" {
			dblStars++
			if dblStars > maxPathPatternDblStars {
				return fmt.Errorf("too many ** segments in %q: %d (max %d)", pattern, dblStars, maxPathPatternDblStars)
			}
		}
	}
	return nil
}

// validatePathPatternSyntax applies only the historical grammar. MatchPath
// uses it for backwards compatibility with persisted rules; new rules go
// through ValidatePathPattern and its resource caps as well.
func validatePathPatternSyntax(pattern string) error {
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
