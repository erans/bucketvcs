package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	repoerrs "github.com/bucketvcs/bucketvcs/internal/repo/repoerrs"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := gitcli.Version(context.Background()); err != nil {
		t.Skip("git not available")
	}
}

// makeSrcRepo authors a tiny bare repo and returns its path. Used by
// importer tests across Tasks 16-18.
func makeSrcRepo(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	mustGit := func(args ...string) {
		t.Helper()
		out, err := gitcli.RunForTest(work, args...)
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	mustGit("init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(work, "f"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustGit("add", "f")
	mustGit("-c", "user.name=t", "-c", "user.email=t@e", "commit", "-m", "init")
	bare := filepath.Join(t.TempDir(), "bare")
	if err := gitcli.CloneBareMirror(context.Background(), work, bare); err != nil {
		t.Fatalf("CloneBareMirror: %v", err)
	}
	return bare
}

func TestPrepareLocalPack_ProducesPackAndRefs(t *testing.T) {
	skipIfNoGit(t)
	src := makeSrcRepo(t)
	prep, err := prepareLocalPack(context.Background(), src)
	if err != nil {
		t.Fatalf("prepareLocalPack: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prep.WorkDir) })

	if prep.PackID == "" || len(prep.PackID) != 40 {
		t.Fatalf("PackID: %q", prep.PackID)
	}
	if _, err := os.Stat(prep.PackPath); err != nil {
		t.Fatalf("pack stat: %v", err)
	}
	if _, err := os.Stat(prep.IdxPath); err != nil {
		t.Fatalf("idx stat: %v", err)
	}
	if len(prep.Refs) == 0 {
		t.Fatalf("expected refs")
	}
	if !strings.HasPrefix(prep.DefaultBranch, "refs/heads/") {
		t.Fatalf("DefaultBranch: %q", prep.DefaultBranch)
	}
	if _, err := os.Stat(prep.WorkDir); err != nil {
		t.Fatalf("workdir stat: %v", err)
	}
}

func TestPrepareLocalPack_FsckRejectsCorruptSource(t *testing.T) {
	skipIfNoGit(t)
	src := makeSrcRepo(t)
	// Drop a corrupt loose object into the source repo to make fsck fail.
	bogusDir := filepath.Join(src, "objects", "ab")
	if err := os.MkdirAll(bogusDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bogusDir, "cdef0123456789012345678901234567890123"), []byte("garbage"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := prepareLocalPack(context.Background(), src); err == nil {
		t.Fatalf("expected prepareLocalPack to fail on corrupt source")
	}
}

func TestPrepareLocalPack_RejectsNonexistentSource(t *testing.T) {
	skipIfNoGit(t)
	if _, err := prepareLocalPack(context.Background(), "/nonexistent/path"); err == nil {
		t.Fatalf("expected prepareLocalPack to fail on nonexistent source")
	}
}

func TestBuildIndexes_FromPreparedPack(t *testing.T) {
	skipIfNoGit(t)
	src := makeSrcRepo(t)
	prep, err := prepareLocalPack(context.Background(), src)
	if err != nil {
		t.Fatalf("prepareLocalPack: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prep.WorkDir) })

	indexes, err := buildIndexesLocal(context.Background(), prep)
	if err != nil {
		t.Fatalf("buildIndexesLocal: %v", err)
	}
	if len(indexes.ObjectMapBytes) == 0 {
		t.Fatalf("empty .bvom")
	}
	if len(indexes.CommitGraphBytes) == 0 {
		t.Fatalf("empty .bvcg")
	}
	if indexes.ObjectMapHash != sha256Hex(indexes.ObjectMapBytes) {
		t.Fatalf(".bvom hash mismatch")
	}
	if indexes.CommitGraphHash != sha256Hex(indexes.CommitGraphBytes) {
		t.Fatalf(".bvcg hash mismatch")
	}
	if indexes.ObjectCount == 0 {
		t.Fatalf("ObjectCount=0")
	}
	if indexes.PackSizeBytes <= 0 {
		t.Fatalf("PackSizeBytes=%d", indexes.PackSizeBytes)
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestPrepareLocalPack_EmptyRepo(t *testing.T) {
	skipIfNoGit(t)
	work := t.TempDir()
	bare := filepath.Join(t.TempDir(), "bare")
	if out, err := gitcli.RunForTest(work, "init", "--initial-branch=main"); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	// Don't author any commits — empty repo.
	if err := gitcli.CloneBareMirror(context.Background(), work, bare); err != nil {
		t.Fatalf("CloneBareMirror: %v", err)
	}
	prep, err := prepareLocalPack(context.Background(), bare)
	if err != nil {
		t.Fatalf("prepareLocalPack on empty repo: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prep.WorkDir) })
	if prep.PackID != "" {
		t.Fatalf("PackID for empty repo: got %q, want \"\"", prep.PackID)
	}
	if len(prep.Refs) != 0 {
		t.Fatalf("Refs for empty repo: got %v, want none", prep.Refs)
	}
	if !strings.HasPrefix(prep.DefaultBranch, "refs/heads/") {
		t.Fatalf("DefaultBranch: %q", prep.DefaultBranch)
	}
}

func TestBuildIndexes_EmptyRepo(t *testing.T) {
	skipIfNoGit(t)
	work := t.TempDir()
	bare := filepath.Join(t.TempDir(), "bare")
	if out, err := gitcli.RunForTest(work, "init", "--initial-branch=main"); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := gitcli.CloneBareMirror(context.Background(), work, bare); err != nil {
		t.Fatalf("CloneBareMirror: %v", err)
	}
	prep, err := prepareLocalPack(context.Background(), bare)
	if err != nil {
		t.Fatalf("prepareLocalPack: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prep.WorkDir) })
	indexes, err := buildIndexesLocal(context.Background(), prep)
	if err != nil {
		t.Fatalf("buildIndexesLocal on empty repo: %v", err)
	}
	if len(indexes.ObjectMapBytes) != 0 || indexes.ObjectMapHash != "" {
		t.Fatalf("expected empty .bvom for empty repo")
	}
	if len(indexes.CommitGraphBytes) != 0 || indexes.CommitGraphHash != "" {
		t.Fatalf("expected empty .bvcg for empty repo")
	}
	if indexes.ObjectCount != 0 || indexes.PackSizeBytes != 0 {
		t.Fatalf("expected zero counts/size")
	}
}

func newTestStore(t *testing.T) storage.ObjectStore {
	t.Helper()
	s, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestImport_RoundTripState(t *testing.T) {
	skipIfNoGit(t)
	src := makeSrcRepo(t)
	store := newTestStore(t)
	res, err := Import(context.Background(), store, Options{
		SourceDir: src,
		Tenant:    "acme", Repo: "x",
		Actor: "tester",
	})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(res.PackID) != 40 {
		t.Fatalf("PackID: %q", res.PackID)
	}
	if res.ManifestVersion != 2 {
		// Create writes manifest_version=1; the import Commit advances to 2.
		t.Fatalf("ManifestVersion: got %d, want 2", res.ManifestVersion)
	}
	r, err := repo.Open(context.Background(), store, "acme", "x")
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	view, err := r.ReadRoot(context.Background())
	if err != nil {
		t.Fatalf("ReadRoot: %v", err)
	}
	var body manifest.Body
	if err := json.Unmarshal(view.Body, &body); err != nil {
		t.Fatalf("Unmarshal body: %v", err)
	}
	if body.DefaultBranch != "refs/heads/main" {
		t.Fatalf("default_branch: %q", body.DefaultBranch)
	}
	if len(body.Refs) == 0 {
		t.Fatalf("no refs in committed body")
	}
	if len(body.Packs) != 1 {
		t.Fatalf("packs: %d", len(body.Packs))
	}
	if body.Packs[0].PackID != res.PackID {
		t.Fatalf("PackID mismatch")
	}
	if body.Indexes.ObjectMap == nil || body.Indexes.ObjectMap.Hash != res.ObjectMapHash {
		t.Fatalf("ObjectMap.Hash mismatch")
	}
	if body.Indexes.CommitGraph == nil || body.Indexes.CommitGraph.Hash != res.CommitGraphHash {
		t.Fatalf("CommitGraph.Hash mismatch")
	}
}

func TestImport_RejectsExistingRepo(t *testing.T) {
	skipIfNoGit(t)
	src := makeSrcRepo(t)
	store := newTestStore(t)
	if _, err := Import(context.Background(), store, Options{SourceDir: src, Tenant: "t", Repo: "r"}); err != nil {
		t.Fatalf("first Import: %v", err)
	}
	if _, err := Import(context.Background(), store, Options{SourceDir: src, Tenant: "t", Repo: "r"}); err == nil {
		t.Fatalf("second Import should fail with ErrRepoExists")
	}
}

func TestImport_EmptyRepoCommitsEmptyBody(t *testing.T) {
	skipIfNoGit(t)
	work := t.TempDir()
	bare := filepath.Join(t.TempDir(), "bare")
	if out, err := gitcli.RunForTest(work, "init", "--initial-branch=main"); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := gitcli.CloneBareMirror(context.Background(), work, bare); err != nil {
		t.Fatalf("CloneBareMirror: %v", err)
	}
	store := newTestStore(t)
	res, err := Import(context.Background(), store, Options{
		SourceDir: bare,
		Tenant:    "acme", Repo: "empty",
	})
	if err != nil {
		t.Fatalf("Import empty repo: %v", err)
	}
	if res.PackID != "" {
		t.Fatalf("empty repo PackID should be \"\", got %q", res.PackID)
	}
	r, err := repo.Open(context.Background(), store, "acme", "empty")
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	view, err := r.ReadRoot(context.Background())
	if err != nil {
		t.Fatalf("ReadRoot: %v", err)
	}
	var body manifest.Body
	if err := json.Unmarshal(view.Body, &body); err != nil {
		t.Fatalf("Unmarshal body: %v", err)
	}
	if len(body.Packs) != 0 || len(body.Refs) != 0 {
		t.Fatalf("empty repo body should have empty packs+refs")
	}
}

func TestImport_RecoversFromCreateOnlyState(t *testing.T) {
	skipIfNoGit(t)
	src := makeSrcRepo(t)
	store := newTestStore(t)
	// Pre-create the repo (simulating a prior import that did Create
	// but failed before Commit).
	if _, err := repo.Create(context.Background(), store, "acme", "x", repo.CreateOptions{
		DefaultBranch: "refs/heads/main",
		ObjectFormat:  "sha1",
		Actor:         "test",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Import should detect the empty state and continue with the Commit
	// instead of failing with ErrRepoExists.
	res, err := Import(context.Background(), store, Options{
		SourceDir: src, Tenant: "acme", Repo: "x", Actor: "tester",
	})
	if err != nil {
		t.Fatalf("Import on recoverable state: %v", err)
	}
	if res.PackID == "" {
		t.Fatalf("expected PackID after recovery")
	}
	if res.ManifestVersion < 2 {
		t.Fatalf("ManifestVersion: got %d, want >=2", res.ManifestVersion)
	}
}

func TestImport_RejectsRealConflict(t *testing.T) {
	skipIfNoGit(t)
	src := makeSrcRepo(t)
	store := newTestStore(t)
	if _, err := Import(context.Background(), store, Options{
		SourceDir: src, Tenant: "t", Repo: "r",
	}); err != nil {
		t.Fatalf("first Import: %v", err)
	}
	// Second import: repo body is now populated, so this is a real conflict.
	_, err := Import(context.Background(), store, Options{
		SourceDir: src, Tenant: "t", Repo: "r",
	})
	if err == nil {
		t.Fatalf("expected error on real conflict")
	}
	if !errors.Is(err, repoerrs.ErrRepoExists) {
		t.Fatalf("expected ErrRepoExists, got %v", err)
	}
}
