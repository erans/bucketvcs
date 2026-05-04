//go:build stress

package repointernal_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	tx "github.com/bucketvcs/bucketvcs/internal/repo/tx"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestCommit_Stress(t *testing.T) {
	const (
		writers          = 100
		commitsPerWriter = 1000
	)
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	r, err := repo.Create(ctx, store, "acme", "stress", repo.CreateOptions{Actor: "u"})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < commitsPerWriter; i++ {
				key := "k_" + strconv.Itoa(w*commitsPerWriter+i)
				const logicalRetryCap = 200
				var (
					cerr error
				)
				for attempt := 0; attempt < logicalRetryCap; attempt++ {
					_, cerr = r.Commit(ctx,
						tx.Body{Type: "push", Actor: "u_" + strconv.Itoa(w)},
						func(prev *repo.RootView) ([]byte, error) {
							var top map[string]json.RawMessage
							if err := json.Unmarshal(prev.Body, &top); err != nil {
								return nil, err
							}
							top[key] = json.RawMessage("true")
							return json.Marshal(top)
						},
						repo.WithCommitPolicy(repo.CommitPolicy{MaxRetries: 32, BackoffBase: 0}),
					)
					if cerr == nil {
						break
					}
					var gaveUp *repo.CommitGaveUpError
					if errors.As(cerr, &gaveUp) {
						continue
					}
					t.Errorf("non-retryable commit error: %v", cerr)
					return
				}
				if cerr != nil {
					t.Errorf("commit exhausted %d logical retries", logicalRetryCap)
					return
				}
			}
		}()
	}
	wg.Wait()

	v, err := r.ReadRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := uint64(1 + writers*commitsPerWriter); v.Header.ManifestVersion != want {
		t.Errorf("manifest_version: want %d, got %d", want, v.Header.ManifestVersion)
	}
}
