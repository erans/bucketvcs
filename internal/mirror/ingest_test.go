package mirror

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIngestPack_CopiesAndUpdatesRefs(t *testing.T) {
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

	// Build a new pack inside a clone of the mirror by adding one more commit.
	bare := m.BareDir()
	work := filepath.Join(t.TempDir(), "wt")
	mustCmd(t, "git", "clone", bare, work)
	if err := os.WriteFile(filepath.Join(work, "b.txt"), []byte("more\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustCmdIn(t, work, "git", "add", ".")
	mustCmdIn(t, work, "git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "more")
	newOID := strings.TrimSpace(string(mustCmdCapture(t, work, "git", "rev-parse", "HEAD")))
	oldOID := strings.TrimSpace(string(mustCmdCapture(t, bare, "git", "rev-parse", "refs/heads/main")))

	// Pack only the new commit (since oldOID).
	tmpPrefix := filepath.Join(t.TempDir(), "newpack")
	mustCmdIn(t, work, "bash", "-c", "git rev-list "+newOID+" ^"+oldOID+" | git pack-objects "+tmpPrefix)
	matches, err := filepath.Glob(tmpPrefix + "-*.pack")
	if err != nil || len(matches) != 1 {
		t.Fatalf("pack-objects produced %v err=%v", matches, err)
	}
	packPath := matches[0]
	// pack-objects also produces the .idx alongside, so no separate IndexPack
	// step needed here — verify it exists.
	idxPath := strings.TrimSuffix(packPath, ".pack") + ".idx"
	if _, err := os.Stat(idxPath); err != nil {
		t.Fatalf("idx not produced by pack-objects: %v", err)
	}

	m.Lock()
	defer m.Unlock()
	updates := []RefUpdate{{Refname: "refs/heads/main", OldOID: oldOID, NewOID: newOID}}
	if err := m.IngestPack(context.Background(), packPath, updates, 99, "01TESTTESTTESTTESTTESTTESTT"); err != nil {
		t.Fatalf("IngestPack: %v", err)
	}

	got := strings.TrimSpace(string(mustCmdCapture(t, bare, "git", "rev-parse", "refs/heads/main")))
	if got != newOID {
		t.Fatalf("ref not updated: got %s, want %s", got, newOID)
	}
	v, err := m.CurrentVersion()
	if err != nil || v != "99" {
		t.Fatalf("sentinel: got %q err=%v", v, err)
	}
}

func TestIngestPack_DeleteRef(t *testing.T) {
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
	mainOID := strings.TrimSpace(string(mustCmdCapture(t, bare, "git", "rev-parse", "refs/heads/main")))
	mustCmdIn(t, bare, "git", "update-ref", "refs/heads/doomed", mainOID)

	m.Lock()
	defer m.Unlock()
	if err := m.IngestPack(context.Background(), "", []RefUpdate{{
		Refname: "refs/heads/doomed",
		OldOID:  mainOID,
		NewOID:  "0000000000000000000000000000000000000000",
	}}, 100, "01TESTTESTTESTTESTTESTTESTT"); err != nil {
		t.Fatalf("IngestPack delete: %v", err)
	}
	out, err := mustCmdCaptureNoFail(t, bare, "git", "rev-parse", "--verify", "refs/heads/doomed")
	if err == nil {
		t.Fatalf("ref still present: %s", out)
	}
}

// mustCmdCaptureNoFail runs a command in dir and returns its combined
// output and the error (without failing the test). Useful for assertions
// that expect the command to fail (e.g. `git rev-parse --verify` of a
// deleted ref).
func mustCmdCaptureNoFail(t *testing.T, dir string, args ...string) ([]byte, error) {
	t.Helper()
	return runCmd(dir, args[0], args[1:]...)
}
