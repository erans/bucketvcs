package gc_test

import (
	"context"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gc"
	"github.com/bucketvcs/bucketvcs/internal/gc/gctest"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestDiscover_OrphanCanonicalPacks(t *testing.T) {
	store, _ := localfs.Open(t.TempDir())
	k, _ := keys.NewRepo("acme", "site")
	live := gc.LiveSet{
		k.CanonicalPackKey("live"):        {},
		k.PackIdxKey("live", "canonical"): {},
	}
	gctest.PutEmpty(t, store, k.CanonicalPackKey("live"))
	gctest.PutEmpty(t, store, k.PackIdxKey("live", "canonical"))
	gctest.PutEmpty(t, store, k.CanonicalPackKey("orphan1"))
	gctest.PutEmpty(t, store, k.CanonicalPackKey("orphan2"))

	got, err := gc.DiscoverCanonicalPacks(context.Background(), store, k, live)
	if err != nil {
		t.Fatalf("DiscoverCanonicalPacks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d candidates, want 2", len(got))
	}
}

func TestDiscover_OrphanIndexes(t *testing.T) {
	store, _ := localfs.Open(t.TempDir())
	k, _ := keys.NewRepo("acme", "site")
	live := gc.LiveSet{k.ObjectMapKey("livehash"): {}}
	gctest.PutEmpty(t, store, k.ObjectMapKey("livehash"))
	gctest.PutEmpty(t, store, k.ObjectMapKey("orphanhash"))
	gctest.PutEmpty(t, store, k.CommitGraphKey("orphan-cg"))
	gctest.PutEmpty(t, store, k.ReachabilityKey("orphan-r"))

	got, err := gc.DiscoverIndexes(context.Background(), store, k, live)
	if err != nil {
		t.Fatalf("DiscoverIndexes: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d candidates, want 3", len(got))
	}
}

func TestDiscover_OrphanBundles(t *testing.T) {
	store, _ := localfs.Open(t.TempDir())
	k, _ := keys.NewRepo("acme", "site")
	live := gc.LiveSet{
		k.BundleKey("live"):         {},
		k.BundleManifestKey("live"): {},
	}
	gctest.PutEmpty(t, store, k.BundleKey("live"))
	gctest.PutEmpty(t, store, k.BundleManifestKey("live"))
	gctest.PutEmpty(t, store, k.BundleKey("orphan"))
	gctest.PutEmpty(t, store, k.BundleManifestKey("orphan"))

	got, err := gc.DiscoverBundles(context.Background(), store, k, live)
	if err != nil {
		t.Fatalf("DiscoverBundles: %v", err)
	}
	want := map[string]bool{
		k.BundleKey("orphan"):         true,
		k.BundleManifestKey("orphan"): true,
	}
	gotSet := map[string]bool{}
	for _, key := range got {
		gotSet[key] = true
	}
	for k := range want {
		if !gotSet[k] {
			t.Errorf("missing candidate %q", k)
		}
	}
	for k := range gotSet {
		if !want[k] {
			t.Errorf("unexpected candidate %q (want one of %v)", k, want)
		}
	}
}

func TestDiscover_OrphanTxRecords_ExcludesMarkedAndCurrent(t *testing.T) {
	store, _ := localfs.Open(t.TempDir())
	k, _ := keys.NewRepo("acme", "site")

	// "winner" tx with a commit marker.
	gctest.PutEmpty(t, store, k.TxRecordKey("tx_winner"))
	gctest.PutEmpty(t, store, k.CommitMarkerKey("tx_winner"))
	// "current" tx (latest_tx of the manifest).
	gctest.PutEmpty(t, store, k.TxRecordKey("tx_current"))
	// "orphan" tx with no marker.
	gctest.PutEmpty(t, store, k.TxRecordKey("tx_orphan"))

	live := gc.LiveSet{
		k.TxRecordKey("tx_current"):     {},
		k.CommitMarkerKey("tx_current"): {},
	}

	cands, orphanMarkers, armed, err := gc.DiscoverTxRecords(context.Background(), store, k, live)
	if err != nil {
		t.Fatalf("DiscoverTxRecords: %v", err)
	}
	if !armed {
		t.Error("tx_orphan_sweep_armed must be true (a marker was observed)")
	}
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1", len(cands))
	}
	if cands[0] != k.TxRecordKey("tx_orphan") {
		t.Fatalf("got candidate %q, want %q", cands[0], k.TxRecordKey("tx_orphan"))
	}
	if len(orphanMarkers) != 0 {
		t.Fatalf("got %d orphan markers, want 0 (sole marker has a record)", len(orphanMarkers))
	}
}

// TestDiscoverTxRecords_OrphanMarkers is the B10 regression: markers
// without a sibling tx record must be enumerated (retention-gated
// sweep candidates) instead of accumulating forever.
func TestDiscoverTxRecords_OrphanMarkers(t *testing.T) {
	ctx := context.Background()
	store, _ := localfs.Open(t.TempDir())
	k, _ := keys.NewRepo("acme", "site")
	// Live tx with live marker.
	gctest.PutEmpty(t, store, k.TxRecordKey("tx_live"))
	gctest.PutEmpty(t, store, k.CommitMarkerKey("tx_live"))
	// Orphan marker: no sibling tx record.
	gctest.PutEmpty(t, store, k.CommitMarkerKey("tx_gone"))

	live := gc.LiveSet{
		k.TxRecordKey("tx_live"):     {},
		k.CommitMarkerKey("tx_live"): {},
	}

	cands, orphanMarkers, armed, err := gc.DiscoverTxRecords(ctx, store, k, live)
	if err != nil {
		t.Fatalf("DiscoverTxRecords: %v", err)
	}
	if !armed {
		t.Error("armed must be true (markers observed)")
	}
	if len(cands) != 0 {
		t.Fatalf("got %d tx candidates, want 0", len(cands))
	}
	if len(orphanMarkers) != 1 || orphanMarkers[0] != k.CommitMarkerKey("tx_gone") {
		t.Fatalf("got orphan markers %v, want [%q]", orphanMarkers, k.CommitMarkerKey("tx_gone"))
	}
}
