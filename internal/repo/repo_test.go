package repo_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func newLocalFS(t *testing.T) storage.ObjectStore {
	t.Helper()
	s, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOpen_NotFound(t *testing.T) {
	s := newLocalFS(t)
	_, err := repo.Open(context.Background(), s, "acme", "missing")
	if !errors.Is(err, repo.ErrRepoNotFound) {
		t.Errorf("want ErrRepoNotFound, got %v", err)
	}
}

func TestOpen_BadIDs(t *testing.T) {
	s := newLocalFS(t)
	_, err := repo.Open(context.Background(), s, "", "x")
	if !errors.Is(err, repo.ErrInvalidTenantID) {
		t.Errorf("want ErrInvalidTenantID, got %v", err)
	}
	_, err = repo.Open(context.Background(), s, "ok", "")
	if !errors.Is(err, repo.ErrInvalidRepoID) {
		t.Errorf("want ErrInvalidRepoID, got %v", err)
	}
}

func TestOpen_FutureSchemaRejected(t *testing.T) {
	s := newLocalFS(t)
	ctx := context.Background()
	header := manifest.RootHeader{
		SchemaVersion:    999,
		MinReaderVersion: "0.1.0",
		RepoID:           "b",
		RepoFormat:       manifest.Format{ObjectFormat: "sha1"},
		ManifestVersion:  1,
		LatestTx:         "tx_x",
		CreatedAt:        time.Now().UTC().Truncate(time.Second),
		UpdatedAt:        time.Now().UTC().Truncate(time.Second),
	}
	wrapped, err := manifest.WrapHeaderInBody(header, json.RawMessage(`{"refs":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutIfAbsent(ctx, "tenants/acme/repos/b/manifest/root.json",
		strings.NewReader(string(wrapped)), nil); err != nil {
		t.Fatal(err)
	}
	_, err = repo.Open(ctx, s, "acme", "b")
	if !errors.Is(err, repo.ErrUnsupportedSchema) {
		t.Errorf("want ErrUnsupportedSchema, got %v", err)
	}
}

func TestOpen_ExistingRepo_AccessorsCorrect(t *testing.T) {
	s := newLocalFS(t)
	ctx := context.Background()

	// Manually plant a valid manifest, since Create comes in Task 11.
	header := manifest.RootHeader{
		SchemaVersion:    1,
		MinReaderVersion: "0.1.0",
		RepoID:           "my-repo",
		RepoFormat:       manifest.Format{ObjectFormat: "sha1", Compatibility: []string{"sha1"}},
		ManifestVersion:  1,
		LatestTx:         "tx_init",
		CreatedAt:        time.Now().UTC().Truncate(time.Second),
		UpdatedAt:        time.Now().UTC().Truncate(time.Second),
	}
	wrapped, err := manifest.WrapHeaderInBody(header, json.RawMessage(`{"refs":{},"packs":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutIfAbsent(ctx, "tenants/acme/repos/my-repo/manifest/root.json",
		strings.NewReader(string(wrapped)), nil); err != nil {
		t.Fatal(err)
	}

	r, err := repo.Open(ctx, s, "acme", "my-repo")
	if err != nil {
		t.Fatal(err)
	}
	if r.TenantID() != "acme" || r.RepoID() != "my-repo" {
		t.Errorf("accessors wrong: tenant=%q repo=%q", r.TenantID(), r.RepoID())
	}
}

func TestCreate_HappyPath(t *testing.T) {
	s := newLocalFS(t)
	ctx := context.Background()
	r, err := repo.Create(ctx, s, "acme", "my-repo", repo.CreateOptions{
		DefaultBranch: "refs/heads/main",
		ObjectFormat:  "sha1",
		Actor:         "u_test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.TenantID() != "acme" || r.RepoID() != "my-repo" {
		t.Errorf("unexpected handle: %s/%s", r.TenantID(), r.RepoID())
	}
	view, err := r.ReadRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if view.Header.ManifestVersion != 1 {
		t.Errorf("want manifest_version=1, got %d", view.Header.ManifestVersion)
	}
	if view.Header.RepoID != "my-repo" {
		t.Errorf("want repo_id=my-repo, got %q", view.Header.RepoID)
	}
	if view.Header.SchemaVersion != 1 {
		t.Errorf("want schema_version=1, got %d", view.Header.SchemaVersion)
	}
	if view.Header.LatestTx == "" {
		t.Errorf("LatestTx should reference the create tx")
	}
}

func TestCreate_DefaultsApplied(t *testing.T) {
	s := newLocalFS(t)
	ctx := context.Background()
	r, err := repo.Create(ctx, s, "acme", "x", repo.CreateOptions{}) // all defaults
	if err != nil {
		t.Fatal(err)
	}
	view, _ := r.ReadRoot(ctx)
	if view.Header.RepoFormat.ObjectFormat != "sha1" {
		t.Errorf("want default sha1, got %q", view.Header.RepoFormat.ObjectFormat)
	}
	if !strings.Contains(string(view.Body), `"refs/heads/main"`) {
		t.Errorf("want default_branch refs/heads/main in body, got %s", view.Body)
	}
}

func TestCreate_RejectsUnsupportedObjectFormat(t *testing.T) {
	s := newLocalFS(t)
	_, err := repo.Create(context.Background(), s, "acme", "x", repo.CreateOptions{
		ObjectFormat: "sha256",
	})
	if err == nil {
		t.Fatal("expected error for unsupported object_format")
	}
}

func TestCreate_AlreadyExists(t *testing.T) {
	s := newLocalFS(t)
	ctx := context.Background()
	if _, err := repo.Create(ctx, s, "acme", "my-repo", repo.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := repo.Create(ctx, s, "acme", "my-repo", repo.CreateOptions{})
	if !errors.Is(err, repo.ErrRepoExists) {
		t.Errorf("want ErrRepoExists, got %v", err)
	}
	// Verify §4.3 carve-out: no orphan tx record from the failed create
	// (Create checks root.json existence FIRST via PutIfAbsent, only
	// writes the create-tx record on success).
	page, err := s.List(ctx, "tenants/acme/repos/my-repo/tx/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Objects) != 1 {
		t.Errorf("want 1 tx record (only the original create), got %d", len(page.Objects))
	}
}

func TestReadRoot_AfterCreate(t *testing.T) {
	s := newLocalFS(t)
	ctx := context.Background()
	r, _ := repo.Create(ctx, s, "acme", "x", repo.CreateOptions{Actor: "u"})
	v, err := r.ReadRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v.Version.Token == "" {
		t.Errorf("expected non-empty version token")
	}
	if v.SizeBytes == 0 {
		t.Errorf("expected non-zero size")
	}
	if !json.Valid(v.Body) {
		t.Errorf("body must be valid JSON: %s", v.Body)
	}
}

func TestCreate_TxRecordHasCreateType(t *testing.T) {
	s := newLocalFS(t)
	ctx := context.Background()
	r, _ := repo.Create(ctx, s, "acme", "x", repo.CreateOptions{Actor: "u_creator"})
	v, _ := r.ReadRoot(ctx)
	txKey := "tenants/acme/repos/x/tx/" + v.Header.LatestTx + ".json"

	obj, err := s.Get(ctx, txKey, nil)
	if err != nil {
		t.Fatalf("get tx record: %v", err)
	}
	defer obj.Body.Close()
	raw, _ := io.ReadAll(obj.Body)
	var tx map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tx); err != nil {
		t.Fatal(err)
	}
	if string(tx["type"]) != `"create"` {
		t.Errorf("tx type want \"create\", got %s", tx["type"])
	}
	if string(tx["actor"]) != `"u_creator"` {
		t.Errorf("tx actor want \"u_creator\", got %s", tx["actor"])
	}
}

func TestNewTxID_ConcurrentlyUnique(t *testing.T) {
	// Verify ulid.LockedMonotonicReader produces distinct IDs under
	// concurrent callers. The full concurrency suite lives at
	// internal/repo/internal in Task 17; this is a fast smoke test
	// against the minting primitive itself.
	const goroutines, perGoroutine = 16, 500
	ids := make(chan string, goroutines*perGoroutine)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				ids <- repo.NewTxIDForTest()
			}
		}()
	}
	wg.Wait()
	close(ids)

	seen := make(map[string]struct{}, goroutines*perGoroutine)
	for id := range ids {
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate tx_id minted: %q", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != goroutines*perGoroutine {
		t.Errorf("want %d unique ids, got %d", goroutines*perGoroutine, len(seen))
	}
}
