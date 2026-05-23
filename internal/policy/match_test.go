package policy_test

import (
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/policy"
)

func TestMatchPath(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"*.md", "README.md", true},
		{"*.md", "docs/README.md", false},
		{"foo", "foo", true},
		{"foo", "bar", false},
		{"f?o", "foo", true},
		{"f?o", "fooo", false},
		{"[abc]oo", "boo", true},
		{"[abc]oo", "doo", false},

		{"secrets/**", "secrets/keys.txt", true},
		{"secrets/**", "secrets/dev/.env", true},
		{"secrets/**", "secrets", false},
		{"secrets/**", "not-secrets/x", false},
		{".github/workflows/*", ".github/workflows/ci.yml", true},
		{".github/workflows/*", ".github/workflows/nested/run.yml", false},
		{"**/*.lock", "go.sum.lock", true},
		{"**/*.lock", "frontend/yarn.lock", true},
		{"**/*.lock", "a/b/c.lock", true},
		{"**/*.lock", "lock.go", false},
		{"**", "a", true},
		{"**", "a/b/c/d", true},
		{"**/secrets/**", "app/secrets/x", true},
		{"**/secrets/**", "secrets/k", true},
		{"**/secrets/**", "a/b/secrets/c/d", true},
		{"**/secrets/**", "mysecrets/x", false},
		{"**/secrets/**", "secrets-old/x", false},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"a/**/b", "a/x/c", false},
	}
	for _, c := range cases {
		t.Run(c.pattern+" vs "+c.path, func(t *testing.T) {
			got, err := policy.MatchPath(c.pattern, c.path)
			if err != nil {
				t.Fatalf("MatchPath(%q, %q) error: %v", c.pattern, c.path, err)
			}
			if got != c.want {
				t.Errorf("MatchPath(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
			}
		})
	}
}

func TestValidatePathPattern(t *testing.T) {
	good := []string{"foo", "secrets/**", "*.md", "a/[abc]/b", "**/*.lock"}
	for _, p := range good {
		if err := policy.ValidatePathPattern(p); err != nil {
			t.Errorf("ValidatePathPattern(%q) = %v, want nil", p, err)
		}
	}
	bad := []string{"", "a//b", "/foo", "foo/", "a/[unclosed"}
	for _, p := range bad {
		if err := policy.ValidatePathPattern(p); err == nil {
			t.Errorf("ValidatePathPattern(%q) returned nil; want error", p)
		}
	}
}
