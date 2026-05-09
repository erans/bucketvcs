//go:build stress

package sqlitestore

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// TestStress_VerifyMany seeds 100 tokens and runs 1,000 sequential
// VerifyCredential calls. Runs under 30s on a dev box.
// (100 verifies/sec expected with argon2id 64MiB memory; regression check)
func TestStress_VerifyMany(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	uid, _ := s.CreateUser(ctx, "alice", false)
	const N = 100
	tokens := make([]string, N)
	for i := 0; i < N; i++ {
		tok, id, secret, _ := auth.GenerateToken()
		hash, _ := auth.HashSecret(secret)
		if err := s.CreateToken(ctx, id, uid, hash, "", nil); err != nil {
			t.Fatalf("CreateToken %d: %v", i, err)
		}
		tokens[i] = tok
	}

	start := time.Now()
	for i := 0; i < 1_000; i++ {
		tok := tokens[i%N]
		if _, _, _, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "alice", Password: tok}); err != nil {
			t.Fatalf("Verify[%d]: %v", i, err)
		}
	}
	elapsed := time.Since(start)
	t.Logf("1k verifies / 100 tokens: %s", elapsed)
	if elapsed > 30*time.Second {
		t.Fatalf("1k verifies took %s (expected <30s; argon2 regression?)", elapsed)
	}
}

// TestStress_ConcurrentVerify sanity-checks WAL + busy_timeout under
// 100 concurrent verifies of distinct tokens.
func TestStress_ConcurrentVerify(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	uid, _ := s.CreateUser(ctx, "alice", false)
	const N = 100
	tokens := make([]string, N)
	for i := 0; i < N; i++ {
		tok, id, secret, _ := auth.GenerateToken()
		hash, _ := auth.HashSecret(secret)
		if err := s.CreateToken(ctx, id, uid, hash, "", nil); err != nil {
			t.Fatal(err)
		}
		tokens[i] = tok
	}

	var wg sync.WaitGroup
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(tok string) {
			defer wg.Done()
			if _, _, _, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "alice", Password: tok}); err != nil {
				errs <- err
			}
		}(tokens[i])
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent verify: %v", err)
	}
}
