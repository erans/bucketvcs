package gitcli

import (
    "context"
    "os"
    "os/exec"
    "path/filepath"
    "testing"
)

// TestBundleCreate_ProducesValidBundle initializes a tiny bare repo,
// commits a synthetic ref, runs BundleCreate against it, then runs
// `git bundle verify` on the output and confirms it covers the ref.
func TestBundleCreate_ProducesValidBundle(t *testing.T) {
    skipIfNoGit(t)
    dir := t.TempDir()
    bareDir := filepath.Join(dir, "bare.git")
    if err := exec.Command("git", "init", "--bare", "-b", "main", bareDir).Run(); err != nil {
        t.Fatalf("git init bare: %v", err)
    }

    // Build a working tree and push to the bare repo.
    workDir := filepath.Join(dir, "work")
    if err := os.MkdirAll(workDir, 0o755); err != nil {
        t.Fatal(err)
    }
    runIn(t, workDir, "git", "init", "-b", "main", ".")
    runIn(t, workDir, "git", "config", "user.email", "t@t")
    runIn(t, workDir, "git", "config", "user.name", "t")
    if err := os.WriteFile(filepath.Join(workDir, "f"), []byte("hi"), 0o644); err != nil {
        t.Fatal(err)
    }
    runIn(t, workDir, "git", "add", ".")
    runIn(t, workDir, "git", "commit", "-m", "init")
    runIn(t, workDir, "git", "remote", "add", "origin", bareDir)
    runIn(t, workDir, "git", "push", "origin", "main")

    bundlePath := filepath.Join(dir, "out.bundle")
    if err := BundleCreate(context.Background(), bareDir, bundlePath, "refs/heads/main"); err != nil {
        t.Fatalf("BundleCreate: %v", err)
    }
    if fi, err := os.Stat(bundlePath); err != nil || fi.Size() == 0 {
        t.Fatalf("expected non-empty bundle file, got fi=%v err=%v", fi, err)
    }

    // git bundle verify validates the bundle integrity + lists refs.
    out, err := exec.Command("git", "-C", bareDir, "bundle", "verify", bundlePath).CombinedOutput()
    if err != nil {
        t.Fatalf("git bundle verify failed: %v\n%s", err, out)
    }
}

func TestBundleCreate_RefMissing_Errors(t *testing.T) {
    skipIfNoGit(t)
    dir := t.TempDir()
    bareDir := filepath.Join(dir, "bare.git")
    if err := exec.Command("git", "init", "--bare", "-b", "main", bareDir).Run(); err != nil {
        t.Fatalf("git init bare: %v", err)
    }
    err := BundleCreate(context.Background(), bareDir, filepath.Join(dir, "x.bundle"), "refs/heads/nonexistent")
    if err == nil {
        t.Fatalf("expected error for missing ref")
    }
}

func TestBundleCreate_RejectsBadRef(t *testing.T) {
	err := BundleCreate(context.Background(), t.TempDir(), filepath.Join(t.TempDir(), "x.bundle"), "--evil")
	if err == nil {
		t.Fatalf("expected error for bad ref")
	}
}

func runIn(t *testing.T, dir string, name string, args ...string) {
    t.Helper()
    cmd := exec.Command(name, args...)
    cmd.Dir = dir
    out, err := cmd.CombinedOutput()
    if err != nil {
        t.Fatalf("%s %v: %v\n%s", name, args, err, out)
    }
}
