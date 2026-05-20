package receivepack

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest/manifesttest"
	"github.com/bucketvcs/bucketvcs/internal/repo/tx"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// TestAdvertise_V0_ShardedBody verifies that the v0 receive-pack advertisement
// works for sharded (v2) manifests by routing through refstore.List instead of
// direct body.Refs reads. The test drives the full Advertise() path (repo.Open →
// refstore.New → rs.List) so that the refstore wiring is actually exercised.
func TestAdvertise_V0_ShardedBody(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	store, err := localfs.Open(tmp)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	defer store.Close()

	const tenant, repoID = "acme", "shard-demo"

	// Create the repo (writes an empty inline-body root manifest).
	r, err := repo.Create(ctx, store, tenant, repoID, repo.CreateOptions{
		DefaultBranch: "refs/heads/main",
	})
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}

	k, err := keys.NewRepo(tenant, repoID)
	if err != nil {
		t.Fatalf("keys.NewRepo: %v", err)
	}

	wantRefs := map[string]string{
		"refs/heads/main": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"refs/heads/dev":  "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"refs/tags/v1.0":  "cccccccccccccccccccccccccccccccccccccccc",
	}

	// Build the sharded body and write every shard object into the store.
	shardedBody, err := manifesttest.MakeShardedBody(ctx, store, k, "refs/heads/main", wantRefs)
	if err != nil {
		t.Fatalf("MakeShardedBody: %v", err)
	}

	// Replace the root manifest body with the sharded layout so that
	// Advertise's repo.Open → refstore.New → rs.List path exercises the
	// shard-read code rather than the inline body.Refs fast path.
	bodyBytes, err := manifest.MarshalBody(shardedBody)
	if err != nil {
		t.Fatalf("MarshalBody: %v", err)
	}
	if _, err := r.Commit(ctx, tx.Body{Type: "test", Actor: "test"}, func(_ *repo.RootView) ([]byte, error) {
		return bodyBytes, nil
	}); err != nil {
		t.Fatalf("r.Commit: %v", err)
	}

	var buf bytes.Buffer
	req := &EngineRequest{
		Ctx:          ctx,
		Tenant:       tenant,
		Repo:         repoID,
		Stdout:       &buf,
		Store:        store,
		AgentVersion: "test",
	}
	if err := Advertise(req); err != nil {
		t.Fatalf("Advertise: %v", err)
	}

	got := buf.String()

	// Every ref in wantRefs must appear in the advertisement.
	for name := range wantRefs {
		if !strings.Contains(got, name) {
			t.Errorf("advertise output missing ref %q\nfull output:\n%s", name, got)
		}
	}

	// receive-pack must NOT advertise HEAD.
	if strings.Contains(got, " HEAD\x00") || strings.Contains(got, " HEAD\n") {
		t.Errorf("receive-pack must not advertise HEAD\nfull output:\n%s", got)
	}

	// Must end with flush packet.
	if !strings.HasSuffix(got, "0000") {
		t.Errorf("output does not end with flush packet '0000'\nfull output:\n%s", got)
	}
}
