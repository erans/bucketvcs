package lfs

import "testing"

func TestRepoLFSPrefix(t *testing.T) {
	got := RepoLFSPrefix("acme", "foo")
	want := "tenants/acme/repos/foo/lfs/objects/"
	if got != want {
		t.Errorf("RepoLFSPrefix = %q, want %q", got, want)
	}
}
