package sshd

// Replica-aware SSH transport tests (M26): a read-only regional replica
// refuses git-receive-pack with the operator pointer message and gates
// git-upload-pack through Replica.Gate. Mirrors the HTTP gateway semantics
// (see internal/gateway/replica_test.go). These tests drive a real git/ssh
// binary, so they skip when those aren't on PATH.

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/replica"
)

// gateFunc adapts a plain func to the replica.Gate interface for tests.
type gateFunc func(ctx context.Context, tenant, repo string) error

func (f gateFunc) CheckAdvertise(ctx context.Context, tenant, repo string) error {
	return f(ctx, tenant, repo)
}

// TestReplicaRefusesSSHReceivePack: a replica refuses git-receive-pack with
// the read-only pointer message and a non-zero exit, before any negotiation.
func TestReplicaRefusesSSHReceivePack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SSH e2e uses bash scripts; not supported on Windows")
	}
	skipIfNoSSHGit(t)

	env := newSSHE2E(t, "acme", "web", func(o *Options) {
		o.Replica = &replica.GatewayConfig{WriteRegionURL: "https://gw-us.example"}
	})
	seedAliceWithKey(t, env, env.clientPubKey)
	if err := env.store.Grant(context.Background(), "alice", "acme", "web", "write"); err != nil {
		t.Fatalf("Grant write: %v", err)
	}

	// Raw git-receive-pack: the server refuses immediately (stderr + non-zero
	// exit) before reading stdin, so this returns without blocking.
	_, stderr, exitCode := sshExec(t, env, "git-receive-pack 'acme/web.git'")
	if exitCode == 0 {
		t.Fatalf("expected non-zero exit for receive-pack on replica; stderr=%q", stderr)
	}
	se := string(stderr)
	if !strings.Contains(se, "read-only replica") {
		t.Fatalf("stderr missing read-only refusal: %q", se)
	}
	if !strings.Contains(se, "https://gw-us.example") {
		t.Fatalf("stderr missing write-region URL: %q", se)
	}
}

// TestReplicaGateBlocksSSHUploadPack: when the gate reports the replica
// unhealthy, git-upload-pack is refused with the gate error on stderr and a
// non-zero exit, before negotiation begins.
func TestReplicaGateBlocksSSHUploadPack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SSH e2e uses bash scripts; not supported on Windows")
	}
	skipIfNoSSHGit(t)

	env := newSSHE2E(t, "acme", "web", func(o *Options) {
		o.Replica = &replica.GatewayConfig{
			WriteRegionURL: "https://gw-us.example",
			Gate: gateFunc(func(ctx context.Context, tenant, repo string) error {
				return &replica.UnhealthyError{Tenant: "t", Repo: "r", Reason: "lag budget exceeded"}
			}),
		}
	})
	seedAliceWithKey(t, env, env.clientPubKey)
	if err := env.store.Grant(context.Background(), "alice", "acme", "web", "read"); err != nil {
		t.Fatalf("Grant read: %v", err)
	}

	_, stderr, exitCode := sshExec(t, env, "git-upload-pack 'acme/web.git'")
	if exitCode == 0 {
		t.Fatalf("expected non-zero exit for gated upload-pack; stderr=%q", stderr)
	}
	if !strings.Contains(string(stderr), "replica unhealthy") {
		t.Fatalf("stderr missing unhealthy message: %q", string(stderr))
	}
}

// TestReplicaGateNilAllowsSSHUploadPack: Replica set but Gate nil → upload
// path is unaffected; ls-remote (which runs git-upload-pack) succeeds.
func TestReplicaGateNilAllowsSSHUploadPack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SSH e2e uses bash scripts; not supported on Windows")
	}
	skipIfNoSSHGit(t)

	env := newSSHE2E(t, "acme", "web", func(o *Options) {
		o.Replica = &replica.GatewayConfig{WriteRegionURL: "https://gw-us.example"} // Gate nil
	})
	seedAliceWithKey(t, env, env.clientPubKey)
	if err := env.store.Grant(context.Background(), "alice", "acme", "web", "read"); err != nil {
		t.Fatalf("Grant read: %v", err)
	}
	script := writeSSHCommandScript(t, env.clientKeyPath, env.knownHostsPath)

	out, err := gitWithSSH(t, script, "ls-remote", sshRemoteURL(env, "acme", "web"))
	if err != nil {
		t.Fatalf("ls-remote failed on replica with nil gate: %v\n%s", err, out)
	}
	if !containsAnySSH(string(out), "refs/heads/main", "HEAD") {
		t.Fatalf("ls-remote output missing refs/heads/main or HEAD:\n%s", out)
	}
}
