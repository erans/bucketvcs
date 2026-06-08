package buildtrigger

import "testing"

func TestRefMatches(t *testing.T) {
	cases := []struct {
		name             string
		include, exclude []string
		ref              string
		want             bool
	}{
		{"empty include = all", nil, nil, "refs/heads/main", true},
		{"exact include", []string{"refs/heads/main"}, nil, "refs/heads/main", true},
		{"non-match include", []string{"refs/heads/main"}, nil, "refs/heads/dev", false},
		{"single-seg glob", []string{"refs/heads/release/*"}, nil, "refs/heads/release/1.0", true},
		{"single-seg glob too deep", []string{"refs/heads/release/*"}, nil, "refs/heads/release/a/b", false},
		{"tag glob", []string{"refs/tags/v*"}, nil, "refs/tags/v1.2.3", true},
		{"exclude wins", []string{"refs/heads/**"}, []string{"refs/heads/dependabot/**"}, "refs/heads/dependabot/x", false},
		{"exclude subtree, sibling passes", []string{"refs/heads/**"}, []string{"refs/heads/dependabot/**"}, "refs/heads/main", true},
		{"empty include + exclude", nil, []string{"refs/tags/**"}, "refs/tags/v1", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := RefMatches(c.include, c.exclude, c.ref)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if got != c.want {
				t.Fatalf("RefMatches(%v,%v,%q)=%v want %v", c.include, c.exclude, c.ref, got, c.want)
			}
		})
	}
}
