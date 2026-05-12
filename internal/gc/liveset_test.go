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

	live, err := gc.BuildLiveSet(k, header, bodyJSON)
	if err != nil {
		t.Fatalf("BuildLiveSet: %v", err)
	}

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

func TestWalk_IncludesReachabilityDeltas(t *testing.T) {
	k, _ := keys.NewRepo("acme", "site")
	deltaKey1 := k.ReachabilityDeltaKey("aabbcc")
	deltaKey2 := k.ReachabilityDeltaKey("ddeeff")
	body := manifest.Body{
		DefaultBranch: "refs/heads/main",
		Refs:          map[string]string{},
		Indexes: manifest.Indexes{
			Reachability: &manifest.ReachabilityRef{
				BaseManifest: "v1",
				Deltas: []manifest.IndexRef{
					{Key: deltaKey1, Hash: "aabbcc"},
					{Key: deltaKey2, Hash: "ddeeff"},
				},
			},
		},
	}
	bodyJSON, _ := json.Marshal(body)
	header := manifest.RootHeader{LatestTx: "tx_01HZ"}

	live, err := gc.BuildLiveSet(k, header, bodyJSON)
	if err != nil {
		t.Fatalf("BuildLiveSet: %v", err)
	}
	for _, want := range []string{deltaKey1, deltaKey2} {
		if _, ok := live[want]; !ok {
			t.Errorf("live-set missing reachability delta key %q", want)
		}
	}
}

func TestBuildLiveSet_IncludesBundleKeys(t *testing.T) {
	k, _ := keys.NewRepo("acme", "r")

	bundleKey := "tenants/acme/repos/r/bundles/sha256-aabbccdd.bundle"
	sidecarKey := "tenants/acme/repos/r/bundles/sha256-aabbccdd.bundle.json"

	body := manifest.Body{
		DefaultBranch: "refs/heads/main",
		Refs:          map[string]string{},
		Bundles: []manifest.BundleEntry{
			{BundleKey: bundleKey, SidecarKey: sidecarKey},
		},
	}
	bodyJSON, _ := json.Marshal(body)
	header := manifest.RootHeader{}

	live, err := gc.BuildLiveSet(k, header, bodyJSON)
	if err != nil {
		t.Fatalf("BuildLiveSet: %v", err)
	}
	if _, ok := live[bundleKey]; !ok {
		t.Errorf("live-set missing BundleKey %q", bundleKey)
	}
	if _, ok := live[sidecarKey]; !ok {
		t.Errorf("live-set missing SidecarKey %q", sidecarKey)
	}

	// An entry with empty keys must not add "" to the live set.
	body2 := manifest.Body{
		DefaultBranch: "refs/heads/main",
		Refs:          map[string]string{},
		Bundles:       []manifest.BundleEntry{{BundleKey: "", SidecarKey: ""}},
	}
	bodyJSON2, _ := json.Marshal(body2)
	live2, err := gc.BuildLiveSet(k, header, bodyJSON2)
	if err != nil {
		t.Fatalf("BuildLiveSet (empty keys): %v", err)
	}
	if _, ok := live2[""]; ok {
		t.Error("live-set must not contain empty-string key from empty BundleEntry")
	}
}

func TestBuildLiveSet_EmptyBodyJustHasHeaderKeys(t *testing.T) {
	k, _ := keys.NewRepo("acme", "site")
	header := manifest.RootHeader{LatestTx: "tx_01HZ"}
	live, err := gc.BuildLiveSet(k, header, []byte(`{}`))
	if err != nil {
		t.Fatalf("BuildLiveSet: %v", err)
	}
	if _, ok := live[k.RootManifestKey()]; !ok {
		t.Error("live-set must contain root manifest key")
	}
	if _, ok := live[k.TxRecordKey("tx_01HZ")]; !ok {
		t.Error("live-set must contain latest_tx record key")
	}
}
