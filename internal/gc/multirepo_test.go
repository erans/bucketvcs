package gc_test

import (
	"context"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gc"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestDiscoverRepos_FindsAllUnderTenants(t *testing.T) {
	store, _ := localfs.Open(t.TempDir())
	ctx := context.Background()
	for _, tr := range []struct{ tenant, repo string }{
		{"acme", "site"},
		{"acme", "blog"},
		{"globex", "infra"},
	} {
		if _, err := repo.Create(ctx, store, tr.tenant, tr.repo, repo.CreateOptions{Actor: "u_test"}); err != nil {
			t.Fatalf("Create %s/%s: %v", tr.tenant, tr.repo, err)
		}
	}
	got, err := gc.DiscoverRepos(ctx, store)
	if err != nil {
		t.Fatalf("DiscoverRepos: %v", err)
	}
	want := []gc.RepoRef{
		{TenantID: "acme", RepoID: "blog"},
		{TenantID: "acme", RepoID: "site"},
		{TenantID: "globex", RepoID: "infra"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d repos, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
