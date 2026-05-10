// Package mtest provides shared test fixtures for the maintenance
// package. All exports are intended for `_test.go` files only.
package mtest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/importer"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// GitAvailable skips the test if `git` is not on PATH.
func GitAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

// MustRunGit runs `git <args>` in dir; t.Fatal on failure.
func MustRunGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// MustGitOutput runs `git <args>` in dir, returning combined output;
// t.Fatal on failure.
func MustGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// UploadFile streams a local file to the given key on s via PutIfAbsent.
func UploadFile(t *testing.T, s storage.ObjectStore, srcPath, dstKey string) {
	t.Helper()
	f, err := os.Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := s.PutIfAbsent(context.Background(), dstKey, f, nil); err != nil {
		t.Fatal(err)
	}
}

// RepackedPack describes a freshly-prepared bare repo + pack on local
// disk, suitable for feeding into pack.Open / objindex.Build /
// commitgraph.Build. Used by Task 4.1's tests.
type RepackedPack struct {
	BareDir  string
	PackPath string
	IdxPath  string
	PackID   string
	Refs     map[string]string
}

// SetupSyntheticBareRepo creates a small synthetic bare repo on disk
// (via real git init + commit + push), runs gitcli.PackObjectsAll, and
// returns the resulting bare.git directory. The bare repo has one
// commit reachable from refs/heads/main.
//
// Caller does not need to clean up; t.TempDir is used for all paths.
func SetupSyntheticBareRepo(t *testing.T) string {
	t.Helper()
	GitAvailable(t)
	src := t.TempDir()
	MustRunGit(t, src, "init", "--bare")
	MustRunGit(t, src, "config", "user.email", "test@example.com")
	MustRunGit(t, src, "config", "user.name", "T")
	wt := t.TempDir()
	MustRunGit(t, wt, "init")
	MustRunGit(t, wt, "config", "user.email", "test@example.com")
	MustRunGit(t, wt, "config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(wt, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	MustRunGit(t, wt, "add", ".")
	MustRunGit(t, wt, "commit", "-m", "init")
	MustRunGit(t, wt, "remote", "add", "origin", src)
	MustRunGit(t, wt, "push", "origin", "HEAD:refs/heads/main")
	// Set the bare repo's HEAD to point to refs/heads/main so rev-parse HEAD works.
	MustRunGit(t, src, "symbolic-ref", "HEAD", "refs/heads/main")
	return src
}

// SetupRepackedPack prepares a bare repo (via SetupSyntheticBareRepo),
// runs gitcli.PackObjectsAll, and returns the local pack/idx paths
// plus the head ref map suitable for buildIndexesFromLocalPack.
func SetupRepackedPack(t *testing.T) RepackedPack {
	t.Helper()
	bare := SetupSyntheticBareRepo(t)
	prefix := filepath.Join(t.TempDir(), "out", "pack")
	if err := os.MkdirAll(filepath.Dir(prefix), 0o755); err != nil {
		t.Fatal(err)
	}
	packID, err := gitcli.PackObjectsAll(context.Background(), bare, prefix)
	if err != nil {
		t.Fatalf("PackObjectsAll: %v", err)
	}
	headOID := strings.TrimSpace(MustGitOutput(t, bare, "rev-parse", "HEAD"))
	return RepackedPack{
		BareDir:  bare,
		PackPath: prefix + "-" + packID + ".pack",
		IdxPath:  prefix + "-" + packID + ".idx",
		PackID:   packID,
		Refs:     map[string]string{"refs/heads/main": headOID},
	}
}

// LocalfsStore opens a fresh localfs ObjectStore at t.TempDir().
func LocalfsStore(t *testing.T) storage.ObjectStore {
	t.Helper()
	dir := t.TempDir()
	s, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// SeedRepoFromImport creates a synthetic source repo on disk and runs
// importer.Import against the given store, leaving a usable repo at
// (tenant, repo) with one canonical pack reachable from refs/heads/main.
func SeedRepoFromImport(t *testing.T, s storage.ObjectStore, tenant, repo string) {
	t.Helper()
	GitAvailable(t)
	src := SetupSyntheticBareRepo(t)
	if _, err := importer.Import(context.Background(), s, importer.Options{
		SourceDir: src,
		Tenant:    tenant,
		Repo:      repo,
		Actor:     "u_seed",
	}); err != nil {
		t.Fatalf("importer.Import: %v", err)
	}
}
