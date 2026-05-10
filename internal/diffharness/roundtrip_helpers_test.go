package diffharness

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/bucketvcs/bucketvcs/internal/diffharness/fixtures"
	"github.com/bucketvcs/bucketvcs/internal/exporter"
	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/importer"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func newTestStore(t *testing.T) storage.ObjectStore {
	t.Helper()
	if url := os.Getenv("BUCKETVCS_DIFFHARNESS_STORE"); url != "" {
		// For cloud schemes, append a unique UUID sub-prefix so that each
		// test gets an isolated namespace and re-runs don't collide on the
		// same fixed repo names (tenant="diff", repo=<fixture>).
		storeURL := url
		if !strings.HasPrefix(url, "localfs:") {
			// Ensure trailing slash before appending the UUID segment.
			if !strings.HasSuffix(storeURL, "/") {
				storeURL += "/"
			}
			storeURL += uuid.New().String() + "/"
		}
		s, err := openStoreFromURL(t, storeURL)
		if err != nil {
			t.Fatalf("BUCKETVCS_DIFFHARNESS_STORE=%q: %v", url, err)
		}
		t.Cleanup(func() { _ = closeStore(s) })
		return s
	}
	s, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// ImportThenExportAndCompare runs the §5.1 round-trip oracle for a fixture.
func ImportThenExportAndCompare(t *testing.T, name string, build fixtures.Builder) {
	t.Helper()
	srcDir := filepath.Join(t.TempDir(), "src")
	fx := build(t, srcDir)
	if fx.Name != name {
		t.Fatalf("fixture name mismatch")
	}
	gitFsck(t, srcDir)

	store := newTestStore(t)
	// For the "empty" fixture the importer's DefaultBranch validation
	// requires a real refs/* name; supply refs/heads/main so it passes.
	defaultBranch := ""
	if len(fx.Refs) == 0 {
		defaultBranch = "refs/heads/main"
	}
	if _, err := importer.Import(context.Background(), store, importer.Options{
		SourceDir: srcDir, Tenant: "diff", Repo: name, Actor: "harness",
		DefaultBranch: defaultBranch,
	}); err != nil {
		t.Fatalf("Import: %v", err)
	}

	dstDir := filepath.Join(t.TempDir(), "out")
	if _, err := exporter.Export(context.Background(), store, exporter.Options{
		Tenant: "diff", Repo: name, DestDir: dstDir,
	}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	gitFsck(t, dstDir)

	srcRefs := gitShowRef(t, srcDir)
	dstRefs := gitShowRef(t, dstDir)
	if !equalRefs(srcRefs, dstRefs) {
		t.Fatalf("refs differ.\nsrc=%v\ndst=%v", srcRefs, dstRefs)
	}
	srcHead, errS := gitcli.SymbolicRef(context.Background(), srcDir, "HEAD")
	dstHead, errD := gitcli.SymbolicRef(context.Background(), dstDir, "HEAD")
	if (errS == nil) != (errD == nil) {
		t.Fatalf("HEAD presence differs: src err=%v, dst err=%v", errS, errD)
	}
	if errS == nil && srcHead != dstHead {
		t.Fatalf("HEAD differs: src=%q dst=%q", srcHead, dstHead)
	}
	srcOIDs := gitRevListAllObjects(t, srcDir)
	dstOIDs := gitRevListAllObjects(t, dstDir)
	if !equalOIDLists(srcOIDs, dstOIDs) {
		t.Fatalf("reachable OIDs differ.\nsrc=%v\ndst=%v", srcOIDs, dstOIDs)
	}
	for _, oid := range srcOIDs {
		got := gitCatFilePretty(t, dstDir, oid)
		want := gitCatFilePretty(t, srcDir, oid)
		ensureBytesEqual(t, "cat-file -p "+oid, got, want)
	}
}

// CatObjectOracle runs the §5.1 pack-reader oracle: every reachable OID
// in the source repo, after import, must produce identical cat-object
// output to upstream git.
func CatObjectOracle(t *testing.T, name string, build fixtures.Builder) {
	t.Helper()
	srcDir := filepath.Join(t.TempDir(), "src")
	fx := build(t, srcDir)
	if len(fx.AllOIDs) == 0 {
		// Empty repo: nothing to compare.
		return
	}
	store := newTestStore(t)
	defaultBranch := ""
	if len(fx.Refs) == 0 {
		defaultBranch = "refs/heads/main"
	}
	if _, err := importer.Import(context.Background(), store, importer.Options{
		SourceDir: srcDir, Tenant: "diff", Repo: name, Actor: "harness",
		DefaultBranch: defaultBranch,
	}); err != nil {
		t.Fatalf("Import: %v", err)
	}
	for _, oid := range fx.AllOIDs {
		// --pretty
		got, err := CatObject(context.Background(), store, "diff", name, oid, CatPretty)
		if err != nil {
			t.Fatalf("CatObject pretty %s: %v", oid, err)
		}
		want := gitCatFilePretty(t, srcDir, oid)
		ensureBytesEqual(t, "bv cat-object --pretty "+oid, got, want)

		// --type
		gotTypeBytes, err := CatObject(context.Background(), store, "diff", name, oid, CatType)
		if err != nil {
			t.Fatalf("CatObject type %s: %v", oid, err)
		}
		gotType := strings.TrimSpace(string(gotTypeBytes))
		wantType := gitCatFileType(t, srcDir, oid)
		if gotType != wantType {
			t.Fatalf("type %s: bv=%q git=%q", oid, gotType, wantType)
		}

		// --size
		gotSizeBytes, err := CatObject(context.Background(), store, "diff", name, oid, CatSize)
		if err != nil {
			t.Fatalf("CatObject size %s: %v", oid, err)
		}
		gotSize := strings.TrimSpace(string(gotSizeBytes))
		wantSize := strconv.FormatInt(gitCatFileSize(t, srcDir, oid), 10)
		if gotSize != wantSize {
			t.Fatalf("size %s: bv=%q git=%q", oid, gotSize, wantSize)
		}
	}
}
