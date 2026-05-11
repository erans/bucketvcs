package maintenance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestGenerateBundleArtifact_Success(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()

	// Build a tiny bare repo with one commit on main.
	bareDir := filepath.Join(dir, "mirror.git")
	if err := exec.Command("git", "init", "--bare", "-b", "main", bareDir).Run(); err != nil {
		t.Fatal(err)
	}
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

	// Resolve the tip OID for the assertion.
	tipBytes, _ := exec.Command("git", "-C", workDir, "rev-parse", "HEAD").Output()
	tipOID := strings.TrimSpace(string(tipBytes))

	// Localfs storage for the upload.
	bucketRoot := filepath.Join(dir, "bucket")
	store, err := localfs.Open(bucketRoot)
	if err != nil {
		t.Fatal(err)
	}
	rkeys, err := keys.NewRepo("ten", "rep")
	if err != nil {
		t.Fatal(err)
	}

	art, err := GenerateBundleArtifact(context.Background(), bareDir, "refs/heads/main", store, rkeys, 7, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("GenerateBundleArtifact: %v", err)
	}
	if art.Entry.Kind != "full_default" {
		t.Errorf("Kind = %q, want full_default", art.Entry.Kind)
	}
	if art.Entry.Ref != "refs/heads/main" {
		t.Errorf("Ref = %q", art.Entry.Ref)
	}
	if art.Entry.TipOID != tipOID {
		t.Errorf("TipOID = %q, want %q", art.Entry.TipOID, tipOID)
	}
	if art.Entry.CoversManifestVersion != 7 {
		t.Errorf("CoversManifestVersion = %d, want 7", art.Entry.CoversManifestVersion)
	}
	if art.Entry.ByteSize == 0 {
		t.Error("ByteSize == 0")
	}
	if !strings.HasPrefix(art.Entry.BundleHash, "sha256-") {
		t.Errorf("BundleHash %q lacks sha256- prefix", art.Entry.BundleHash)
	}
	if art.LocalBundle == "" {
		t.Error("LocalBundle empty")
	}
	if _, err := os.Stat(art.LocalBundle); err != nil {
		t.Errorf("LocalBundle stat: %v", err)
	}
	if art.LocalDir == "" || filepath.Dir(art.LocalBundle) != art.LocalDir {
		t.Errorf("LocalDir = %q, want parent of LocalBundle %q", art.LocalDir, art.LocalBundle)
	}
	t.Cleanup(func() { _ = os.RemoveAll(art.LocalDir) })

	// Bundle blob should be uploaded under BundleKey and its content
	// should hash to BundleHash.
	blobObj, err := store.Get(context.Background(), art.Entry.BundleKey, nil)
	if err != nil {
		t.Fatalf("Get bundle blob: %v", err)
	}
	h := sha256.New()
	if _, err := io.Copy(h, blobObj.Body); err != nil {
		blobObj.Body.Close()
		t.Fatalf("hash blob: %v", err)
	}
	blobObj.Body.Close()
	gotHash := "sha256-" + hex.EncodeToString(h.Sum(nil))
	if gotHash != art.Entry.BundleHash {
		t.Errorf("uploaded blob hash = %q, want %q", gotHash, art.Entry.BundleHash)
	}

	// Sidecar JSON should round-trip with all fields preserved.
	obj, err := store.Get(context.Background(), art.Entry.SidecarKey, nil)
	if err != nil {
		t.Fatalf("Get sidecar: %v", err)
	}
	sidecarBytes, _ := io.ReadAll(obj.Body)
	obj.Body.Close()
	var got manifest.BundleEntry
	if err := json.Unmarshal(sidecarBytes, &got); err != nil {
		t.Fatalf("sidecar JSON: %v\n%s", err, sidecarBytes)
	}
	if got != art.Entry {
		t.Errorf("sidecar round-trip mismatch:\n got=%+v\nwant=%+v", got, art.Entry)
	}
}

// TestGenerateBundleArtifact_RefMissing_Errors verifies that a ref that
// does not exist in the mirror causes rev-parse to fail and the function
// to return an error without producing partial sidecar or bundle uploads.
func TestGenerateBundleArtifact_RefMissing_Errors(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	bareDir := filepath.Join(dir, "mirror.git")
	if err := exec.Command("git", "init", "--bare", "-b", "main", bareDir).Run(); err != nil {
		t.Fatal(err)
	}
	bucketRoot := filepath.Join(dir, "bucket")
	store, err := localfs.Open(bucketRoot)
	if err != nil {
		t.Fatal(err)
	}
	rkeys, err := keys.NewRepo("ten", "rep")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := GenerateBundleArtifact(context.Background(), bareDir, "refs/heads/nope", store, rkeys, 1, time.Now()); err == nil {
		t.Fatal("expected error for missing ref")
	} else if !strings.Contains(err.Error(), "bundle: rev-parse") {
		t.Fatalf("expected error to be pinned at rev-parse step; got %q", err.Error())
	}

	// No partial uploads under the repo's bundles/ prefix.
	page, err := store.List(context.Background(), rkeys.Prefix()+"bundles/", nil)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("List after failure: %v", err)
	}
	if page != nil && len(page.Objects) > 0 {
		t.Fatalf("expected zero objects under bundles/ after failed run; got %d", len(page.Objects))
	}
}

// TestGenerateBundleArtifact_IdempotentReUpload verifies that running
// GenerateBundleArtifact twice against the same mirror+ref+store
// succeeds the second time even though both bundle and sidecar keys
// already exist (the function swallows ErrAlreadyExists for both).
// The blob stored under the content-addressed key must remain
// byte-identical to the first run; the sidecar from the first run
// wins (documented contract).
func TestGenerateBundleArtifact_IdempotentReUpload(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()

	bareDir := filepath.Join(dir, "mirror.git")
	if err := exec.Command("git", "init", "--bare", "-b", "main", bareDir).Run(); err != nil {
		t.Fatal(err)
	}
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

	bucketRoot := filepath.Join(dir, "bucket")
	store, err := localfs.Open(bucketRoot)
	if err != nil {
		t.Fatal(err)
	}
	rkeys, err := keys.NewRepo("ten", "rep")
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	first, err := GenerateBundleArtifact(ctx, bareDir, "refs/heads/main", store, rkeys, 7, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("first GenerateBundleArtifact: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(first.LocalDir) })

	// Snapshot the blob bytes after the first run.
	obj1, err := store.Get(ctx, first.Entry.BundleKey, nil)
	if err != nil {
		t.Fatalf("Get blob after first run: %v", err)
	}
	blob1, _ := io.ReadAll(obj1.Body)
	obj1.Body.Close()

	// Second run at a different manifestVersion and GeneratedAt — both
	// keys already exist; the function must succeed and return a fresh
	// BundleEntry whose content-addressed keys match the first run.
	second, err := GenerateBundleArtifact(ctx, bareDir, "refs/heads/main", store, rkeys, 9, time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("second GenerateBundleArtifact: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(second.LocalDir) })

	if second.Entry.BundleKey != first.Entry.BundleKey {
		t.Errorf("BundleKey changed across runs: %q vs %q", second.Entry.BundleKey, first.Entry.BundleKey)
	}
	if second.Entry.SidecarKey != first.Entry.SidecarKey {
		t.Errorf("SidecarKey changed across runs: %q vs %q", second.Entry.SidecarKey, first.Entry.SidecarKey)
	}
	if second.Entry.BundleHash != first.Entry.BundleHash {
		t.Errorf("BundleHash changed across runs: %q vs %q", second.Entry.BundleHash, first.Entry.BundleHash)
	}
	// New BundleEntry reflects the new manifestVersion.
	if second.Entry.CoversManifestVersion != 9 {
		t.Errorf("second CoversManifestVersion = %d, want 9", second.Entry.CoversManifestVersion)
	}

	// Blob bytes are unchanged.
	obj2, err := store.Get(ctx, first.Entry.BundleKey, nil)
	if err != nil {
		t.Fatalf("Get blob after second run: %v", err)
	}
	blob2, _ := io.ReadAll(obj2.Body)
	obj2.Body.Close()
	if !bytes.Equal(blob1, blob2) {
		t.Error("blob content changed across runs; expected byte-identical content-addressed object")
	}

	// Sidecar from the first run wins (documented contract): the
	// CoversManifestVersion / GeneratedAt / ID stored on disk reflect
	// the first generation, NOT the second.
	sidecarObj, err := store.Get(ctx, first.Entry.SidecarKey, nil)
	if err != nil {
		t.Fatalf("Get sidecar after second run: %v", err)
	}
	sidecarBytes, _ := io.ReadAll(sidecarObj.Body)
	sidecarObj.Body.Close()
	var storedSidecar manifest.BundleEntry
	if err := json.Unmarshal(sidecarBytes, &storedSidecar); err != nil {
		t.Fatalf("unmarshal stored sidecar: %v", err)
	}
	if storedSidecar.CoversManifestVersion != first.Entry.CoversManifestVersion {
		t.Errorf("sidecar CoversManifestVersion = %d, want %d (first-run snapshot)",
			storedSidecar.CoversManifestVersion, first.Entry.CoversManifestVersion)
	}
	if storedSidecar.GeneratedAt != first.Entry.GeneratedAt {
		t.Errorf("sidecar GeneratedAt = %q, want %q (first-run snapshot)",
			storedSidecar.GeneratedAt, first.Entry.GeneratedAt)
	}
	if storedSidecar.ID != first.Entry.ID {
		t.Errorf("sidecar ID = %q, want %q (first-run snapshot)",
			storedSidecar.ID, first.Entry.ID)
	}
}

// runIn is a local test helper: run a git (or other) command in dir; t.Fatal on failure.
func runIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}
