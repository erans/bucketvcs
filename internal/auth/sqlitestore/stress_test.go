//go:build stress

package sqlitestore

import (
	"context"
	"fmt"
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
		if err := s.CreateToken(ctx, id, uid, hash, "", nil, auth.ScopeLegacy); err != nil {
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
		if err := s.CreateToken(ctx, id, uid, hash, "", nil, auth.ScopeLegacy); err != nil {
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

// TestStress_10kPublicKeyCallbacks seeds 1,000 SSH keys and runs 10,000
// sequential VerifyCredential calls cycling through them. Asserts that
// the index-backed lookup completes in under 5 seconds (regression guard
// against an n^2 full-table scan).
func TestStress_10kPublicKeyCallbacks(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, err := s.CreateUser(ctx, "alice", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Insert 1,000 keys.
	const numKeys = 1000
	fingerprints := make([]string, 0, numKeys)
	for i := 0; i < numKeys; i++ {
		fp := fmt.Sprintf("SHA256:stress-%04d", i)
		if err := s.AddSSHKey(ctx, auth.SSHKey{
			ID:          fmt.Sprintf("bvsk_stress_%04d", i),
			Fingerprint: fp,
			PublicKey:   []byte{0x01, 0x02},
			KeyType:     "ssh-ed25519",
			UserID:      uid,
		}); err != nil {
			t.Fatal(err)
		}
		fingerprints = append(fingerprints, fp)
	}

	// Run 10,000 sequential VerifyCredential calls picking round-robin fingerprints.
	const N = 10000
	start := time.Now()
	for i := 0; i < N; i++ {
		fp := fingerprints[i%numKeys]
		if _, _, _, err := s.VerifyCredential(ctx, auth.SSHKeyFingerprint{Fingerprint: fp}); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
	}
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Errorf("10,000 verifies took %v, want < 5s (suggests an n^2 scan)", elapsed)
	}
	t.Logf("10,000 verifies in %v (avg %v per call)", elapsed, elapsed/N)
}
