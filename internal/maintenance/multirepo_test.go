package maintenance

import (
	"bytes"
	"context"
	"sort"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestDiscoverRepos_FindsAllUnderTenants(t *testing.T) {
	dir := t.TempDir()
	s, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, key := range []string{
		"tenants/acme/repos/site/manifest/root.json",
		"tenants/acme/repos/api/manifest/root.json",
		"tenants/contoso/repos/web/manifest/root.json",
		"random/object",
	} {
		if _, err := s.PutIfAbsent(ctx, key, bytes.NewReader([]byte("{}")), nil); err != nil {
			t.Fatal(err)
		}
	}
	got, err := DiscoverRepos(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(got, func(i, j int) bool {
		if got[i].TenantID != got[j].TenantID {
			return got[i].TenantID < got[j].TenantID
		}
		return got[i].RepoID < got[j].RepoID
	})
	want := []RepoRef{
		{TenantID: "acme", RepoID: "api"},
		{TenantID: "acme", RepoID: "site"},
		{TenantID: "contoso", RepoID: "web"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i, r := range want {
		if got[i] != r {
			t.Errorf("repo[%d] = %+v, want %+v", i, got[i], r)
		}
	}
}

func TestDiscoverRepos_EmptyStore(t *testing.T) {
	dir := t.TempDir()
	s, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DiscoverRepos(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}
