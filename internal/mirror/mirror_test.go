package mirror

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/importer"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// makeImportedRepo imports a tiny synthetic repo into a localfs store and
// returns (openStore, tenant, repoID). The store is closed via t.Cleanup.
func makeImportedRepo(t *testing.T) (*localfs.Localfs, string, string) {
	t.Helper()
	storeDir := t.TempDir()
	srcWork := t.TempDir()
	srcBare := filepath.Join(t.TempDir(), "src.git")

	mustCmd(t, "git", "init", "--bare", srcBare)
	mustCmd(t, "git", "clone", srcBare, srcWork)
	if err := os.WriteFile(filepath.Join(srcWork, "a.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustCmdIn(t, srcWork, "git", "add", ".")
	mustCmdIn(t, srcWork, "git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "init")
	mustCmdIn(t, srcWork, "git", "push", "origin", "HEAD:refs/heads/main")

	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := importer.Import(context.Background(), store, importer.Options{
		SourceDir:     srcBare,
		Tenant:        "acme",
		Repo:          "demo",
		Actor:         "test",
		DefaultBranch: "refs/heads/main",
	}); err != nil {
		t.Fatalf("Import: %v", err)
	}
	return store, "acme", "demo"
}

func TestMirror_LazyMaterializeFromExporter(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	store, tenant, repoID := makeImportedRepo(t)

	root := t.TempDir()
	mgr, err := NewManager(root, store)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	m, err := mgr.Open(context.Background(), tenant, repoID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	bare := m.BareDir()
	if _, err := os.Stat(filepath.Join(bare, "HEAD")); err != nil {
		t.Fatalf("bare repo not materialized: %v", err)
	}
	versPath := filepath.Join(root, tenant, repoID, "manifest_version.txt")
	if _, err := os.Stat(versPath); err != nil {
		t.Fatalf("manifest_version.txt not written: %v", err)
	}

	// Second open on the SAME manager: should be cached, no rebuild,
	// and return the SAME *Mirror so the per-repo mutex is shared.
	m2, err := mgr.Open(context.Background(), tenant, repoID)
	if err != nil {
		t.Fatalf("Open second: %v", err)
	}
	if m2 != m {
		t.Fatalf("Open returned a different *Mirror across calls")
	}
	if m2.BareDir() != bare {
		t.Fatalf("BareDir changed across opens: %q vs %q", m2.BareDir(), bare)
	}
}

func TestMirror_StaleDetectionRebuilds(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	store, tenant, repoID := makeImportedRepo(t)

	root := t.TempDir()
	mgr, err := NewManager(root, store)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	if _, err := mgr.Open(context.Background(), tenant, repoID); err != nil {
		t.Fatalf("first Open: %v", err)
	}
	versPath := filepath.Join(root, tenant, repoID, "manifest_version.txt")
	// Corrupt the sentinel so the next open detects mismatch.
	if err := os.WriteFile(versPath, []byte("999999"), 0o644); err != nil {
		t.Fatalf("corrupt sentinel: %v", err)
	}
	if _, err := mgr.Open(context.Background(), tenant, repoID); err != nil {
		t.Fatalf("second Open after sentinel mismatch: %v", err)
	}
	got, err := os.ReadFile(versPath)
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	if string(got) == "999999" {
		t.Fatalf("sentinel not rewritten after rebuild")
	}
}

func TestMirror_RejectsBadTenantOrRepo(t *testing.T) {
	root := t.TempDir()
	store, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	mgr, err := NewManager(root, store)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	long := strings.Repeat("a", 129) // exceeds maxNameLen
	for _, bad := range []string{"", ".", "..", "../etc", "with space", "a/b", "a..b", long} {
		if _, err := mgr.Open(context.Background(), bad, "ok"); err == nil {
			t.Fatalf("Open tenant=%q: expected error", bad)
		}
		if _, err := mgr.Open(context.Background(), "ok", bad); err == nil {
			t.Fatalf("Open repo=%q: expected error", bad)
		}
	}
}

// TestMirror_NoDirsForRepoLayerRejectedNames covers names that pass
// the URL-routing-layer regex (allows '.') but are rejected by
// internal/repo/keys.validID. Manager.Open must not leave a mirror
// directory tree behind for such names.
func TestMirror_NoDirsForRepoLayerRejectedNames(t *testing.T) {
	root := t.TempDir()
	store, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	mgr, err := NewManager(root, store)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	cases := []struct {
		tenant, repoID string
	}{
		{"acme.prod", "ok"},  // tenant has '.', rejected by keys.validID
		{"acme", "demo.git"}, // repoID has '.'
	}
	for _, c := range cases {
		if _, err := mgr.Open(context.Background(), c.tenant, c.repoID); err == nil {
			t.Fatalf("Open(%q,%q): expected error", c.tenant, c.repoID)
		}
		if _, err := os.Stat(filepath.Join(root, c.tenant, c.repoID)); !os.IsNotExist(err) {
			t.Fatalf("mirror dirs leaked for (%q,%q): stat err = %v", c.tenant, c.repoID, err)
		}
	}
}

// sentinel records the same ManifestVersion as the bucket but a different
// LatestTx. Same-version replacement (repo deleted+recreated, restored
// from backup, swapped from another bucket) must force a rebuild rather
// than serving the cached bare repo.
func TestMirror_StaleDetectionDifferentLatestTx(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	store, tenant, repoID := makeImportedRepo(t)

	root := t.TempDir()
	mgr, err := NewManager(root, store)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	m, err := mgr.Open(context.Background(), tenant, repoID)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	original, err := readSentinel(m.VersionFile())
	if err != nil {
		t.Fatalf("readSentinel: %v", err)
	}
	// Same numeric version, different LatestTx — must be detected stale.
	tampered := sentinel{
		ManifestVersion: original.ManifestVersion,
		LatestTx:        "tx_FAKE_DIFFERENT_VALUE",
	}
	if err := writeSentinel(m.VersionFile(), tampered); err != nil {
		t.Fatalf("writeSentinel: %v", err)
	}
	if _, err := mgr.Open(context.Background(), tenant, repoID); err != nil {
		t.Fatalf("second Open after tx mismatch: %v", err)
	}
	got, err := readSentinel(m.VersionFile())
	if err != nil {
		t.Fatalf("readSentinel after rebuild: %v", err)
	}
	if got.LatestTx == tampered.LatestTx {
		t.Fatalf("sentinel not rewritten after LatestTx mismatch: got %+v", got)
	}
	if got != original {
		t.Fatalf("sentinel after rebuild: got %+v want %+v", got, original)
	}
}
