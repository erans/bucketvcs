package policy_test

import (
	"context"
	"fmt"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/policy"
)

// Wave-4 U-13 regression coverage.
//
// (a) Adversarial patterns (>2 `**` segments or >64 total segments) made the
//     matchSegments recursion O(n^k) on the receive-pack path; ValidatePathPattern
//     must now reject them at rule-creation time.
// (b) matchSegments is memoized over (patternIdx, pathIdx). The memoized
//     matcher must be bit-identical to the naive recursion for every in-limit
//     pattern; referenceMatch below is an exact copy of the pre-fix recursion,
//     and the battery compares both matchers over a truth table that includes
//     leading/trailing/bare `**` edge cases.

// referenceMatch runs the pre-fix (uncapped) path: matchSegments recursion on
// the raw segment splits. Every battery entry satisfies the pre-existing
// shape checks (non-empty segments etc.), so the skipped validation is
// inert here.
func referenceMatch(pattern, p string) (bool, error) {
	patSegs := strings.Split(pattern, "/")
	pathSegs := strings.Split(p, "/")
	return referenceMatchSegments(patSegs, pathSegs)
}

// referenceMatchSegments: verbatim copy of the pre-fix (unmemoized) matchSegments.
func referenceMatchSegments(patSegs, pathSegs []string) (bool, error) {
	for len(patSegs) > 0 {
		head := patSegs[0]
		if head == "**" {
			rest := patSegs[1:]
			if len(rest) == 0 {
				return len(pathSegs) > 0, nil
			}
			for i := 0; i <= len(pathSegs); i++ {
				ok, err := referenceMatchSegments(rest, pathSegs[i:])
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

// u13BatteryPatterns: in-limit patterns (<=2 `**`, <=64 segments) spanning
// bare/leading/trailing/embedded `**`, `*`, `?`, char classes, and literals.
var u13BatteryPatterns = []string{
	// bare / leading / trailing `**`
	"**", "**/a", "**/b", "a/**", "b/**", "**/*.txt", "**/[ab]",
	// embedded + double `**`
	"a/**/b", "a/**/c", "**/a/**", "a/**/**", "**/**/b",
	// no `**` baseline
	"a", "b", "*.txt", "?", "[ab]", "a/b", "a/b/c", "*/b/*",
	// mixed
	"a/**/b/**", "**/a/b", "a/b/**", "*/**/c",
}

// u13BatteryPaths: 1-4 segment paths over a small alphabet (plus a dotfile
// segment) — enough to exercise zero/many-segment `**` expansion both ways.
var u13BatteryPaths = []string{
	"a", "b", "c", "a/a", "a/b", "b/a", "a/b/c", "a/b/c/d", "a.txt", "b.txt",
	"dir/a.txt", ".env", "a/.env", "a/b/.env", "x", "x/y/z", "a/c/b",
	"cc", "a/cc", "cc/a",
}

func TestU13_MatchPathEqualsReference_Battery(t *testing.T) {
	for _, pattern := range u13BatteryPatterns {
		for _, p := range u13BatteryPaths {
			t.Run(fmt.Sprintf("%q vs %q", pattern, p), func(t *testing.T) {
				got, gotErr := policy.MatchPath(pattern, p)
				want, wantErr := referenceMatch(pattern, p)
				if got != want {
					t.Errorf("match mismatch: got %v, reference %v", got, want)
				}
				if (gotErr == nil) != (wantErr == nil) {
					t.Errorf("error mismatch: got %v, reference %v", gotErr, wantErr)
				}
			})
		}
	}
	// Long-path spot check: 2-`**` pattern against a 400-segment path must
	// complete (memoized) and agree with the reference.
	deep := strings.TrimSuffix(strings.Repeat("a/", 399), "/") + "/x/tail"
	got, err := policy.MatchPath("**/x/**", deep)
	if err != nil {
		t.Fatalf("MatchPath deep: %v", err)
	}
	want, werr := referenceMatch("**/x/**", deep)
	if werr != nil || got != want {
		t.Fatalf("deep path: got (%v, %v), reference (%v, %v)", got, err, want, werr)
	}
	if !got {
		t.Fatal("**/x/** vs a/.../x/tail: want true (trailing ** consumes tail)")
	}
	got2, _ := policy.MatchPath("**/zzz/**", deep)
	want2, _ := referenceMatch("**/zzz/**", deep)
	if got2 != want2 || got2 {
		t.Fatalf("deep negative: got %v reference %v", got2, want2)
	}
}

func TestU13_MemoizedMatcherIsPolynomial(t *testing.T) {
	// The unmemoized recursion is combinatorial in (segments, `**` count).
	// With in-limit inputs the memoized matcher must be comfortably fast; a
	// generous wall-clock bound catches an accidental non-memoized regression
	// without being flaky on slow CI.
	pathSegs := strings.TrimSuffix(strings.Repeat("z/", 400), "/")
	start := time.Now()
	for i := 0; i < 50; i++ {
		_, err := policy.MatchPath("**/needle/**/end", pathSegs)
		if err != nil {
			t.Fatalf("MatchPath: %v", err)
		}
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("50 in-limit matches took %v; memoization appears absent", elapsed)
	}
}

func TestU13_ValidatePathPattern_Caps(t *testing.T) {
	t.Run("third double-star rejected", func(t *testing.T) {
		// The currently-slow adversarial case from the adjudication.
		if err := policy.ValidatePathPattern("**/x/**/y/**"); err == nil {
			t.Fatal("3 `**` segments accepted; want rejection")
		}
		// Existing persisted rules from before the cap must remain evaluable;
		// only creation-time validation rejects new over-cap patterns.
		matched, err := policy.MatchPath("**/x/**/y/**", "a/x/b/y/c")
		if err != nil || !matched {
			t.Fatalf("legacy over-cap rule match = %v, err=%v; want true, nil", matched, err)
		}
	})
	t.Run("over-segment-cap rejected", func(t *testing.T) {
		long := strings.TrimSuffix(strings.Repeat("a/", 64), "/") // 64 segments
		if err := policy.ValidatePathPattern(long); err != nil {
			t.Fatalf("64 segments (at cap) must stay valid: %v", err)
		}
		over := long + "/a" // 65 segments
		if err := policy.ValidatePathPattern(over); err == nil {
			t.Fatal("65 segments accepted; want rejection")
		}
	})
	t.Run("at-cap double-stars stay valid", func(t *testing.T) {
		for _, p := range []string{"**", "**/x", "x/**", "a/**/b", "**/x/**", "a/**/**"} {
			if err := policy.ValidatePathPattern(p); err != nil {
				t.Errorf("in-limit pattern %q rejected: %v", p, err)
			}
		}
	})
}

func TestU13_LegacyMatcherHasEvaluationBudget_Review(t *testing.T) {
	// Legacy rows bypass creation caps for compatibility, but evaluation must
	// still have a hard resource ceiling. This shape forces the matcher to
	// explore a large globstar/path state space before proving no match.
	pattern := strings.Repeat("**/", 300) + "never"
	candidate := strings.TrimSuffix(strings.Repeat("a/", 300), "/")
	if _, err := policy.MatchPath(pattern, candidate); err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("MatchPath legacy resource shape error = %v, want budget error", err)
	}
}

func TestU13_CheckPathsHasCumulativeEvaluationBudget_Review(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	pattern := strings.Repeat("**/", 100) + "never"
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO protected_paths
		  (tenant, repo, refname_pattern, path_pattern, created_at)
		VALUES (?, ?, ?, ?, 1)`, "acme", "site", "refs/heads/*", pattern); err != nil {
		t.Fatal(err)
	}
	candidate := strings.TrimSuffix(strings.Repeat("a/", 100), "/")
	changed := make([]string, 40)
	for i := range changed {
		changed[i] = candidate
	}
	err := policy.New(db).CheckPaths(context.Background(), "acme", "site", "refs/heads/main", changed)
	if err == nil || !strings.Contains(err.Error(), "cumulative") {
		t.Fatalf("CheckPaths error = %v, want cumulative budget error", err)
	}
}

func TestU13_AddPathRuleRejectsOverCapPattern(t *testing.T) {
	// Creation-time enforcement: an over-`**` path rule must fail AddPathRule
	// (the store boundary every writer flows through).
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db)
	ctx := context.Background()
	if err := svc.AddPathRule(ctx, policy.ProtectedPath{
		Tenant:         "acme",
		Repo:           "app",
		RefnamePattern: "refs/heads/*",
		PathPattern:    "**/x/**/y/**",
	}); err == nil {
		t.Fatal("over-cap PathPattern accepted at rule creation")
	}
}
