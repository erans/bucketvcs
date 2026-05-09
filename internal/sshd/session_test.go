package sshd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// startTestServer spins up an in-process Server on 127.0.0.1:0 and returns it.
// The fakeStore lets each test override verify behavior.
func startTestServer(t *testing.T, store auth.Store) (*Server, ssh.Signer) {
	t.Helper()
	opts := newTestServerOpts(t, store)
	s, err := NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		s.Close()
	})
	go s.Serve(ctx) //nolint:errcheck
	signer := mustGenerateClientKey(t)
	return s, signer
}

func mustGenerateClientKey(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func TestSession_RejectsShellRequest(t *testing.T) {
	store := &fakeStore{verify: func(c auth.Credential) (*auth.Actor, string, *auth.Scope, error) {
		return &auth.Actor{UserID: "u1", Name: "alice"}, "k1", nil, nil
	}}
	srv, signer := startTestServer(t, store)
	cfg := &ssh.ClientConfig{
		User:            "git",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	}
	client, err := ssh.Dial("tcp", srv.Addr().String(), cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer sess.Close()

	// Asking for a shell should fail (server rejects + non-zero exit).
	// Shell() may return an error immediately or defer it to Wait().
	shellErr := sess.Shell()
	if shellErr == nil {
		shellErr = sess.Wait()
	}
	if shellErr == nil {
		t.Fatal("expected non-zero exit for shell request")
	}
}

func TestSession_RejectsUnknownExec(t *testing.T) {
	store := &fakeStore{verify: func(c auth.Credential) (*auth.Actor, string, *auth.Scope, error) {
		return &auth.Actor{UserID: "u1", Name: "alice"}, "k1", nil, nil
	}}
	srv, signer := startTestServer(t, store)
	client, err := ssh.Dial("tcp", srv.Addr().String(), &ssh.ClientConfig{
		User:            "git",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	sess.Stderr = &stderr
	err = sess.Run("rm -rf /")
	if err == nil {
		t.Fatal("expected exit error")
	}
	if !bytes.Contains(stderr.Bytes(), []byte("command not allowed")) {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestSession_NonGitUserRejected(t *testing.T) {
	store := &fakeStore{verify: func(c auth.Credential) (*auth.Actor, string, *auth.Scope, error) {
		return &auth.Actor{UserID: "u1"}, "k1", nil, nil
	}}
	srv, signer := startTestServer(t, store)
	cfg := &ssh.ClientConfig{
		User:            "alice", // wrong username
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	}
	if _, err := ssh.Dial("tcp", srv.Addr().String(), cfg); err == nil {
		t.Fatal("expected handshake rejection for non-git user")
	}
}

func TestSession_RevokedKeyRejected(t *testing.T) {
	store := &fakeStore{verify: func(c auth.Credential) (*auth.Actor, string, *auth.Scope, error) {
		return nil, "", nil, auth.ErrTokenRevoked
	}}
	srv, signer := startTestServer(t, store)
	cfg := &ssh.ClientConfig{
		User:            "git",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	}
	if _, err := ssh.Dial("tcp", srv.Addr().String(), cfg); err == nil {
		t.Fatal("expected handshake rejection for revoked key")
	}
}

func TestSession_GracefulShutdown(t *testing.T) {
	store := &fakeStore{verify: func(c auth.Credential) (*auth.Actor, string, *auth.Scope, error) {
		return &auth.Actor{UserID: "u1"}, "k1", nil, nil
	}}
	// Don't use startTestServer here since we want manual control over Close.
	opts := newTestServerOpts(t, store)
	srv, err := NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	addr := srv.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx) //nolint:errcheck

	// Close the server.
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}

	// After Close, a new dial should fail (listener closed).
	_, err = dialTestTimeout(addr, 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected dial to fail after Close")
	}
	_ = errors.New
}

func dialTestTimeout(addr string, d time.Duration) (interface{}, error) {
	c, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            "git",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         d,
	})
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c, nil
}
