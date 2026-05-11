package keys_test

import (
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
)

func TestNewRepo_ValidIDs(t *testing.T) {
	cases := []string{"a", "abc", "acme-prod_1", "A1Z9_-", repeatChar("x", 128)}
	for _, id := range cases {
		if _, err := keys.NewRepo(id, id); err != nil {
			t.Errorf("expected NewRepo(%q,%q) ok, got %v", id, id, err)
		}
	}
}

func TestNewRepo_InvalidIDs(t *testing.T) {
	cases := []struct {
		tenant, repo string
		wantErr      error
	}{
		{"", "ok", repo.ErrInvalidTenantID},
		{"ok", "", repo.ErrInvalidRepoID},
		{"a/b", "ok", repo.ErrInvalidTenantID},
		{"ok", "a..b", repo.ErrInvalidRepoID},
		{"ok", "a b", repo.ErrInvalidRepoID},
		{repeatChar("x", 129), "ok", repo.ErrInvalidTenantID},
		{"ok", repeatChar("x", 129), repo.ErrInvalidRepoID},
		{"ok", ".", repo.ErrInvalidRepoID},
		{"ok", "..", repo.ErrInvalidRepoID},
	}
	for _, c := range cases {
		_, err := keys.NewRepo(c.tenant, c.repo)
		if !errors.Is(err, c.wantErr) {
			t.Errorf("NewRepo(%q,%q): want %v, got %v", c.tenant, c.repo, c.wantErr, err)
		}
	}
}

func TestRepoPrefix(t *testing.T) {
	r, err := keys.NewRepo("acme", "my-repo")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := r.Prefix(), "tenants/acme/repos/my-repo/"; got != want {
		t.Errorf("Prefix: want %q, got %q", want, got)
	}
}

func TestRootManifestKey(t *testing.T) {
	r, _ := keys.NewRepo("acme", "my-repo")
	if got, want := r.RootManifestKey(), "tenants/acme/repos/my-repo/manifest/root.json"; got != want {
		t.Errorf("RootManifestKey: want %q, got %q", want, got)
	}
}

func TestTxRecordKey(t *testing.T) {
	r, _ := keys.NewRepo("acme", "my-repo")
	id := "01HW7JSXEMABCDEF0123456789"
	want := "tenants/acme/repos/my-repo/tx/" + id + ".json"
	if got := r.TxRecordKey(id); got != want {
		t.Errorf("TxRecordKey: want %q, got %q", want, got)
	}
}

func TestTxPrefix(t *testing.T) {
	r, _ := keys.NewRepo("acme", "my-repo")
	if got, want := r.TxPrefix(), "tenants/acme/repos/my-repo/tx/"; got != want {
		t.Errorf("TxPrefix: want %q, got %q", want, got)
	}
}

func repeatChar(c string, n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = c[0]
	}
	return string(out)
}

func TestPackKeys(t *testing.T) {
	r, _ := keys.NewRepo("acme", "my-repo")
	hash := "sha256-abc"
	cases := []struct {
		got, want string
	}{
		{r.CanonicalPackKey(hash), "tenants/acme/repos/my-repo/packs/canonical/" + hash + ".pack"},
		{r.GeneratedPackKey(hash), "tenants/acme/repos/my-repo/packs/generated/" + hash + ".pack"},
		{r.PackIdxKey(hash, "canonical"), "tenants/acme/repos/my-repo/packs/canonical/" + hash + ".idx"},
		{r.PackBitmapKey(hash), "tenants/acme/repos/my-repo/packs/canonical/" + hash + ".bitmap"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("want %q, got %q", c.want, c.got)
		}
	}
}

func TestIndexAndBundleKeys(t *testing.T) {
	r, _ := keys.NewRepo("acme", "my-repo")
	cases := []struct{ got, want string }{
		{r.CommitGraphKey("g1"), "tenants/acme/repos/my-repo/indexes/commit-graph/g1.bvcg"},
		{r.ObjectMapKey("o1"), "tenants/acme/repos/my-repo/indexes/object-map/o1.bvom"},
		{r.ReachabilityKey("i1"), "tenants/acme/repos/my-repo/indexes/reachability/i1.json"},
		{r.BundleKey("b1"), "tenants/acme/repos/my-repo/bundles/b1.bundle"},
		{r.BundleManifestKey("b1"), "tenants/acme/repos/my-repo/bundles/b1.json"},
		{r.LFSObjectKey("sha"), "tenants/acme/repos/my-repo/lfs/objects/sha"},
		{r.HookKey("h1", "pre-receive"), "tenants/acme/repos/my-repo/hooks/h1/pre-receive"},
		{r.GCMarkKey("m1"), "tenants/acme/repos/my-repo/gc/marks/m1.json"},
		{r.GCSweepKey("s1"), "tenants/acme/repos/my-repo/gc/sweeps/s1.json"},
		{r.RefShardKey("rs1"), "tenants/acme/repos/my-repo/manifest/ref-shards/rs1.json"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("want %q, got %q", c.want, c.got)
		}
	}
}

func TestCommitMarkerKey(t *testing.T) {
	r, err := keys.NewRepo("acme", "site")
	if err != nil {
		t.Fatalf("NewRepo: %v", err)
	}
	got := r.CommitMarkerKey("tx_01HZSAMPLE")
	want := "tenants/acme/repos/site/tx/tx_01HZSAMPLE.json.commit"
	if got != want {
		t.Fatalf("CommitMarkerKey = %q, want %q", got, want)
	}
}

func TestPackIdxKey_RejectsBadArea(t *testing.T) {
	r, _ := keys.NewRepo("acme", "my-repo")
	defer func() {
		if recover() == nil {
			t.Fatalf("expected panic for bad area")
		}
	}()
	_ = r.PackIdxKey("h", "loose")
}

func TestReachabilityDeltaKey(t *testing.T) {
	r, err := keys.NewRepo("t", "r")
	if err != nil {
		t.Fatalf("NewRepo: %v", err)
	}
	got := r.ReachabilityDeltaKey("abcd")
	want := "tenants/t/repos/r/indexes/reachability-delta/abcd.bvrd"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestReachabilityDeltaPrefix(t *testing.T) {
	r, _ := keys.NewRepo("t", "r")
	got := r.ReachabilityDeltaPrefix()
	want := "tenants/t/repos/r/indexes/reachability-delta/"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
