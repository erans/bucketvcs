// Package rtest provides test fixtures for the reachability package.
// All exports are intended for _test.go files only.
package rtest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/commitgraph"
	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/objindex"
	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/reachability/deltaindex"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// Fixture holds a fully prepared test repo in an in-memory localfs store.
type Fixture struct {
	Store storage.ObjectStore
	Keys  *keys.Repo
	Body  manifest.Body

	// Commit OIDs for the chain. A is the root; each subsequent commit
	// has the prior one as parent. D is only present for "with delta"
	// fixtures (zero-value for base-only).
	A, B, C, D pack.OID
}

// gitAvailable skips t if git is not on PATH.
func gitAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

// mustGit runs git in dir; t.Fatal on failure.
func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// mustGitOutput runs git in dir and returns trimmed stdout; t.Fatal on failure.
func mustGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// mustParseOID converts a hex string to pack.OID; t.Fatal on failure.
func mustParseOID(t *testing.T, hex string) pack.OID {
	t.Helper()
	oid, err := pack.ParseOID(hex)
	if err != nil {
		t.Fatalf("ParseOID(%q): %v", hex, err)
	}
	return oid
}

// setup3CommitRepo creates a bare git repo with 3 commits (A→B→C) on
// refs/heads/main and returns:
//   - bare dir path
//   - OIDs A, B, C (oldest to newest)
func setup3CommitRepo(t *testing.T) (bareDir string, a, b, c pack.OID) {
	t.Helper()
	// Create a bare repo as the "remote".
	bare := t.TempDir()
	mustGit(t, bare, "init", "--bare")
	mustGit(t, bare, "config", "user.email", "test@example.com")
	mustGit(t, bare, "config", "user.name", "T")

	// Create a working tree, make 3 commits, push.
	wt := t.TempDir()
	mustGit(t, wt, "init")
	mustGit(t, wt, "config", "user.email", "test@example.com")
	mustGit(t, wt, "config", "user.name", "T")

	// Commit A.
	if err := os.WriteFile(filepath.Join(wt, "a.txt"), []byte("commit-A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, wt, "add", "a.txt")
	mustGit(t, wt, "-c", "user.name=T", "-c", "user.email=test@example.com",
		"commit", "-m", "A")
	oidA := mustGitOutput(t, wt, "rev-parse", "HEAD")

	// Commit B.
	if err := os.WriteFile(filepath.Join(wt, "b.txt"), []byte("commit-B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, wt, "add", "b.txt")
	mustGit(t, wt, "-c", "user.name=T", "-c", "user.email=test@example.com",
		"commit", "-m", "B")
	oidB := mustGitOutput(t, wt, "rev-parse", "HEAD")

	// Commit C.
	if err := os.WriteFile(filepath.Join(wt, "c.txt"), []byte("commit-C\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, wt, "add", "c.txt")
	mustGit(t, wt, "-c", "user.name=T", "-c", "user.email=test@example.com",
		"commit", "-m", "C")
	oidC := mustGitOutput(t, wt, "rev-parse", "HEAD")

	// Push to bare.
	mustGit(t, wt, "remote", "add", "origin", bare)
	mustGit(t, wt, "push", "origin", "HEAD:refs/heads/main")
	mustGit(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")

	return bare, mustParseOID(t, oidA), mustParseOID(t, oidB), mustParseOID(t, oidC)
}

// buildBaseIndexes packs the bare repo and builds .bvom + .bvcg bytes.
// Returns (packID, bvomBytes, bvomHash, bvcgBytes, bvcgHash, packPath, idxPath).
func buildBaseIndexes(t *testing.T, ctx context.Context, bare string, refs map[string]string) (
	packID string, bvom []byte, bvomHash string, bvcg []byte, bvcgHash string, packPath, idxPath string,
) {
	t.Helper()
	outDir := t.TempDir()
	prefix := filepath.Join(outDir, "pack")
	var err error
	packID, err = gitcli.PackObjectsAll(ctx, bare, prefix)
	if err != nil {
		t.Fatalf("PackObjectsAll: %v", err)
	}
	packPath = prefix + "-" + packID + ".pack"
	idxPath = prefix + "-" + packID + ".idx"

	// Build .bvom.
	localStore := newLocalFileStore(t, packPath, idxPath)
	r, err := pack.Open(ctx, localStore, "p.pack", "p.idx")
	if err != nil {
		t.Fatalf("pack.Open: %v", err)
	}
	defer r.Close()

	bvom, err = objindex.Build(r, packID)
	if err != nil {
		t.Fatalf("objindex.Build: %v", err)
	}
	sum := sha256.Sum256(bvom)
	bvomHash = hex.EncodeToString(sum[:])

	// Build tips from refs.
	tips := make([]commitgraph.Tip, 0, len(refs))
	for refName, oidStr := range refs {
		oid, e := pack.ParseOID(oidStr)
		if e != nil {
			t.Fatalf("ParseOID: %v", e)
		}
		tips = append(tips, commitgraph.Tip{Ref: refName, OID: oid})
	}

	bvcg, err = commitgraph.Build(ctx, r, tips)
	if err != nil {
		t.Fatalf("commitgraph.Build: %v", err)
	}
	sum = sha256.Sum256(bvcg)
	bvcgHash = hex.EncodeToString(sum[:])

	return
}

// uploadBytes stores b under key in the store.
func uploadBytes(t *testing.T, ctx context.Context, store storage.ObjectStore, b []byte, key string) {
	t.Helper()
	if _, err := store.PutIfAbsent(ctx, key, bytes.NewReader(b), nil); err != nil {
		t.Fatalf("PutIfAbsent(%s): %v", key, err)
	}
}

// uploadFile stores the file at srcPath under key in the store.
func uploadFile(t *testing.T, ctx context.Context, store storage.ObjectStore, srcPath, key string) {
	t.Helper()
	f, err := os.Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := store.PutIfAbsent(ctx, key, f, nil); err != nil {
		t.Fatalf("PutIfAbsent(%s): %v", key, err)
	}
}

// NewBaseOnlyRepo builds a 3-commit chain A→B→C. The base index pair
// (.bvom + .bvcg) is uploaded; no reachability deltas. refs/heads/main
// points at C.
func NewBaseOnlyRepo(t *testing.T) Fixture {
	t.Helper()
	gitAvailable(t)
	ctx := context.Background()

	bareDir, a, b, c := setup3CommitRepo(t)

	refs := map[string]string{
		"refs/heads/main": c.String(),
	}

	packID, bvom, bvomHash, bvcg, bvcgHash, packPath, idxPath := buildBaseIndexes(t, ctx, bareDir, refs)

	// Open localfs store.
	storeDir := t.TempDir()
	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	k, err := keys.NewRepo("t", "r")
	if err != nil {
		t.Fatal(err)
	}

	// Upload pack, idx, .bvom, .bvcg.
	uploadFile(t, ctx, store, packPath, k.CanonicalPackKey(packID))
	uploadFile(t, ctx, store, idxPath, k.PackIdxKey(packID, "canonical"))
	bvomKey := k.ObjectMapKey(bvomHash)
	uploadBytes(t, ctx, store, bvom, bvomKey)
	bvcgKey := k.CommitGraphKey(bvcgHash)
	uploadBytes(t, ctx, store, bvcg, bvcgKey)

	body := manifest.Body{
		DefaultBranch: "refs/heads/main",
		Refs:          refs,
		Indexes: manifest.Indexes{
			ObjectMap:   &manifest.IndexRef{Key: bvomKey, Hash: bvomHash},
			CommitGraph: &manifest.IndexRef{Key: bvcgKey, Hash: bvcgHash},
		},
	}

	return Fixture{Store: store, Keys: k, Body: body, A: a, B: b, C: c}
}

// NewBaseWithDeltaRepo builds a base-only repo (A→B→C) then layers a
// single .bvrd delta containing commit D (parent=C, gen=4) and a
// ref-tip update main: C → D.
func NewBaseWithDeltaRepo(t *testing.T) Fixture {
	t.Helper()
	gitAvailable(t)
	ctx := context.Background()

	bareDir, a, b, c := setup3CommitRepo(t)

	refs := map[string]string{
		"refs/heads/main": c.String(),
	}

	packID, bvom, bvomHash, bvcg, bvcgHash, packPath, idxPath := buildBaseIndexes(t, ctx, bareDir, refs)

	// Add a 4th commit D to the bare repo but DON'T repack. We'll put D
	// only in the delta (representing a push that arrived after the base).
	wt2 := t.TempDir()
	mustGit(t, wt2, "init")
	mustGit(t, wt2, "config", "user.email", "test@example.com")
	mustGit(t, wt2, "config", "user.name", "T")
	mustGit(t, wt2, "remote", "add", "origin", bareDir)
	mustGit(t, wt2, "fetch", "origin")
	mustGit(t, wt2, "checkout", "-b", "main", "origin/main")
	if err := os.WriteFile(filepath.Join(wt2, "d.txt"), []byte("commit-D\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, wt2, "add", "d.txt")
	mustGit(t, wt2, "-c", "user.name=T", "-c", "user.email=test@example.com",
		"commit", "-m", "D")
	oidDStr := mustGitOutput(t, wt2, "rev-parse", "HEAD")
	d := mustParseOID(t, oidDStr)

	// Open localfs store.
	storeDir := t.TempDir()
	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	k, err := keys.NewRepo("t", "r")
	if err != nil {
		t.Fatal(err)
	}

	// Upload base pack + indexes.
	uploadFile(t, ctx, store, packPath, k.CanonicalPackKey(packID))
	uploadFile(t, ctx, store, idxPath, k.PackIdxKey(packID, "canonical"))
	bvomKey := k.ObjectMapKey(bvomHash)
	uploadBytes(t, ctx, store, bvom, bvomKey)
	bvcgKey := k.CommitGraphKey(bvcgHash)
	uploadBytes(t, ctx, store, bvcg, bvcgKey)

	// Build and upload the delta for D.
	// Generation of D = 4 (A=1, B=2, C=3, D=4).
	delta := deltaindex.Delta{
		Commits: []deltaindex.CommitRecord{
			{OID: d, Generation: 4, Parents: []pack.OID{c}},
		},
		RefTips: []deltaindex.RefTipDiff{
			{RefName: "refs/heads/main", OldOID: c, NewOID: d},
		},
	}
	deltaBytes, err := deltaindex.Encode(delta)
	if err != nil {
		t.Fatalf("deltaindex.Encode: %v", err)
	}
	deltaSum := sha256.Sum256(deltaBytes)
	deltaHash := hex.EncodeToString(deltaSum[:])
	deltaKey := k.ReachabilityDeltaKey(deltaHash)
	uploadBytes(t, ctx, store, deltaBytes, deltaKey)

	body := manifest.Body{
		DefaultBranch: "refs/heads/main",
		Refs:          refs,
		Indexes: manifest.Indexes{
			ObjectMap:   &manifest.IndexRef{Key: bvomKey, Hash: bvomHash},
			CommitGraph: &manifest.IndexRef{Key: bvcgKey, Hash: bvcgHash},
			Reachability: &manifest.ReachabilityRef{
				BaseManifest: "v2",
				Deltas: []manifest.IndexRef{
					{Key: deltaKey, Hash: deltaHash},
				},
			},
		},
	}

	return Fixture{Store: store, Keys: k, Body: body, A: a, B: b, C: c, D: d}
}

// NewShadowedFixture builds a base-only repo (A→B→C) then layers a
// delta that re-introduces A with generation=99, to test shadow
// semantics (latest delta wins for lookup).
func NewShadowedFixture(t *testing.T) Fixture {
	t.Helper()
	gitAvailable(t)
	ctx := context.Background()

	bareDir, a, b, c := setup3CommitRepo(t)

	refs := map[string]string{
		"refs/heads/main": c.String(),
	}

	packID, bvom, bvomHash, bvcg, bvcgHash, packPath, idxPath := buildBaseIndexes(t, ctx, bareDir, refs)

	storeDir := t.TempDir()
	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	k, err := keys.NewRepo("t", "r")
	if err != nil {
		t.Fatal(err)
	}

	uploadFile(t, ctx, store, packPath, k.CanonicalPackKey(packID))
	uploadFile(t, ctx, store, idxPath, k.PackIdxKey(packID, "canonical"))
	bvomKey := k.ObjectMapKey(bvomHash)
	uploadBytes(t, ctx, store, bvom, bvomKey)
	bvcgKey := k.CommitGraphKey(bvcgHash)
	uploadBytes(t, ctx, store, bvcg, bvcgKey)

	// Delta that "shadows" A with an impossible generation of 99.
	delta := deltaindex.Delta{
		Commits: []deltaindex.CommitRecord{
			{OID: a, Generation: 99, Parents: nil},
		},
	}
	deltaBytes, err := deltaindex.Encode(delta)
	if err != nil {
		t.Fatalf("deltaindex.Encode: %v", err)
	}
	deltaSum := sha256.Sum256(deltaBytes)
	deltaHash := hex.EncodeToString(deltaSum[:])
	deltaKey := k.ReachabilityDeltaKey(deltaHash)
	uploadBytes(t, ctx, store, deltaBytes, deltaKey)

	body := manifest.Body{
		DefaultBranch: "refs/heads/main",
		Refs:          refs,
		Indexes: manifest.Indexes{
			ObjectMap:   &manifest.IndexRef{Key: bvomKey, Hash: bvomHash},
			CommitGraph: &manifest.IndexRef{Key: bvcgKey, Hash: bvcgHash},
			Reachability: &manifest.ReachabilityRef{
				BaseManifest: "v2",
				Deltas: []manifest.IndexRef{
					{Key: deltaKey, Hash: deltaHash},
				},
			},
		},
	}

	return Fixture{Store: store, Keys: k, Body: body, A: a, B: b, C: c}
}

// localFileStore is a minimal read-only ObjectStore backed by two files.
// It is a local copy of importer's localFilePackStore to avoid a
// dependency on the importer package from test helpers.
type localFileStore struct {
	packPath, idxPath string
}

func newLocalFileStore(t *testing.T, packPath, idxPath string) *localFileStore {
	t.Helper()
	return &localFileStore{packPath: packPath, idxPath: idxPath}
}

func (s *localFileStore) pathFor(key string) (string, error) {
	switch key {
	case "p.pack":
		return s.packPath, nil
	case "p.idx":
		return s.idxPath, nil
	}
	return "", fmt.Errorf("localFileStore: unknown key %q", key)
}

func (s *localFileStore) Name() string                       { return "localfile-test" }
func (s *localFileStore) Capabilities() storage.Capabilities { return storage.Capabilities{} }

func (s *localFileStore) Get(ctx context.Context, key string, opts *storage.GetOptions) (*storage.Object, error) {
	p, err := s.pathFor(key)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	return &storage.Object{
		Body:     f,
		Metadata: storage.ObjectMetadata{Key: key, Size: st.Size()},
	}, nil
}

func (s *localFileStore) Head(ctx context.Context, key string) (*storage.ObjectMetadata, error) {
	p, err := s.pathFor(key)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	return &storage.ObjectMetadata{Key: key, Size: st.Size()}, nil
}

func (s *localFileStore) GetRange(ctx context.Context, key string, start, endInclusive int64) (io.ReadCloser, error) {
	p, err := s.pathFor(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return &lengthReader{f: f, remaining: endInclusive - start + 1}, nil
}

func (s *localFileStore) PutIfAbsent(_ context.Context, _ string, _ io.Reader, _ *storage.PutOptions) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, fmt.Errorf("localFileStore: read-only")
}

func (s *localFileStore) PutIfVersionMatches(_ context.Context, _ string, _ storage.ObjectVersion, _ io.Reader, _ *storage.PutOptions) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, fmt.Errorf("localFileStore: read-only")
}

func (s *localFileStore) DeleteIfVersionMatches(_ context.Context, _ string, _ storage.ObjectVersion) error {
	return fmt.Errorf("localFileStore: read-only")
}

func (s *localFileStore) List(_ context.Context, _ string, _ *storage.ListOptions) (*storage.ListPage, error) {
	return nil, fmt.Errorf("localFileStore: list not supported")
}

func (s *localFileStore) CreateMultipart(_ context.Context, _ string, _ *storage.MultipartOptions) (storage.MultipartUpload, error) {
	return nil, fmt.Errorf("localFileStore: multipart not supported")
}

func (s *localFileStore) CompleteMultipartIfAbsent(_ context.Context, _ storage.MultipartUpload, _ []storage.MultipartPart) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, fmt.Errorf("localFileStore: multipart not supported")
}

func (s *localFileStore) SignedGetURL(_ context.Context, _ string, _ storage.SignedURLOptions) (string, http.Header, error) {
	return "", nil, fmt.Errorf("localFileStore: signed URLs not supported")
}

type lengthReader struct {
	f         *os.File
	remaining int64
}

func (lr *lengthReader) Read(p []byte) (int, error) {
	if lr.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > lr.remaining {
		p = p[:lr.remaining]
	}
	n, err := lr.f.Read(p)
	lr.remaining -= int64(n)
	return n, err
}

func (lr *lengthReader) Close() error { return lr.f.Close() }
