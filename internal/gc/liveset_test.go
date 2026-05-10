package gc_test

import (
	"encoding/json"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gc"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

func TestBuildLiveSet_IncludesRootTxAndPackTriples(t *testing.T) {
	k, _ := keys.NewRepo("acme", "site")
	body := manifest.Body{
		DefaultBranch: "refs/heads/main",
		Refs:          map[string]string{},
		Packs: []manifest.PackEntry{
			{
				PackID: "abc", PackKey: "tenants/acme/repos/site/packs/canonical/abc.pack",
				IdxKey: "tenants/acme/repos/site/packs/canonical/abc.idx",
			},
		},
		Indexes: manifest.Indexes{
			ObjectMap:   &manifest.IndexRef{Key: "tenants/acme/repos/site/indexes/object-map/x.bvom"},
			CommitGraph: &manifest.IndexRef{Key: "tenants/acme/repos/site/indexes/commit-graph/y.bvcg"},
		},
	}
	bodyJSON, _ := json.Marshal(body)
	header := manifest.RootHeader{LatestTx: "tx_01HZ"}

	live := gc.BuildLiveSet(k, header, bodyJSON)

	want := []string{
		k.RootManifestKey(),
		k.TxRecordKey("tx_01HZ"),
		k.CommitMarkerKey("tx_01HZ"),
		"tenants/acme/repos/site/packs/canonical/abc.pack",
		"tenants/acme/repos/site/packs/canonical/abc.idx",
		"tenants/acme/repos/site/indexes/object-map/x.bvom",
		"tenants/acme/repos/site/indexes/commit-graph/y.bvcg",
	}
	for _, w := range want {
		if _, ok := live[w]; !ok {
			t.Errorf("live-set missing %q", w)
		}
	}
}

func TestBuildLiveSet_EmptyBodyJustHasHeaderKeys(t *testing.T) {
	k, _ := keys.NewRepo("acme", "site")
	header := manifest.RootHeader{LatestTx: "tx_01HZ"}
	live := gc.BuildLiveSet(k, header, []byte(`{}`))
	if _, ok := live[k.RootManifestKey()]; !ok {
		t.Error("live-set must contain root manifest key")
	}
	if _, ok := live[k.TxRecordKey("tx_01HZ")]; !ok {
		t.Error("live-set must contain latest_tx record key")
	}
}
