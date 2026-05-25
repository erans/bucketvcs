//go:build bwrap

package hooks_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/hooks"
)

// bwrapSupportsRlimit returns true if `bwrap --rlimit-cpu 1 -- /bin/true`
// exits 0. bwrap < 0.12 rejects the flag with a usage error.
func bwrapSupportsRlimit() bool {
	cmd := exec.Command("bwrap", "--rlimit-cpu", "1", "--", "/bin/true")
	return cmd.Run() == nil
}

func newSandboxRunner(t *testing.T) (*hooks.Runner, string) {
	t.Helper()
	bw, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bwrap not on PATH")
	}
	if !bwrapSupportsRlimit() {
		t.Skip("bwrap does not support --rlimit-cpu (needs >= 0.12)")
	}
	root := t.TempDir()
	return hooks.NewRunner(hooks.RunnerConfig{
		HooksRoot:   root,
		UseSandbox:  true,
		BwrapPath:   bw,
		TimeoutSec:  10,
		CPUSec:      5,
		MemoryMB:    128,
		OutputMaxKB: 16,
	}), root
}

func writeSandboxScript(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestSandbox_NoEtcAccess(t *testing.T) {
	r, root := newSandboxRunner(t)
	writeSandboxScript(t, root, "ls-etc.sh",
		"#!/bin/sh\nif ls /etc 2>/dev/null; then echo ETC-VISIBLE; exit 1; fi; echo ETC-HIDDEN\n")
	res := r.Run(context.Background(), "", "ls-etc.sh", nil, nil)
	if res.ExitCode != 0 || !strings.Contains(string(res.Stdout), "ETC-HIDDEN") {
		t.Errorf("Stdout=%q ExitCode=%d Err=%v — expected ETC-HIDDEN", res.Stdout, res.ExitCode, res.Err)
	}
}

func TestSandbox_NoNetworkByDefault(t *testing.T) {
	r, root := newSandboxRunner(t)
	writeSandboxScript(t, root, "net.sh",
		"#!/bin/sh\nif ip route 2>/dev/null | grep -q default; then echo HAS-NET; exit 1; fi; echo NO-NET\n")
	res := r.Run(context.Background(), "", "net.sh", nil, nil)
	if res.ExitCode != 0 || !strings.Contains(string(res.Stdout), "NO-NET") {
		t.Errorf("Stdout=%q ExitCode=%d Err=%v", res.Stdout, res.ExitCode, res.Err)
	}
}

func TestSandbox_EnvVarsPropagated(t *testing.T) {
	r, root := newSandboxRunner(t)
	writeSandboxScript(t, root, "env.sh",
		"#!/bin/sh\necho \"$BUCKETVCS_TENANT:$BUCKETVCS_REPO:$BUCKETVCS_TRIGGER\"\n")
	res := r.Run(context.Background(), "", "env.sh", nil, map[string]string{
		"BUCKETVCS_TENANT":  "acme",
		"BUCKETVCS_REPO":    "site",
		"BUCKETVCS_TRIGGER": "pre-receive",
	})
	if res.Err != nil || !strings.Contains(string(res.Stdout), "acme:site:pre-receive") {
		t.Errorf("Stdout=%q Err=%v", res.Stdout, res.Err)
	}
}

func TestSandbox_TmpfsWritable(t *testing.T) {
	r, root := newSandboxRunner(t)
	writeSandboxScript(t, root, "tmp.sh",
		"#!/bin/sh\necho hi > /tmp/test && cat /tmp/test && exit 0\nexit 1\n")
	res := r.Run(context.Background(), "", "tmp.sh", nil, nil)
	if res.Err != nil || res.ExitCode != 0 {
		t.Errorf("/tmp tmpfs not writable: Stdout=%q ExitCode=%d Err=%v", res.Stdout, res.ExitCode, res.Err)
	}
}

// TestSandbox_RepoMountReadable verifies the bareDir parameter is bind-mounted
// at /repo and the script can read files from it. M20's worked example
// (operator guide §11.6) and the `enforce-ticket-link.sh` script rely on this.
func TestSandbox_RepoMountReadable(t *testing.T) {
	r, root := newSandboxRunner(t)
	bareDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(bareDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSandboxScript(t, root, "read-head.sh",
		"#!/bin/sh\ncat /repo/HEAD\n")
	res := r.Run(context.Background(), bareDir, "read-head.sh", nil, nil)
	if res.Err != nil || res.ExitCode != 0 {
		t.Fatalf("read /repo/HEAD failed: ExitCode=%d Err=%v Stderr=%q", res.ExitCode, res.Err, res.Stderr)
	}
	if !strings.Contains(string(res.Stdout), "ref: refs/heads/main") {
		t.Errorf("Stdout=%q, want 'ref: refs/heads/main'", res.Stdout)
	}
}

// TestSandbox_RepoMountIsReadOnly verifies the bind mount of bareDir at /repo
// is read-only — a malicious or buggy hook script cannot mutate the repo.
func TestSandbox_RepoMountIsReadOnly(t *testing.T) {
	r, root := newSandboxRunner(t)
	bareDir := t.TempDir()
	writeSandboxScript(t, root, "try-write.sh",
		"#!/bin/sh\nif echo bad > /repo/test 2>/dev/null; then echo WROTE; exit 1; fi; echo READONLY\n")
	res := r.Run(context.Background(), bareDir, "try-write.sh", nil, nil)
	if res.Err != nil || res.ExitCode != 0 {
		t.Fatalf("Err=%v ExitCode=%d Stderr=%q", res.Err, res.ExitCode, res.Stderr)
	}
	if !strings.Contains(string(res.Stdout), "READONLY") {
		t.Errorf("Stdout=%q, want READONLY (write should have failed)", res.Stdout)
	}
}

// TestSandbox_CrossTenantContainment verifies a hook running for tenant A's
// bareDir cannot see tenant B's bareDir contents — different mount namespaces
// expose only the bind-mounted directory.
func TestSandbox_CrossTenantContainment(t *testing.T) {
	r, root := newSandboxRunner(t)
	tenantA := t.TempDir()
	tenantB := t.TempDir()
	// Plant a secret in tenant B.
	if err := os.WriteFile(filepath.Join(tenantB, "secret"), []byte("tenant-b-secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSandboxScript(t, root, "probe.sh",
		"#!/bin/sh\nif [ -f /repo/secret ]; then echo LEAK; cat /repo/secret; exit 1; fi; echo CONTAINED\n")
	// Run as tenant A; only tenantA is mounted at /repo, so tenantB's secret
	// must not be visible.
	res := r.Run(context.Background(), tenantA, "probe.sh", nil, nil)
	if res.Err != nil || res.ExitCode != 0 {
		t.Fatalf("Err=%v ExitCode=%d Stderr=%q Stdout=%q", res.Err, res.ExitCode, res.Stderr, res.Stdout)
	}
	if !strings.Contains(string(res.Stdout), "CONTAINED") {
		t.Errorf("Stdout=%q, want CONTAINED — cross-tenant leak detected", res.Stdout)
	}
}
