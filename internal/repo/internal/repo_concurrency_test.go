// Package repointernal hosts concurrency tests that exercise the public
// internal/repo API surface against a real ObjectStore. The
// PropertyManifestVersionMonotonic test is the M1 ship gate per the
// design doc §8.3 and is parametrized over the store factory so cloud
// adapters at M5 (R2 or S3) and M7 (the others) re-run the same
// invariants against their backend before claiming conformance.
package repointernal_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	tx "github.com/bucketvcs/bucketvcs/internal/repo/tx"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// StoreFactory returns a freshly-constructed ObjectStore plus a cleanup
// function. Future cloud-adapter tests provide their own factory and
// pass it to RunPropertyManifestVersionMonotonic.
type StoreFactory func(testing.TB) (storage.ObjectStore, func())

// RunPropertyManifestVersionMonotonic is the M1 ship-gate property
// test, parametrized over the store factory. Asserts:
//   - final manifest_version == 1 + N writers × M commits per writer
//   - latest_tx references a tx record that exists on disk with the
//     matching tx_id field
//   - every commit-tagged key is present in the final body
//   - no torn JSON during the run (parses succeed)
//
// Per-commit retry budget: inner = 16 (BackoffBase=0 for speed),
// outer = 100 logical attempts. Total worst case = 1600 attempts per
// commit, all without sleep, so a regression surfaces in seconds.
func RunPropertyManifestVersionMonotonic(t *testing.T, factory StoreFactory) {
	t.Helper()
	const (
		writers          = 8
		commitsPerWriter = 200
	)
	store, cleanup := factory(t)
	defer cleanup()

	ctx := context.Background()
	r, err := repo.Create(ctx, store, "acme", "stress", repo.CreateOptions{Actor: "u_init"})
	if err != nil {
		t.Fatal(err)
	}

	var (
		wg          sync.WaitGroup
		committedTx sync.Map // tx_id → struct{}
	)
	for w := 0; w < writers; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < commitsPerWriter; i++ {
				key := "k_" + strconv.Itoa(w*commitsPerWriter+i)
				const logicalRetryCap = 100
				var (
					txID  string
					cerr  error
				)
				for attempt := 0; attempt < logicalRetryCap; attempt++ {
					txID, cerr = r.Commit(ctx,
						tx.Body{Type: "push", Actor: "u_" + strconv.Itoa(w)},
						func(prev *repo.RootView) ([]byte, error) {
							var top map[string]json.RawMessage
							if err := json.Unmarshal(prev.Body, &top); err != nil {
								return nil, err
							}
							top[key] = json.RawMessage("true")
							return json.Marshal(top)
						},
						repo.WithCommitPolicy(repo.CommitPolicy{
							MaxRetries:  16,
							BackoffBase: 0,
						}),
					)
					if cerr == nil {
						break
					}
					var gaveUp *repo.CommitGaveUpError
					if errors.As(cerr, &gaveUp) {
						continue
					}
					t.Errorf("Commit failed (non-retryable): %v", cerr)
					return
				}
				if cerr != nil {
					t.Errorf("Commit exhausted %d logical retries", logicalRetryCap)
					return
				}
				committedTx.Store(txID, struct{}{})
			}
		}()
	}
	wg.Wait()

	view, err := r.ReadRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantManifestVersion := uint64(1 + writers*commitsPerWriter)
	if view.Header.ManifestVersion != wantManifestVersion {
		t.Errorf("ManifestVersion: want %d, got %d", wantManifestVersion, view.Header.ManifestVersion)
	}

	if _, ok := committedTx.Load(view.Header.LatestTx); !ok {
		t.Errorf("latest_tx %q not in in-memory committed set", view.Header.LatestTx)
	}

	// Verify the on-disk tx record for LatestTx exists, parses, and
	// matches its filename. This is the durability check missed by
	// the in-memory set assertion alone.
	if err := assertTxRecordIntegrity(t, store, view.Header.LatestTx, view.Header.ManifestVersion); err != nil {
		t.Errorf("LatestTx integrity: %v", err)
	}

	// Body sanity: every commit-tagged key present.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(view.Body, &top); err != nil {
		t.Fatal(err)
	}
	for w := 0; w < writers; w++ {
		for i := 0; i < commitsPerWriter; i++ {
			k := "k_" + strconv.Itoa(w*commitsPerWriter+i)
			if _, ok := top[k]; !ok {
				t.Errorf("body missing key %q", k)
			}
		}
	}
}

// assertTxRecordIntegrity reads the tx record at the canonical key,
// parses it, and asserts:
//   - the file exists at the predicted key
//   - the parsed tx_id equals txID
//   - the parsed base_manifest_version is < finalVersion (cannot equal
//     or exceed; the LatestTx attempt's base must precede the final)
func assertTxRecordIntegrity(t *testing.T, store storage.ObjectStore, txID string, finalVersion uint64) error {
	t.Helper()
	key := "tenants/acme/repos/stress/tx/" + txID + ".json"
	obj, err := store.Get(context.Background(), key, nil)
	if err != nil {
		return err
	}
	defer obj.Body.Close()
	raw, err := io.ReadAll(obj.Body)
	if err != nil {
		return err
	}
	var rec map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rec); err != nil {
		return err
	}
	if got := strings.Trim(string(rec["tx_id"]), `"`); got != txID {
		t.Errorf("tx record tx_id field = %q, expected %q (filename mismatch)", got, txID)
	}
	var base uint64
	if err := json.Unmarshal(rec["base_manifest_version"], &base); err != nil {
		return err
	}
	if base >= finalVersion {
		t.Errorf("LatestTx base_manifest_version=%d is not < finalVersion=%d", base, finalVersion)
	}
	return nil
}

// TestCommit_PropertyManifestVersionMonotonic runs the property test
// against the localfs adapter (the M1 reference store).
func TestCommit_PropertyManifestVersionMonotonic(t *testing.T) {
	RunPropertyManifestVersionMonotonic(t, func(tb testing.TB) (storage.ObjectStore, func()) {
		dir := tb.TempDir()
		s, err := localfs.Open(dir)
		if err != nil {
			tb.Fatal(err)
		}
		return s, func() { _ = s.Close() }
	})
}
