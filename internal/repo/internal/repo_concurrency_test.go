// Package repointernal hosts concurrency tests that exercise the public
// internal/repo API surface against a real localfs store. These tests
// are the M1 ship gate per the design doc §8.3.
//
// "Cloud adapters at M5 (R2 or S3) and M7 (the others) MUST run this
// same suite against their backend before claiming conformance."
package repointernal_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	tx "github.com/bucketvcs/bucketvcs/internal/repo/tx"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestCommit_PropertyManifestVersionMonotonic(t *testing.T) {
	const (
		writers          = 8
		commitsPerWriter = 200
	)
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	r, err := repo.Create(ctx, store, "acme", "stress", repo.CreateOptions{Actor: "u_init"})
	if err != nil {
		t.Fatal(err)
	}

	var (
		wg          sync.WaitGroup
		seq         atomic.Int64
		committedTx sync.Map
	)
	for w := 0; w < writers; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < commitsPerWriter; i++ {
				key := "k_" + strconv.Itoa(w*commitsPerWriter+i)
				var (
					txID string
					err  error
				)
				const logicalRetryCap = 1000
				for attempt := 0; attempt < logicalRetryCap; attempt++ {
					txID, err = r.Commit(ctx,
						tx.Body{Type: "push", Actor: "u_" + strconv.Itoa(w)},
						func(prev *repo.RootView) ([]byte, error) {
							var top map[string]json.RawMessage
							if err := json.Unmarshal(prev.Body, &top); err != nil {
								return nil, err
							}
							top[key] = json.RawMessage("true")
							_ = seq.Add(1)
							return json.Marshal(top)
						},
					)
					if err == nil {
						break
					}
					var gaveUp *repo.CommitGaveUpError
					if errors.As(err, &gaveUp) {
						// Bounded retry exhaustion under contention is
						// a valid outcome of Commit's contract; retry
						// the logical operation. The orphan tx records
						// from this attempt accumulate (M8 GC sweeps).
						continue
					}
					t.Errorf("Commit failed (non-retryable): %v", err)
					return
				}
				if err != nil {
					t.Errorf("Commit exhausted %d logical retries", logicalRetryCap)
					return
				}
				committedTx.Store(txID, true)
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
		t.Errorf("latest_tx %q not in committed set", view.Header.LatestTx)
	}

	// All commit-flagged keys must be present in body.
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
