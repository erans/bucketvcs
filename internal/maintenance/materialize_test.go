package maintenance

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	mtest "github.com/bucketvcs/bucketvcs/internal/maintenance/mtest"
	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestDownloadPack_StreamsPackAndIdxToBareDir(t *testing.T) {
	storeDir := t.TempDir()
	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	packKey := "tenants/acme/repos/site/packs/canonical/abc.pack"
	idxKey := "tenants/acme/repos/site/packs/canonical/abc.idx"
	if _, err := store.PutIfAbsent(ctx, packKey, bytes.NewReader([]byte("PACKBYTES")), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutIfAbsent(ctx, idxKey, bytes.NewReader([]byte("IDXBYTES")), nil); err != nil {
		t.Fatal(err)
	}

	bareDir := filepath.Join(t.TempDir(), "bare.git", "objects", "pack")
	if err := os.MkdirAll(bareDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gotPack, gotIdx, err := downloadPack(ctx, store, packKey, idxKey, bareDir)
	if err != nil {
		t.Fatalf("downloadPack: %v", err)
	}
	pb, err := os.ReadFile(gotPack)
	if err != nil {
		t.Fatal(err)
	}
	ib, err := os.ReadFile(gotIdx)
	if err != nil {
		t.Fatal(err)
	}
	if string(pb) != "PACKBYTES" {
		t.Errorf("pack bytes = %q", pb)
	}
	if string(ib) != "IDXBYTES" {
		t.Errorf("idx bytes = %q", ib)
	}
	// Both files must share the same basename root (git's pack-N convention).
	pbase := filepath.Base(gotPack)
	ibase := filepath.Base(gotIdx)
	if pbase[:len(pbase)-len(".pack")] != ibase[:len(ibase)-len(".idx")] {
		t.Errorf("pack/idx basenames don't match: %s vs %s", gotPack, gotIdx)
	}
}

func TestMaterialize_BuildsBareRepoThatFscks(t *testing.T) {
	mtest.GitAvailable(t)

	// Build a real source bare repo with a single commit reachable from main.
	src := t.TempDir()
	mtest.MustRunGit(t, src, "init", "--bare")
	mtest.MustRunGit(t, src, "config", "user.email", "test@example.com")
	mtest.MustRunGit(t, src, "config", "user.name", "T")
	wt := t.TempDir()
	mtest.MustRunGit(t, wt, "init")
	mtest.MustRunGit(t, wt, "config", "user.email", "test@example.com")
	mtest.MustRunGit(t, wt, "config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(wt, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mtest.MustRunGit(t, wt, "add", ".")
	mtest.MustRunGit(t, wt, "commit", "-m", "init")
	mtest.MustRunGit(t, wt, "remote", "add", "origin", src)
	mtest.MustRunGit(t, wt, "push", "origin", "HEAD:refs/heads/main")

	// Pack-objects-all the source bare → tmp/out/pack-<id>.{pack,idx}
	prefix := filepath.Join(t.TempDir(), "out", "pack")
	if err := os.MkdirAll(filepath.Dir(prefix), 0o755); err != nil {
		t.Fatal(err)
	}
	packID, err := gitcli.PackObjectsAll(context.Background(), src, prefix)
	if err != nil {
		t.Fatalf("PackObjectsAll: %v", err)
	}
	srcPack := prefix + "-" + packID + ".pack"
	srcIdx := prefix + "-" + packID + ".idx"

	// Build a localfs store and upload the pack/idx under a canonical key.
	storeDir := t.TempDir()
	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	packKey := "tenants/acme/repos/site/packs/canonical/" + packID + ".pack"
	idxKey := "tenants/acme/repos/site/packs/canonical/" + packID + ".idx"
	mtest.UploadFile(t, store, srcPack, packKey)
	mtest.UploadFile(t, store, srcIdx, idxKey)

	// Resolve the main branch OID.
	out := mtest.MustGitOutput(t, src, "rev-parse", "refs/heads/main")
	headOID := strings.TrimSpace(out)

	bareDir := t.TempDir()
	if err := Materialize(context.Background(), store, MaterializeInput{
		BareDir:       bareDir,
		Packs:         []PackRef{{PackKey: packKey, IdxKey: idxKey}},
		Refs:          map[string]string{"refs/heads/main": headOID},
		DefaultBranch: "main",
	}); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	// Verify fsck-clean.
	cmd := exec.Command("git", "--git-dir="+filepath.Join(bareDir, "bare.git"), "fsck", "--full")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git fsck failed: %v\n%s", err, out)
	}
}
