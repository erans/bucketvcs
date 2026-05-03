package conformance

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// runStress is the entry point for the §29 stress tests applicable to
// localfs in M0: 100 concurrent CAS attempts, 10,000 small object
// creates, large multipart pack upload conflict.
func runStress(t *testing.T, f Factory) {
	t.Helper()
	t.Run("Stress100ConcurrentCAS", func(t *testing.T) { stress100ConcurrentCAS(t, f) })
	t.Run("Stress10kCreates", func(t *testing.T) { stress10kCreates(t, f) })
	t.Run("StressLargeMultipartConflict", func(t *testing.T) { stressLargeMultipartConflict(t, f) })
}

// §29 stress: 100 concurrent manifest CAS attempts.
//
// Models the manifest commit hot path. Many goroutines all read the
// current version then race to PutIfVersionMatches. Exactly one wins
// per round; the rest retry with the new version. After many rounds,
// every winner's CAS must have produced a strictly later version, and
// the final state matches the last winner.
func stress100ConcurrentCAS(t *testing.T, f Factory) {
	s := newStore(t, f)
	const writers = 100
	const rounds = 5
	const key = "stress/cas"

	v0, err := s.PutIfAbsent(ctx(), key, bytes.NewReader([]byte("init")), nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	current := v0

	for r := 0; r < rounds; r++ {
		var wg sync.WaitGroup
		results := make(chan struct {
			v   storage.ObjectVersion
			err error
		}, writers)
		expected := current
		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				body := bytes.NewReader([]byte(fmt.Sprintf("round-%d-writer-%d", r, i)))
				v, err := s.PutIfVersionMatches(ctx(), key, expected, body, nil)
				results <- struct {
					v   storage.ObjectVersion
					err error
				}{v, err}
			}(i)
		}
		wg.Wait()
		close(results)

		successes, conflicts := 0, 0
		var winner storage.ObjectVersion
		for r := range results {
			if r.err == nil {
				successes++
				winner = r.v
			} else if errors.Is(r.err, storage.ErrVersionMismatch) {
				conflicts++
			} else {
				t.Errorf("unexpected error: %v", r.err)
			}
		}
		if successes != 1 {
			t.Fatalf("round %d: successes = %d, want 1", r, successes)
		}
		if conflicts != writers-1 {
			t.Fatalf("round %d: conflicts = %d, want %d", r, conflicts, writers-1)
		}
		current = winner
	}

	md, err := s.Head(ctx(), key)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if md.Version != current {
		t.Errorf("final Version = %+v, want %+v", md.Version, current)
	}
}

// §29 stress: 10,000 small object creates.
//
// Exercises listing pagination, keyed-mutex map growth, sidecar I/O
// rate. Each object is 16 bytes. After all creates, list every object
// across many pages and assert the count.
func stress10kCreates(t *testing.T, f Factory) {
	s := newStore(t, f)
	const total = 10_000
	const concurrency = 32

	work := make(chan int, concurrency)
	errs := make(chan error, total)
	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				body := DeterministicBytes(16, fmt.Sprintf("seed-%d", i))
				if _, err := s.PutIfAbsent(ctx(), Key("stress/10k", i), bytes.NewReader(body), nil); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	for i := 0; i < total; i++ {
		work <- i
	}
	close(work)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("PutIfAbsent: %v", err)
	}

	got := 0
	token := ""
	for iter := 0; iter < 10_000; iter++ {
		page, err := s.List(ctx(), "stress/10k/", &storage.ListOptions{MaxKeys: 1024, ContinuationToken: token})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		got += len(page.Objects)
		if page.NextToken == "" {
			break
		}
		token = page.NextToken
	}
	if got != total {
		t.Errorf("listed %d objects, want %d", got, total)
	}
}

// §29 stress: large multipart pack upload conflict.
//
// Pre-creates the target key, then runs a 256 MiB multipart upload
// targeting same key. Asserts CompleteMultipartIfAbsent returns
// ErrAlreadyExists and the original content + version are unchanged.
func stressLargeMultipartConflict(t *testing.T, f Factory) {
	s := newStore(t, f)
	const partSize = 32 * 1024 * 1024 // 32 MiB
	const numParts = 8                // 256 MiB total
	const key = "stress/large-multi"

	v0, err := s.PutIfAbsent(ctx(), key, bytes.NewReader([]byte("original-pack")), nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	mp, err := s.CreateMultipart(ctx(), key, nil)
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	parts := make([]storage.MultipartPart, 0, numParts)
	for i := 0; i < numParts; i++ {
		chunk := DeterministicBytes(partSize, fmt.Sprintf("large-%d", i))
		p, err := mp.UploadPart(ctx(), i+1, bytes.NewReader(chunk))
		if err != nil {
			t.Fatalf("UploadPart %d: %v", i+1, err)
		}
		parts = append(parts, p)
	}

	if _, err := s.CompleteMultipartIfAbsent(ctx(), mp, parts); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Errorf("Complete on existing key = %v, want ErrAlreadyExists", err)
	}

	md, err := s.Head(ctx(), key)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if md.Version != v0 {
		t.Errorf("version mutated by failed Complete: got %+v, want %+v", md.Version, v0)
	}
	if md.Size != int64(len("original-pack")) {
		t.Errorf("size mutated by failed Complete: got %d, want %d", md.Size, len("original-pack"))
	}
}
