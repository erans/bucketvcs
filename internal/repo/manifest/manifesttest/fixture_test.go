package manifesttest_test

import (
	"context"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest/manifesttest"
	"github.com/bucketvcs/bucketvcs/internal/repo/refstore"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestMakeShardedBody_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	store, err := localfs.Open(tmp)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	defer store.Close()
	k, _ := keys.NewRepo("acme", "demo")
	refs := map[string]string{
		"refs/heads/main": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"refs/heads/dev":  "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	body, err := manifesttest.MakeShardedBody(context.Background(), store, k, "refs/heads/main", refs)
	if err != nil {
		t.Fatalf("MakeShardedBody: %v", err)
	}
	rs, err := refstore.New(context.Background(), store, k, &body)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	out, err := rs.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for k, v := range refs {
		if out[k] != v {
			t.Errorf("ref %q: got=%q want=%q", k, out[k], v)
		}
	}
}
