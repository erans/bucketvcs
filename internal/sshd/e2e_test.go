package sshd

// SSH end-to-end tests (M6 Task 26): drive a real `git` binary against the
// SSH gateway and assert protocol-level outcomes for each combination of (key
// state, repo perms, requested action).
//
// Each scenario:
//  1. Builds a localfs object store and seeds it with a tiny bare repo via
//     the importer (so the bucket has at least one commit + ref to clone).
//  2. Builds a sqlitestore.Store, registers the repo, creates alice with an
//     ed25519 client keypair, and grants the appropriate permissions.
//  3. Starts an sshd.Server on 127.0.0.1:0.
//  4. Writes a GIT_SSH_COMMAND script that passes the correct private key
//     and a known_hosts entry pinning the server host key.
//  5. Drives `git clone` / `git push` / `git ls-remote` via exec.CommandContext
//     and asserts on exit status + stderr content.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/importer"
	"github.com/bucketvcs/bucketvcs/internal/lfs"
	"github.com/bucketvcs/bucketvcs/internal/mirror"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// skipIfNoSSHGit skips the test if `git` or `ssh` is not on PATH.
func skipIfNoSSHGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh binary not available")
	}
}

// sshE2EEnv bundles everything an SSH e2e test needs.
type sshE2EEnv struct {
	srv            *Server
	store          *sqlitestore.Store
	tenant         string
	repo           string
	sshAddr        string // "127.0.0.1:<port>"
	knownHostsPath string // path to the known_hosts file pinning server host key
	clientKeyPath  string // path to alice's private key file on disk
	clientPubKey   ssh.PublicKey
	mirrorMgr      *mirror.Manager
}

// sshRemoteURL returns the SSH remote URL for git.
func sshRemoteURL(env *sshE2EEnv, tenant, repo string) string {
	return "ssh://git@" + env.sshAddr + "/" + tenant + "/" + repo + ".git"
}

// writeSSHCommandScript writes a tiny shell script that invokes ssh with the
// given private key and known_hosts file. Returns the path to the script.
// The script is mode 0755 so the shell can exec it.
func writeSSHCommandScript(t *testing.T, clientPrivKeyPath, knownHostsPath string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "git-ssh.sh")
	body := fmt.Sprintf(`#!/usr/bin/env bash
exec ssh \
  -i %s \
  -o UserKnownHostsFile=%s \
  -o StrictHostKeyChecking=yes \
  -o GlobalKnownHostsFile=/dev/null \
  -o IdentitiesOnly=yes \
  "$@"
`,
		shellQ(clientPrivKeyPath),
		shellQ(knownHostsPath),
	)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write ssh script: %v", err)
	}
	return script
}

// shellQ single-quotes a path for safe use in shell scripts.
func shellQ(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// gitWithSSH runs git with GIT_SSH_COMMAND set to the given script, plus
// a hermetic home directory so the developer's ~/.ssh/config doesn't interfere.
// Returns combined stdout+stderr and the exit error.
func gitWithSSH(t *testing.T, sshScript string, args ...string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(),
		"GIT_SSH_COMMAND="+sshScript,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"HOME="+t.TempDir(),
	)
	return cmd.CombinedOutput()
}

// mustExecSSH runs a command and fatals on failure. Shared with the gateway
// pattern helper; we inline it here to avoid cross-package coupling.
func mustExecSSH(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}

// makeRepoForSSH seeds a tiny one-commit repo into storeDir. It mirrors the
// pattern from internal/gateway/inforefs_test.go makeRepoInStore.
func makeRepoForSSH(t *testing.T, storeDir, tenant, repoID string) {
	t.Helper()
	srcBare := filepath.Join(t.TempDir(), "src.git")
	work := filepath.Join(t.TempDir(), "wt")

	mustExecSSH(t, "", "git", "init", "--bare", srcBare)
	mustExecSSH(t, "", "git", "clone", srcBare, work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustExecSSH(t, work, "git", "add", ".")
	mustExecSSH(t, work, "git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "init")
	mustExecSSH(t, work, "git", "push", "origin", "HEAD:refs/heads/main")

	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	defer store.Close()
	if _, err := importer.Import(context.Background(), store, importer.Options{
		Tenant: tenant, Repo: repoID, SourceDir: srcBare, DefaultBranch: "refs/heads/main",
	}); err != nil {
		t.Fatalf("Import: %v", err)
	}
}

// makeWorkRepoSSH builds a fresh local git repo with one commit. Used by push scenarios.
func makeWorkRepoSSH(t *testing.T) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "wt")
	mustExecSSH(t, "", "git", "init", work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustExecSSH(t, work, "git", "add", ".")
	mustExecSSH(t, work, "git", "-c", "user.email=t@t", "-c", "user.name=t",
		"commit", "-m", "init")
	return work
}

// newSSHE2E spins up a full SSH environment: object store, auth store, SSH
// server, and a generated client keypair for alice. It also seeds a one-commit
// fixture repo identified by (tenant, repoID). The caller can further mutate
// the auth store (grant permissions, add keys, etc.) before driving git.
//
// The env is fully cleaned up via t.Cleanup.
func newSSHE2E(t *testing.T, tenant, repoID string) *sshE2EEnv {
	t.Helper()

	// ---- object store + fixture repo ----------------------------------------
	storeDir := t.TempDir()
	makeRepoForSSH(t, storeDir, tenant, repoID)
	objStore, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = objStore.Close() })

	// ---- auth store ----------------------------------------------------------
	authStore, err := sqlitestore.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("sqlitestore.Open: %v", err)
	}
	t.Cleanup(func() { _ = authStore.Close() })
	if err := authStore.RegisterRepo(context.Background(), tenant, repoID); err != nil {
		t.Fatalf("RegisterRepo: %v", err)
	}

	// ---- mirror manager ------------------------------------------------------
	mirrorDir := t.TempDir()
	mgr, err := mirror.NewManager(mirrorDir, objStore)
	if err != nil {
		t.Fatalf("mirror.NewManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	// ---- host key ------------------------------------------------------------
	hostKeyPath := filepath.Join(t.TempDir(), "host_key")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// ---- SSH server ----------------------------------------------------------
	srv, err := NewServer(Options{
		Addr:         "127.0.0.1:0",
		HostKeyPath:  hostKeyPath,
		Grace:        0,
		Store:        authStore,
		BVStore:      objStore,
		Mirror:       mgr,
		Logger:       logger,
		AgentVersion: "e2e-test",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("srv.Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = srv.Close()
	})
	go srv.Serve(ctx) //nolint:errcheck

	// ---- known_hosts pinning -------------------------------------------------
	// Re-load the host key from disk to get the public key wire bytes.
	hostPriv, err := loadPrivKeyForKnownHosts(hostKeyPath)
	if err != nil {
		t.Fatalf("load host key for known_hosts: %v", err)
	}
	sshAddr := srv.Addr().(*net.TCPAddr)
	hostEntry := buildKnownHostsEntry(sshAddr, hostPriv.PublicKey())

	khPath := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(khPath, []byte(hostEntry+"\n"), 0o600); err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}

	// ---- client keypair for alice --------------------------------------------
	_, clientPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	clientSigner, err := ssh.NewSignerFromKey(clientPriv)
	if err != nil {
		t.Fatalf("NewSignerFromKey: %v", err)
	}
	pemBlock, err := ssh.MarshalPrivateKey(clientPriv, "")
	if err != nil {
		t.Fatalf("MarshalPrivateKey: %v", err)
	}
	clientKeyPath := filepath.Join(t.TempDir(), "alice_key")
	if err := os.WriteFile(clientKeyPath, pem.EncodeToMemory(pemBlock), 0o600); err != nil {
		t.Fatalf("write client key: %v", err)
	}

	env := &sshE2EEnv{
		srv:            srv,
		store:          authStore,
		tenant:         tenant,
		repo:           repoID,
		sshAddr:        sshAddr.String(),
		knownHostsPath: khPath,
		clientKeyPath:  clientKeyPath,
		clientPubKey:   clientSigner.PublicKey(),
		mirrorMgr:      mgr,
	}
	return env
}

// seedAliceWithKey creates user "alice", inserts the given public key as her
// SSH user key, and returns her userID and keyID.
func seedAliceWithKey(t *testing.T, env *sshE2EEnv, pub ssh.PublicKey) (userID, keyID string) {
	t.Helper()
	ctx := context.Background()
	uid, err := env.store.CreateUser(ctx, "alice", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	keyID, err = auth.GenerateSSHKeyID()
	if err != nil {
		t.Fatalf("GenerateSSHKeyID: %v", err)
	}
	fp := SHA256Fingerprint(pub)
	if err := env.store.AddSSHKey(ctx, auth.SSHKey{
		ID:          keyID,
		Fingerprint: fp,
		PublicKey:   pub.Marshal(),
		KeyType:     pub.Type(),
		Label:       "alice-test",
		UserID:      uid,
	}); err != nil {
		t.Fatalf("AddSSHKey: %v", err)
	}
	return uid, keyID
}

// seedDeployKey inserts the given public key as a deploy key scoped to
// (tenant, repo, perm). Deploy keys have no associated user — their
// actor.UserID is a "deploy:" sentinel, which handleLFSAuthenticate
// must reject.
func seedDeployKey(t *testing.T, env *sshE2EEnv, pub ssh.PublicKey, tenant, repo string, perm auth.Perm) string {
	t.Helper()
	ctx := context.Background()
	keyID, err := auth.GenerateSSHKeyID()
	if err != nil {
		t.Fatalf("GenerateSSHKeyID: %v", err)
	}
	fp := SHA256Fingerprint(pub)
	if err := env.store.AddSSHKey(ctx, auth.SSHKey{
		ID:          keyID,
		Fingerprint: fp,
		PublicKey:   pub.Marshal(),
		KeyType:     pub.Type(),
		Label:       "deploy-test",
		ScopeTenant: tenant,
		ScopeRepo:   repo,
		ScopePerm:   perm,
	}); err != nil {
		t.Fatalf("AddSSHKey (deploy): %v", err)
	}
	return keyID
}

// loadPrivKeyForKnownHosts reads the PEM-encoded private key at path and
// returns the signer. Used to extract the host public key for known_hosts.
func loadPrivKeyForKnownHosts(path string) (ssh.Signer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(raw)
}

// buildKnownHostsEntry builds a known_hosts line for a TCP address and public key.
// Format: [host]:port <keytype> <base64>
func buildKnownHostsEntry(addr *net.TCPAddr, pub ssh.PublicKey) string {
	hostStr := fmt.Sprintf("[%s]:%d", addr.IP.String(), addr.Port)
	// MarshalAuthorizedKey returns "<keytype> <base64>\n"; we just need the
	// key material (without trailing newline) for the known_hosts format.
	authorizedLine := strings.TrimRight(string(ssh.MarshalAuthorizedKey(pub)), "\n")
	return hostStr + " " + authorizedLine
}

// containsAnySSH reports whether haystack (lower-cased) contains any of the
// given needles. Used to match git's varying error wording across versions.
func containsAnySSH(haystack string, needles ...string) bool {
	low := strings.ToLower(haystack)
	for _, n := range needles {
		if strings.Contains(low, strings.ToLower(n)) {
			return true
		}
	}
	return false
}

// expectSSHGitFails asserts git command failed and output mentions one of errTokens.
func expectSSHGitFails(t *testing.T, out []byte, err error, errTokens ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected git to fail, but it succeeded. output:\n%s", out)
	}
	if len(errTokens) > 0 && !containsAnySSH(string(out), errTokens...) {
		t.Fatalf("git failed but output didn't match any of %v.\nerr=%v\noutput:\n%s",
			errTokens, err, out)
	}
}

// addDeployKey adds a deploy key bound to a specific repo with the given perm.
// Returns the key ID and the path to the private key file.
func addDeployKey(t *testing.T, env *sshE2EEnv, label, tenant, repo string, perm auth.Perm) (keyID, privKeyPath string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey for deploy key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("NewSignerFromKey: %v", err)
	}
	pemBlock, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("MarshalPrivateKey: %v", err)
	}
	keyPath := filepath.Join(t.TempDir(), label+"_key")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(pemBlock), 0o600); err != nil {
		t.Fatalf("write deploy key: %v", err)
	}
	keyID, err = auth.GenerateSSHKeyID()
	if err != nil {
		t.Fatalf("GenerateSSHKeyID: %v", err)
	}
	fp := SHA256Fingerprint(signer.PublicKey())
	if err := env.store.AddSSHKey(context.Background(), auth.SSHKey{
		ID:          keyID,
		Fingerprint: fp,
		PublicKey:   signer.PublicKey().Marshal(),
		KeyType:     signer.PublicKey().Type(),
		Label:       label,
		ScopeTenant: tenant,
		ScopeRepo:   repo,
		ScopePerm:   perm,
	}); err != nil {
		t.Fatalf("AddSSHKey (deploy key %s): %v", label, err)
	}
	return keyID, keyPath
}

// -----------------------------------------------------------------------------
// Main test entry point
// -----------------------------------------------------------------------------

func TestE2E_SSHEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SSH e2e uses bash scripts; not supported on Windows")
	}
	skipIfNoSSHGit(t)

	// ---- scenario 1: clone with valid user key → succeeds --------------------
	t.Run("CloneValidUserKey_Succeeds", func(t *testing.T) {
		env := newSSHE2E(t, "acme", "web")
		uid, _ := seedAliceWithKey(t, env, env.clientPubKey)
		_ = uid
		if err := env.store.Grant(context.Background(), "alice", "acme", "web", "read"); err != nil {
			t.Fatalf("Grant: %v", err)
		}
		script := writeSSHCommandScript(t, env.clientKeyPath, env.knownHostsPath)

		workdir := t.TempDir()
		out, err := gitWithSSH(t, script, "-C", workdir, "clone", sshRemoteURL(env, "acme", "web"), "clone1")
		if err != nil {
			t.Fatalf("clone failed: %v\n%s", err, out)
		}
		if _, err := os.Stat(filepath.Join(workdir, "clone1", ".git", "HEAD")); err != nil {
			t.Fatalf("expected clone1/.git/HEAD to exist: %v", err)
		}
	})

	// ---- scenario 2: clone with revoked key → fails --------------------------
	t.Run("CloneRevokedKey_Fails", func(t *testing.T) {
		env := newSSHE2E(t, "acme", "web")
		_, keyID := seedAliceWithKey(t, env, env.clientPubKey)
		if err := env.store.Grant(context.Background(), "alice", "acme", "web", "read"); err != nil {
			t.Fatalf("Grant: %v", err)
		}
		if err := env.store.RevokeSSHKey(context.Background(), keyID); err != nil {
			t.Fatalf("RevokeSSHKey: %v", err)
		}
		script := writeSSHCommandScript(t, env.clientKeyPath, env.knownHostsPath)

		workdir := t.TempDir()
		out, err := gitWithSSH(t, script, "-C", workdir, "clone", sshRemoteURL(env, "acme", "web"), "clone2")
		expectSSHGitFails(t, out, err, "permission denied", "authentication", "fatal")
	})

	// ---- scenario 3: clone with disabled-user key → fails --------------------
	t.Run("CloneDisabledUser_Fails", func(t *testing.T) {
		env := newSSHE2E(t, "acme", "web")
		uid, _ := seedAliceWithKey(t, env, env.clientPubKey)
		if err := env.store.Grant(context.Background(), "alice", "acme", "web", "read"); err != nil {
			t.Fatalf("Grant: %v", err)
		}
		// Disable alice by ID (avoids last-admin guard, since we only have alice).
		if err := env.store.DisableUserByID(context.Background(), uid); err != nil {
			t.Fatalf("DisableUserByID: %v", err)
		}
		script := writeSSHCommandScript(t, env.clientKeyPath, env.knownHostsPath)

		workdir := t.TempDir()
		out, err := gitWithSSH(t, script, "-C", workdir, "clone", sshRemoteURL(env, "acme", "web"), "clone3")
		expectSSHGitFails(t, out, err, "permission denied", "authentication", "fatal")
	})

	// ---- scenario 4: push with read-only deploy key → fails ------------------
	t.Run("PushReadOnlyDeployKey_Fails", func(t *testing.T) {
		env := newSSHE2E(t, "acme", "web")
		_, deployKeyPath := addDeployKey(t, env, "ci-read", "acme", "web", auth.PermRead)

		script := writeSSHCommandScript(t, deployKeyPath, env.knownHostsPath)
		work := makeWorkRepoSSH(t)

		// Make an additional commit to push.
		if err := os.WriteFile(filepath.Join(work, "b.txt"), []byte("more\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		mustExecSSH(t, work, "git", "add", ".")
		mustExecSSH(t, work, "git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "more")

		out, err := gitWithSSH(t, script, "-C", work, "push", sshRemoteURL(env, "acme", "web"), "HEAD:refs/heads/topic")
		expectSSHGitFails(t, out, err, "insufficient", "permission denied", "fatal")
	})

	// ---- scenario 5: push with write deploy key → succeeds -------------------
	t.Run("PushWriteDeployKey_Succeeds", func(t *testing.T) {
		env := newSSHE2E(t, "acme", "web")
		_, deployKeyPath := addDeployKey(t, env, "ci-write", "acme", "web", auth.PermWrite)

		script := writeSSHCommandScript(t, deployKeyPath, env.knownHostsPath)
		work := makeWorkRepoSSH(t)

		if err := os.WriteFile(filepath.Join(work, "b.txt"), []byte("more\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		mustExecSSH(t, work, "git", "add", ".")
		mustExecSSH(t, work, "git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "more")

		out, err := gitWithSSH(t, script, "-C", work, "push", sshRemoteURL(env, "acme", "web"), "HEAD:refs/heads/topic")
		if err != nil {
			t.Fatalf("push with write deploy key failed: %v\n%s", err, out)
		}
	})

	// ---- scenario 6: deploy key cross-repo → fails ---------------------------
	t.Run("DeployKeyCrossRepo_Fails", func(t *testing.T) {
		env := newSSHE2E(t, "acme", "web")

		// Register a second repo ("other") that the key is NOT bound to.
		makeRepoForSSH(t, t.TempDir(), "acme", "other")
		if err := env.store.RegisterRepo(context.Background(), "acme", "other"); err != nil {
			t.Fatalf("RegisterRepo other: %v", err)
		}

		// Create a deploy key scoped to acme/web.
		_, deployKeyPath := addDeployKey(t, env, "ci-web", "acme", "web", auth.PermRead)
		script := writeSSHCommandScript(t, deployKeyPath, env.knownHostsPath)

		// Attempt to clone acme/other with the acme/web deploy key.
		workdir := t.TempDir()
		out, err := gitWithSSH(t, script, "-C", workdir, "clone", sshRemoteURL(env, "acme", "other"), "clone6")
		expectSSHGitFails(t, out, err, "not authorized", "key not authorized", "fatal")
	})

	// ---- scenario 7: git ls-remote over SSH succeeds -------------------------
	t.Run("LsRemote_Succeeds", func(t *testing.T) {
		env := newSSHE2E(t, "acme", "web")
		seedAliceWithKey(t, env, env.clientPubKey)
		if err := env.store.Grant(context.Background(), "alice", "acme", "web", "read"); err != nil {
			t.Fatalf("Grant: %v", err)
		}
		script := writeSSHCommandScript(t, env.clientKeyPath, env.knownHostsPath)

		out, err := gitWithSSH(t, script, "ls-remote", sshRemoteURL(env, "acme", "web"))
		if err != nil {
			t.Fatalf("ls-remote failed: %v\n%s", err, out)
		}
		if !containsAnySSH(string(out), "refs/heads/main", "HEAD") {
			t.Fatalf("ls-remote output missing refs/heads/main or HEAD:\n%s", out)
		}
	})

	// ---- scenario 8: push with write user key → succeeds --------------------
	t.Run("PushWriteUserKey_Succeeds", func(t *testing.T) {
		env := newSSHE2E(t, "acme", "web")
		seedAliceWithKey(t, env, env.clientPubKey)
		if err := env.store.Grant(context.Background(), "alice", "acme", "web", "write"); err != nil {
			t.Fatalf("Grant write: %v", err)
		}
		script := writeSSHCommandScript(t, env.clientKeyPath, env.knownHostsPath)
		work := makeWorkRepoSSH(t)

		if err := os.WriteFile(filepath.Join(work, "b.txt"), []byte("more\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		mustExecSSH(t, work, "git", "add", ".")
		mustExecSSH(t, work, "git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "more")

		out, err := gitWithSSH(t, script, "-C", work, "push", sshRemoteURL(env, "acme", "web"), "HEAD:refs/heads/topic")
		if err != nil {
			t.Fatalf("push with write user key failed: %v\n%s", err, out)
		}
	})

	// ---- scenario 9: annotated tag push succeeds ----------------------------
	t.Run("AnnotatedTagPush_Succeeds", func(t *testing.T) {
		env := newSSHE2E(t, "acme", "web")
		seedAliceWithKey(t, env, env.clientPubKey)
		if err := env.store.Grant(context.Background(), "alice", "acme", "web", "write"); err != nil {
			t.Fatalf("Grant write: %v", err)
		}
		script := writeSSHCommandScript(t, env.clientKeyPath, env.knownHostsPath)

		// Clone first so we have something to tag.
		workdir := t.TempDir()
		out, err := gitWithSSH(t, script, "-C", workdir, "clone", sshRemoteURL(env, "acme", "web"), "tagtest")
		if err != nil {
			t.Fatalf("clone for tag test failed: %v\n%s", err, out)
		}
		repoDir := filepath.Join(workdir, "tagtest")
		mustExecSSH(t, repoDir, "git", "-c", "user.email=t@t", "-c", "user.name=t",
			"tag", "-a", "v1.0.0", "-m", "release 1.0.0")

		out, err = gitWithSSH(t, script, "-C", repoDir, "push", "origin", "v1.0.0")
		if err != nil {
			t.Fatalf("annotated tag push failed: %v\n%s", err, out)
		}

		// Verify the tag appears in ls-remote.
		out, err = gitWithSSH(t, script, "ls-remote", sshRemoteURL(env, "acme", "web"))
		if err != nil {
			t.Fatalf("ls-remote after tag push: %v\n%s", err, out)
		}
		if !containsAnySSH(string(out), "refs/tags/v1.0.0") {
			t.Fatalf("ls-remote missing refs/tags/v1.0.0 after push:\n%s", out)
		}
	})

	// ---- scenario 10: force-push with write perm → succeeds -----------------
	t.Run("ForcePush_Succeeds", func(t *testing.T) {
		env := newSSHE2E(t, "acme", "web")
		seedAliceWithKey(t, env, env.clientPubKey)
		if err := env.store.Grant(context.Background(), "alice", "acme", "web", "write"); err != nil {
			t.Fatalf("Grant write: %v", err)
		}
		script := writeSSHCommandScript(t, env.clientKeyPath, env.knownHostsPath)

		// Clone the seeded repo.
		workdir := t.TempDir()
		out, err := gitWithSSH(t, script, "-C", workdir, "clone", sshRemoteURL(env, "acme", "web"), "fptest")
		if err != nil {
			t.Fatalf("clone for force-push test failed: %v\n%s", err, out)
		}
		repoDir := filepath.Join(workdir, "fptest")

		// Make a commit on main, push it normally first.
		if err := os.WriteFile(filepath.Join(repoDir, "b.txt"), []byte("second\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		mustExecSSH(t, repoDir, "git", "add", ".")
		mustExecSSH(t, repoDir, "git", "-c", "user.email=t@t", "-c", "user.name=t",
			"commit", "-m", "second")
		out, err = gitWithSSH(t, script, "-C", repoDir, "push", "origin", "HEAD:refs/heads/main")
		if err != nil {
			t.Fatalf("first push failed: %v\n%s", err, out)
		}

		// Make an amend-style commit (rewrite HEAD) and force-push.
		mustExecSSH(t, repoDir, "git", "reset", "--soft", "HEAD^")
		if err := os.WriteFile(filepath.Join(repoDir, "b.txt"), []byte("second-amended\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		mustExecSSH(t, repoDir, "git", "add", ".")
		mustExecSSH(t, repoDir, "git", "-c", "user.email=t@t", "-c", "user.name=t",
			"commit", "-m", "second-amended")

		out, err = gitWithSSH(t, script, "-C", repoDir, "push", "--force", "origin", "HEAD:refs/heads/main")
		if err != nil {
			t.Fatalf("force-push failed: %v\n%s", err, out)
		}
	})
}

// skipIfNoOpenSSH skips a test when no `ssh` client is on PATH. Used by
// LFS git-lfs-authenticate tests, which invoke ssh directly instead of
// going through `git`.
func skipIfNoOpenSSH(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh binary not available")
	}
}

// newSSHE2ELFS mirrors newSSHE2E but also wires the LFS SSH-authenticate
// Options fields (TokenIssuer + BaseURL + TTL). The baseURL is embedded
// in the issued response's Href; the test does not actually HTTP-fetch
// it, so any well-formed URL works.
//
// Implemented as a sibling rather than extending newSSHE2E so the 30+
// existing newSSHE2E call sites stay untouched.
func newSSHE2ELFS(t *testing.T, tenant, repoID, baseURL string) *sshE2EEnv {
	t.Helper()

	storeDir := t.TempDir()
	makeRepoForSSH(t, storeDir, tenant, repoID)
	objStore, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = objStore.Close() })

	authStore, err := sqlitestore.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("sqlitestore.Open: %v", err)
	}
	t.Cleanup(func() { _ = authStore.Close() })
	if err := authStore.RegisterRepo(context.Background(), tenant, repoID); err != nil {
		t.Fatalf("RegisterRepo: %v", err)
	}

	mirrorDir := t.TempDir()
	mgr, err := mirror.NewManager(mirrorDir, objStore)
	if err != nil {
		t.Fatalf("mirror.NewManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	hostKeyPath := filepath.Join(t.TempDir(), "host_key")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	srv, err := NewServer(Options{
		Addr:           "127.0.0.1:0",
		HostKeyPath:    hostKeyPath,
		Grace:          0,
		Store:          authStore,
		BVStore:        objStore,
		Mirror:         mgr,
		Logger:         logger,
		AgentVersion:   "e2e-test",
		LFSTokenIssuer: authStore, // sqlitestore.Store satisfies lfs.TokenIssuer
		LFSBaseURL:     baseURL,
		LFSSSHTokenTTL: 15 * time.Minute,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("srv.Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = srv.Close()
	})
	go srv.Serve(ctx) //nolint:errcheck

	hostPriv, err := loadPrivKeyForKnownHosts(hostKeyPath)
	if err != nil {
		t.Fatalf("load host key for known_hosts: %v", err)
	}
	sshAddr := srv.Addr().(*net.TCPAddr)
	hostEntry := buildKnownHostsEntry(sshAddr, hostPriv.PublicKey())

	khPath := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(khPath, []byte(hostEntry+"\n"), 0o600); err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}

	_, clientPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	clientSigner, err := ssh.NewSignerFromKey(clientPriv)
	if err != nil {
		t.Fatalf("NewSignerFromKey: %v", err)
	}
	pemBlock, err := ssh.MarshalPrivateKey(clientPriv, "")
	if err != nil {
		t.Fatalf("MarshalPrivateKey: %v", err)
	}
	clientKeyPath := filepath.Join(t.TempDir(), "alice_key")
	if err := os.WriteFile(clientKeyPath, pem.EncodeToMemory(pemBlock), 0o600); err != nil {
		t.Fatalf("write client key: %v", err)
	}

	return &sshE2EEnv{
		srv:            srv,
		store:          authStore,
		tenant:         tenant,
		repo:           repoID,
		sshAddr:        sshAddr.String(),
		knownHostsPath: khPath,
		clientKeyPath:  clientKeyPath,
		clientPubKey:   clientSigner.PublicKey(),
		mirrorMgr:      mgr,
	}
}

// sshExec runs `ssh ... alice@host <remoteCmd>` against the e2e server and
// returns stdout, stderr, and the remote process's exit code. Used by the
// git-lfs-authenticate tests, which need to invoke ssh directly (no git
// wrapper) so they can inspect the JSON-on-stdout response.
func sshExec(t *testing.T, env *sshE2EEnv, remoteCmd string) (stdout, stderr []byte, exitCode int) {
	t.Helper()
	host, port, err := net.SplitHostPort(env.sshAddr)
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", env.sshAddr, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, "ssh",
		"-i", env.clientKeyPath,
		"-o", "UserKnownHostsFile="+env.knownHostsPath,
		"-o", "StrictHostKeyChecking=yes",
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "IdentitiesOnly=yes",
		"-p", port,
		"git@"+host,
		remoteCmd,
	)
	cmd.Env = append(os.Environ(),
		"HOME="+t.TempDir(), // hermetic — ignore developer's ~/.ssh/config
	)
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	stdout = []byte(outBuf.String())
	stderr = []byte(errBuf.String())
	if err == nil {
		return stdout, stderr, 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return stdout, stderr, exitErr.ExitCode()
	}
	t.Fatalf("ssh exec failed (non-ExitError): %v\nstderr=%s", err, stderr)
	return nil, nil, -1
}

// ---- LFS SSH-authenticate e2e --------------------------------------------

// TestE2E_LFSAuthenticate_HappyPath_Upload: alice has write, asks for
// upload → exit 0 + JSON {Header:Basic base64(alice:bvts_...), Href:<base>/t/r.git/info/lfs}.
func TestE2E_LFSAuthenticate_HappyPath_Upload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("e2e ssh tests are POSIX-only")
	}
	skipIfNoOpenSSH(t)

	const baseURL = "https://gw.example"
	env := newSSHE2ELFS(t, "acme", "foo", baseURL)
	seedAliceWithKey(t, env, env.clientPubKey)
	if err := env.store.Grant(context.Background(), "alice", "acme", "foo", "write"); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	stdout, stderr, code := sshExec(t, env, "git-lfs-authenticate acme/foo upload")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	var resp lfs.SSHAuthResponse
	if err := json.Unmarshal(stdout, &resp); err != nil {
		t.Fatalf("unmarshal: %v\nstdout=%q", err, stdout)
	}
	authz := resp.Header["Authorization"]
	if !strings.HasPrefix(authz, "Basic ") {
		t.Fatalf("Authorization header = %q, want Basic <...>", authz)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(authz, "Basic "))
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	user, secret, ok := strings.Cut(string(raw), ":")
	if !ok || user != "alice" || !strings.HasPrefix(secret, "bvts_") {
		t.Errorf("decoded basic credential: user=%q secret_prefix=%q", user, secret[:min(8, len(secret))])
	}
	wantHref := baseURL + "/acme/foo.git/info/lfs"
	if resp.Href != wantHref {
		t.Errorf("Href = %q, want %q", resp.Href, wantHref)
	}
	if time.Until(resp.ExpiresAt) <= 0 {
		t.Errorf("ExpiresAt = %v (already past); want a future timestamp", resp.ExpiresAt)
	}
}

// TestE2E_LFSAuthenticate_Forbidden_Upload: alice has only read, asks for
// upload → exit non-zero + stderr "insufficient permissions".
func TestE2E_LFSAuthenticate_Forbidden_Upload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("e2e ssh tests are POSIX-only")
	}
	skipIfNoOpenSSH(t)

	env := newSSHE2ELFS(t, "acme", "foo", "https://gw.example")
	seedAliceWithKey(t, env, env.clientPubKey)
	if err := env.store.Grant(context.Background(), "alice", "acme", "foo", "read"); err != nil {
		t.Fatalf("Grant read: %v", err)
	}

	stdout, stderr, code := sshExec(t, env, "git-lfs-authenticate acme/foo upload")
	if code == 0 {
		t.Fatalf("expected non-zero exit on forbidden, got 0; stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(string(stderr), "insufficient permissions") {
		t.Errorf("stderr = %q, want substring %q", stderr, "insufficient permissions")
	}
	if len(stdout) != 0 {
		t.Errorf("expected empty stdout on forbidden, got %q", stdout)
	}
}

// TestE2E_LFSAuthenticate_LFSDisabled: server built without LFS fields
// (plain newSSHE2E) → command exits non-zero + stderr "lfs not enabled".
func TestE2E_LFSAuthenticate_LFSDisabled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("e2e ssh tests are POSIX-only")
	}
	skipIfNoOpenSSH(t)

	env := newSSHE2E(t, "acme", "foo")
	seedAliceWithKey(t, env, env.clientPubKey)
	if err := env.store.Grant(context.Background(), "alice", "acme", "foo", "write"); err != nil {
		t.Fatalf("Grant write: %v", err)
	}

	stdout, stderr, code := sshExec(t, env, "git-lfs-authenticate acme/foo upload")
	if code == 0 {
		t.Fatalf("expected non-zero exit when LFS is disabled, got 0; stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(string(stderr), "lfs not enabled") {
		t.Errorf("stderr = %q, want substring %q", stderr, "lfs not enabled")
	}
	if len(stdout) != 0 {
		t.Errorf("expected empty stdout when LFS disabled, got %q", stdout)
	}
}

// TestE2E_LFSAuthenticate_HappyPath_Download: alice has only read,
// asks for download → exit 0 + JSON. Exercises the LFSOp→ActionRead
// refinement at dispatch time (TestParseExecCommand_LFSAuthenticate_*
// only covers the parser).
func TestE2E_LFSAuthenticate_HappyPath_Download(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("e2e ssh tests are POSIX-only")
	}
	skipIfNoOpenSSH(t)

	const baseURL = "https://gw.example"
	env := newSSHE2ELFS(t, "acme", "foo", baseURL)
	seedAliceWithKey(t, env, env.clientPubKey)
	if err := env.store.Grant(context.Background(), "alice", "acme", "foo", "read"); err != nil {
		t.Fatalf("Grant read: %v", err)
	}

	stdout, stderr, code := sshExec(t, env, "git-lfs-authenticate acme/foo download")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	var resp lfs.SSHAuthResponse
	if err := json.Unmarshal(stdout, &resp); err != nil {
		t.Fatalf("unmarshal: %v\nstdout=%q", err, stdout)
	}
	if !strings.HasPrefix(resp.Header["Authorization"], "Basic ") {
		t.Errorf("Authorization header = %q, want Basic <...>", resp.Header["Authorization"])
	}
	if resp.Href != baseURL+"/acme/foo.git/info/lfs" {
		t.Errorf("Href = %q", resp.Href)
	}
}

// TestE2E_LFSAuthenticate_DeployKey_Rejected: a deploy key with write
// scope on (acme, foo) attempts git-lfs-authenticate. Deploy-key actors
// have a "deploy:" UserID prefix and cannot mint tokens (the user_id
// FK in tokens would be invalid). handleLFSAuthenticate must reject
// with exit 128 and stderr "anonymous and deploy keys cannot mint LFS
// bearers". Guards against regressions in the prefix sentinel check.
func TestE2E_LFSAuthenticate_DeployKey_Rejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("e2e ssh tests are POSIX-only")
	}
	skipIfNoOpenSSH(t)

	env := newSSHE2ELFS(t, "acme", "foo", "https://gw.example")
	seedDeployKey(t, env, env.clientPubKey, "acme", "foo", auth.PermWrite)

	stdout, stderr, code := sshExec(t, env, "git-lfs-authenticate acme/foo upload")
	if code != 128 {
		t.Fatalf("expected exit 128 for deploy key, got %d; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(string(stderr), "anonymous and deploy keys cannot mint LFS bearers") {
		t.Errorf("stderr = %q, want deploy-key rejection message", stderr)
	}
	if len(stdout) != 0 {
		t.Errorf("expected empty stdout, got %q", stdout)
	}
}
