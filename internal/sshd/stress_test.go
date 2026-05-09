//go:build stress

package sshd

import (
	"runtime"
	"sync"
	"testing"
	"time"

	"crypto/ed25519"
	"crypto/rand"

	"golang.org/x/crypto/ssh"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// TestStress_200ParallelSSHClonesShareGateway exercises the SSH server
// under concurrent connection load. 200 distinct user keys, each with
// the same fixture repo grant, dial in parallel and request an exec.
//
// Asserts:
//   - all 200 connections complete (fewer than 1% failures)
//   - no goroutine leak (NumGoroutine delta < 10)
//   - VerifyCredential is invoked at least once per successful connection
func TestStress_200ParallelSSHClonesShareGateway(t *testing.T) {
	runtime.GC()
	baseGoroutines := runtime.NumGoroutine()

	var (
		mu          sync.Mutex
		verifyCalls int
	)
	store := &fakeStore{
		verify: func(c auth.Credential) (*auth.Actor, string, *auth.Scope, error) {
			mu.Lock()
			verifyCalls++
			mu.Unlock()
			return &auth.Actor{UserID: "u-stress", Name: "stress"}, "k-stress", nil, nil
		},
	}

	srv, _ := startTestServer(t, store)

	const N = 200
	var wg sync.WaitGroup
	errs := make(chan error, N)
	start := time.Now()
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each goroutine generates its own client key.
			_, priv, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				errs <- err
				return
			}
			signer, err := ssh.NewSignerFromKey(priv)
			if err != nil {
				errs <- err
				return
			}

			client, err := ssh.Dial("tcp", srv.Addr().String(), &ssh.ClientConfig{
				User:            "git",
				Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
				HostKeyCallback: ssh.InsecureIgnoreHostKey(),
				Timeout:         10 * time.Second,
			})
			if err != nil {
				errs <- err
				return
			}
			defer client.Close()

			sess, err := client.NewSession()
			if err != nil {
				errs <- err
				return
			}
			defer sess.Close()

			// Run a path that the engine will reject (ErrRepoNotFound)
			// — we just want the auth callback + session lifecycle.
			_ = sess.Run("git-upload-pack 'stress/repo.git'")
			// Either a clean exit or a non-zero exit; both fine.
		}()
	}
	wg.Wait()
	close(errs)
	elapsed := time.Since(start)

	var failedDials int
	for err := range errs {
		t.Logf("connection error: %v", err)
		failedDials++
	}
	if failedDials > N/100 {
		t.Fatalf("more than 1%% of dials failed: %d/%d", failedDials, N)
	}
	if verifyCalls+failedDials < N {
		t.Errorf("verifyCalls = %d, failedDials = %d, want sum >= N=%d",
			verifyCalls, failedDials, N)
	}

	// Allow goroutines a moment to drain.
	time.Sleep(200 * time.Millisecond)
	runtime.GC()
	leaked := runtime.NumGoroutine() - baseGoroutines
	if leaked > 10 {
		t.Errorf("goroutine leak: started %d, ended %d (delta %d)",
			baseGoroutines, runtime.NumGoroutine(), leaked)
	}

	t.Logf("200 parallel SSH connects in %v; verifyCalls=%d failedDials=%d goroutineDelta=%d",
		elapsed, verifyCalls, failedDials, leaked)
}
