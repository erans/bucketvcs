package main

import (
	"strings"
	"testing"
)

func TestSplitTenantRepo_RejectsNamesDurableKeysCannotRepresent_U9(t *testing.T) {
	for _, input := range []string{
		"acme/my.repo", ".hidden/repo", "acme/..", "acme/a..b",
		"acme/" + strings.Repeat("x", 129),
	} {
		if _, _, err := splitTenantRepo(input); err == nil {
			t.Errorf("splitTenantRepo(%q) succeeded", input)
		}
	}
	if tenant, repo, err := splitTenantRepo("acme/repo-1"); err != nil || tenant != "acme" || repo != "repo-1" {
		t.Fatalf("valid split = %q/%q, err=%v", tenant, repo, err)
	}
}
