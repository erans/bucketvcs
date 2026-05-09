//go:build !windows

package diffharness

// clone_ssh_vs_http_oracle_test.go: SSH-vs-HTTP differential oracle (M6 Task 27)
//
// For every fixture in fixtures.Registry, this oracle:
//  1. Seeds the fixture into a shared in-process object store.
//  2. Starts a gateway HTTP server (identical to the auth-clone oracle).
//  3. Starts an SSH server pointing at the same store.
//  4. Registers a user with both an HTTPS token (admin, no per-repo grant
//     needed) and an SSH deploy key scoped to read on the fixture repo.
//  5. Clones over HTTPS with Basic auth into tmp/http-clone.
//  6. Clones over SSH (GIT_SSH_COMMAND) into tmp/ssh-clone.
//  7. Asserts that `git rev-list --all --objects` (sorted) is identical
//     across both clones.
//
// This validates that the SSH gateway engine emits exactly the same object
// closure as the HTTP/upload-pack gateway for every fixture.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net"
	"net/http/httptest"
	"net/url"
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
	"github.com/bucketvcs/bucketvcs/internal/diffharness/fixtures"
	"github.com/bucketvcs/bucketvcs/internal/gateway"
	"github.com/bucketvcs/bucketvcs/internal/importer"
	"github.com/bucketvcs/bucketvcs/internal/mirror"
	sshd "github.com/bucketvcs/bucketvcs/internal/sshd"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// skipIfNoSSH skips the test if git or ssh binaries are unavailable.
func skipIfNoSSH(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh binary not available")
	}
}

// sshHTTPOracle bundles all resources needed for one fixture run.
type sshHTTPOracle struct {
	// HTTP side
	httpURL string // "http://user:token@127.0.0.1:PORT/tenant/repo.git"

	// SSH side
	sshAddr       string // "127.0.0.1:PORT"
	sshScript     string // path to the GIT_SSH_COMMAND wrapper script
	sshRemoteURL  string // "ssh://git@127.0.0.1:PORT/tenant/repo.git"
	sshTempHome   string // hermetic HOME for git-over-SSH

	// Set only for non-empty fixtures.
	hasRefs bool
}

// newSSHHTTPOracle sets up a complete dual-transport test environment for
// the given fixture and returns the oracle bundle.
//
// Resources are registered with t.Cleanup; caller must not close them.
func newSSHHTTPOracle(t *testing.T, name string, build fixtures.Builder) *sshHTTPOracle {
	t.Helper()

	tenant := "fx"
	repoID := name

	// ---- 1. Build the fixture source repo -----------------------------------
	workDir := t.TempDir()
	srcDir := filepath.Join(workDir, "src")
	fx := build(t, srcDir)
	gitFsck(t, srcDir)

	hasRefs := len(fx.Refs) > 0
	defaultBranch := ""
	if !hasRefs {
		defaultBranch = "refs/heads/main"
	}

	// ---- 2. Shared object store ---------------------------------------------
	storeDir := t.TempDir()
	objStore, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("[%s] localfs.Open: %v", name, err)
	}
	t.Cleanup(func() { _ = objStore.Close() })

	if _, err := importer.Import(context.Background(), objStore, importer.Options{
		SourceDir:     srcDir,
		Tenant:        tenant,
		Repo:          repoID,
		Actor:         "harness",
		DefaultBranch: defaultBranch,
	}); err != nil {
		t.Fatalf("[%s] Import: %v", name, err)
	}

	// ---- 3. Shared auth store -----------------------------------------------
	authDB := filepath.Join(t.TempDir(), "auth.db")
	authStore, err := sqlitestore.Open(authDB)
	if err != nil {
		t.Fatalf("[%s] sqlitestore.Open: %v", name, err)
	}
	t.Cleanup(func() { _ = authStore.Close() })

	ctx := context.Background()
	if err := authStore.RegisterRepo(ctx, tenant, repoID); err != nil {
		t.Fatalf("[%s] RegisterRepo: %v", name, err)
	}

	// Admin user for HTTPS (admin role short-circuits per-repo grant check).
	const adminName = "diffadmin"
	uid, err := authStore.CreateUser(ctx, adminName, true /* isAdmin */)
	if err != nil {
		t.Fatalf("[%s] CreateUser: %v", name, err)
	}
	tokStr, tokID, tokSecret, err := auth.GenerateToken()
	if err != nil {
		t.Fatalf("[%s] GenerateToken: %v", name, err)
	}
	tokHash, err := auth.HashSecret(tokSecret)
	if err != nil {
		t.Fatalf("[%s] HashSecret: %v", name, err)
	}
	if err := authStore.CreateToken(ctx, tokID, uid, tokHash, "diffharness", nil); err != nil {
		t.Fatalf("[%s] CreateToken: %v", name, err)
	}

	// SSH deploy key with read permission on this repo.
	_, clientPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("[%s] GenerateKey: %v", name, err)
	}
	clientSigner, err := ssh.NewSignerFromKey(clientPriv)
	if err != nil {
		t.Fatalf("[%s] NewSignerFromKey: %v", name, err)
	}
	pemBlock, err := ssh.MarshalPrivateKey(clientPriv, "")
	if err != nil {
		t.Fatalf("[%s] MarshalPrivateKey: %v", name, err)
	}
	clientKeyPath := filepath.Join(t.TempDir(), "deploy_key")
	if err := os.WriteFile(clientKeyPath, pem.EncodeToMemory(pemBlock), 0o600); err != nil {
		t.Fatalf("[%s] write deploy key: %v", name, err)
	}
	fp := sshFingerprint(clientSigner.PublicKey())
	keyID, err := auth.GenerateSSHKeyID()
	if err != nil {
		t.Fatalf("[%s] GenerateSSHKeyID: %v", name, err)
	}
	if err := authStore.AddSSHKey(ctx, auth.SSHKey{
		ID:          keyID,
		Fingerprint: fp,
		PublicKey:   clientSigner.PublicKey().Marshal(),
		KeyType:     clientSigner.PublicKey().Type(),
		Label:       "oracle-deploy-read",
		ScopeTenant: tenant,
		ScopeRepo:   repoID,
		ScopePerm:   auth.PermRead,
	}); err != nil {
		t.Fatalf("[%s] AddSSHKey: %v", name, err)
	}

	// ---- 4. HTTP gateway ----------------------------------------------------
	httpSrv, err := gateway.NewServer(objStore, gateway.Options{
		MirrorDir: t.TempDir(),
		Version:   "test",
		AuthStore: authStore,
	})
	if err != nil {
		t.Fatalf("[%s] gateway.NewServer: %v", name, err)
	}
	t.Cleanup(func() { _ = httpSrv.Close() })
	ts := httptest.NewServer(httpSrv)
	t.Cleanup(ts.Close)

	rawURL := ts.URL + "/" + tenant + "/" + repoID + ".git"
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("[%s] parse URL: %v", name, err)
	}
	u.User = url.UserPassword(adminName, tokStr)
	httpCloneURL := u.String()

	// ---- 5. Mirror manager for SSH server -----------------------------------
	mirrorDir := t.TempDir()
	mgr, err := mirror.NewManager(mirrorDir, objStore)
	if err != nil {
		t.Fatalf("[%s] mirror.NewManager: %v", name, err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	// ---- 6. SSH server -------------------------------------------------------
	hostKeyPath := filepath.Join(t.TempDir(), "host_key")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	sshdSrv, err := sshd.NewServer(sshd.Options{
		Addr:         "127.0.0.1:0",
		HostKeyPath:  hostKeyPath,
		Grace:        0,
		Store:        authStore,
		BVStore:      objStore,
		Mirror:       mgr,
		Logger:       logger,
		AgentVersion: "oracle-test",
	})
	if err != nil {
		t.Fatalf("[%s] sshd.NewServer: %v", name, err)
	}
	if err := sshdSrv.Listen(); err != nil {
		t.Fatalf("[%s] sshdSrv.Listen: %v", name, err)
	}
	srvCtx, srvCancel := context.WithCancel(ctx)
	t.Cleanup(func() {
		srvCancel()
		_ = sshdSrv.Close()
	})
	go sshdSrv.Serve(srvCtx) //nolint:errcheck

	// ---- 7. known_hosts pinning for the SSH client --------------------------
	hostPriv, err := oracleLoadPrivKey(hostKeyPath)
	if err != nil {
		t.Fatalf("[%s] load host key: %v", name, err)
	}
	sshTCPAddr := sshdSrv.Addr().(*net.TCPAddr)
	hostEntry := oracleBuildKnownHosts(sshTCPAddr, hostPriv.PublicKey())

	khPath := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(khPath, []byte(hostEntry+"\n"), 0o600); err != nil {
		t.Fatalf("[%s] write known_hosts: %v", name, err)
	}

	// ---- 8. GIT_SSH_COMMAND wrapper script ----------------------------------
	scriptDir := t.TempDir()
	script := filepath.Join(scriptDir, "git-ssh.sh")
	scriptBody := fmt.Sprintf(`#!/usr/bin/env bash
exec ssh \
  -i %s \
  -o UserKnownHostsFile=%s \
  -o StrictHostKeyChecking=yes \
  -o GlobalKnownHostsFile=/dev/null \
  -o IdentitiesOnly=yes \
  "$@"
`,
		oracleShellQ(clientKeyPath),
		oracleShellQ(khPath),
	)
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatalf("[%s] write ssh script: %v", name, err)
	}

	sshAddr := sshTCPAddr.String()
	sshURL := "ssh://git@" + sshAddr + "/" + tenant + "/" + repoID + ".git"
	sshHome := t.TempDir()

	return &sshHTTPOracle{
		httpURL:      httpCloneURL,
		sshAddr:      sshAddr,
		sshScript:    script,
		sshRemoteURL: sshURL,
		sshTempHome:  sshHome,
		hasRefs:      hasRefs,
	}
}

// oracleShellQ single-quotes a path for use inside a shell script.
func oracleShellQ(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// oracleLoadPrivKey reads a PEM-encoded OpenSSH private key and returns the signer.
func oracleLoadPrivKey(path string) (ssh.Signer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(raw)
}

// oracleBuildKnownHosts produces a single known_hosts entry for the given
// TCP address and host public key.
func oracleBuildKnownHosts(addr *net.TCPAddr, pub ssh.PublicKey) string {
	hostStr := fmt.Sprintf("[%s]:%d", addr.IP.String(), addr.Port)
	line := strings.TrimRight(string(ssh.MarshalAuthorizedKey(pub)), "\n")
	return hostStr + " " + line
}

// sshFingerprint returns the SHA-256 fingerprint of a public key in the
// format expected by sqlitestore (matches sshd.SHA256Fingerprint).
// We inline it here to avoid importing the sshd package for a single
// utility function, and to stay within the diffharness package boundary.
func sshFingerprint(pub ssh.PublicKey) string {
	return ssh.FingerprintSHA256(pub)
}

// cloneOverHTTPOracle clones the fixture repo over HTTPS using Basic auth.
// Returns the destination directory. For empty fixtures, returns "" and skips
// further assertions.
func cloneOverHTTPOracle(t *testing.T, env *sshHTTPOracle, workDir, name string) string {
	t.Helper()
	if !env.hasRefs {
		return ""
	}
	dstDir := filepath.Join(workDir, "http-clone.git")
	cmd := exec.Command("git", "clone", "--mirror",
		"-c", "protocol.version=2",
		env.httpURL, dstDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("[%s] git clone --mirror (HTTP): %v\n%s", name, err, out)
	}
	return dstDir
}

// cloneOverSSHOracle clones the fixture repo over SSH.
// Returns the destination directory. For empty fixtures, returns "".
func cloneOverSSHOracle(t *testing.T, env *sshHTTPOracle, workDir, name string) string {
	t.Helper()
	if !env.hasRefs {
		return ""
	}
	dstDir := filepath.Join(workDir, "ssh-clone.git")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, "git", "clone", "--mirror",
		"-c", "protocol.version=2",
		env.sshRemoteURL, dstDir)
	cmd.Env = append(os.Environ(),
		"GIT_SSH_COMMAND="+env.sshScript,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"HOME="+env.sshTempHome,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("[%s] git clone --mirror (SSH): %v\n%s", name, err, out)
	}
	return dstDir
}

// TestOracle_CloneEquivalenceSSHvsHTTP asserts that for every fixture, a
// `git clone --mirror` over HTTPS and over SSH against the same gateway
// state produce identical reachable object closures (git rev-list --all
// --objects, sorted).
func TestOracle_CloneEquivalenceSSHvsHTTP(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SSH oracle uses bash scripts; not supported on Windows")
	}
	skipIfNoGit(t)
	skipIfNoSSH(t)

	for name, build := range fixtures.Registry {
		name, build := name, build
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			workDir := t.TempDir()

			env := newSSHHTTPOracle(t, name, build)

			// Empty fixture: both transports produce no objects — trivially equal.
			if !env.hasRefs {
				return
			}

			httpCloneDir := cloneOverHTTPOracle(t, env, workDir, name)
			sshCloneDir := cloneOverSSHOracle(t, env, workDir, name)

			gitFsck(t, httpCloneDir)
			gitFsck(t, sshCloneDir)

			httpRefs := gitShowRef(t, httpCloneDir)
			sshRefs := gitShowRef(t, sshCloneDir)
			if !equalRefs(httpRefs, sshRefs) {
				t.Fatalf("[%s] refs differ between HTTP and SSH clones.\nHTTP=%v\nSSH= %v",
					name, httpRefs, sshRefs)
			}

			httpOIDs := gitRevListAllObjects(t, httpCloneDir)
			sshOIDs := gitRevListAllObjects(t, sshCloneDir)
			if !equalOIDLists(httpOIDs, sshOIDs) {
				t.Fatalf("[%s] object closure differs between HTTP and SSH clones.\nHTTP=%v\nSSH= %v",
					name, httpOIDs, sshOIDs)
			}
		})
	}
}
