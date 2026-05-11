package maintenance

import (
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

func TestResolveDefaultBranch_HEAD(t *testing.T) {
	m := manifest.Body{
		DefaultBranch: "refs/heads/develop",
		Refs: map[string]string{
			"refs/heads/develop": "0123456789abcdef0123456789abcdef01234567",
			"refs/heads/main":    "1111111111111111111111111111111111111111",
		},
	}
	got, err := ResolveDefaultBranch(m, "")
	if err != nil {
		t.Fatalf("ResolveDefaultBranch: %v", err)
	}
	if got != "refs/heads/develop" {
		t.Errorf("got %q, want refs/heads/develop", got)
	}
}

func TestResolveDefaultBranch_Override(t *testing.T) {
	m := manifest.Body{
		DefaultBranch: "refs/heads/develop",
		Refs: map[string]string{
			"refs/heads/develop": "0123456789abcdef0123456789abcdef01234567",
			"refs/heads/main":    "1111111111111111111111111111111111111111",
		},
	}
	got, err := ResolveDefaultBranch(m, "refs/heads/main")
	if err != nil {
		t.Fatalf("ResolveDefaultBranch: %v", err)
	}
	if got != "refs/heads/main" {
		t.Errorf("got %q, want refs/heads/main (override)", got)
	}
}

func TestResolveDefaultBranch_FallbackMain(t *testing.T) {
	m := manifest.Body{
		Refs: map[string]string{"refs/heads/main": "0123456789abcdef0123456789abcdef01234567"},
	}
	got, err := ResolveDefaultBranch(m, "")
	if err != nil {
		t.Fatalf("ResolveDefaultBranch: %v", err)
	}
	if got != "refs/heads/main" {
		t.Errorf("got %q, want refs/heads/main", got)
	}
}

func TestResolveDefaultBranch_FallbackMaster(t *testing.T) {
	m := manifest.Body{
		Refs: map[string]string{"refs/heads/master": "0123456789abcdef0123456789abcdef01234567"},
	}
	got, err := ResolveDefaultBranch(m, "")
	if err != nil {
		t.Fatalf("ResolveDefaultBranch: %v", err)
	}
	if got != "refs/heads/master" {
		t.Errorf("got %q, want refs/heads/master", got)
	}
}

func TestResolveDefaultBranch_NoMatch(t *testing.T) {
	m := manifest.Body{
		Refs: map[string]string{"refs/heads/feat": "0123456789abcdef0123456789abcdef01234567"},
	}
	_, err := ResolveDefaultBranch(m, "")
	if err == nil {
		t.Fatalf("expected error when no default branch can be resolved")
	}
}

func TestResolveDefaultBranch_OverrideMissingRef(t *testing.T) {
	m := manifest.Body{Refs: map[string]string{"refs/heads/main": "abcd"}}
	_, err := ResolveDefaultBranch(m, "refs/heads/missing")
	if err == nil {
		t.Fatalf("expected error when override ref does not exist")
	}
}
