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
	txpkg "github.com/bucketvcs/bucketvcs/internal/repo/tx"
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
	// With reversed ordering (tx-first, then root PutIfAbsent), a duplicate
	// Create attempt writes a tx record before discovering the root exists.
	// That orphan tx record is acceptable; M8 GC sweeps it.
	page, err := s.List(ctx, "tenants/acme/repos/my-repo/tx/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Objects) != 2 {
		t.Errorf("want 2 tx records (original create + orphan from duplicate attempt), got %d", len(page.Objects))
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

func TestCommit_HappyPath(t *testing.T) {
	s := newLocalFS(t)
	ctx := context.Background()
	r, _ := repo.Create(ctx, s, "acme", "x", repo.CreateOptions{Actor: "u"})

	txID, err := r.Commit(ctx,
		txpkg.Body{Type: "push", Actor: "u_pusher"},
		func(prev *repo.RootView) ([]byte, error) {
			var top map[string]json.RawMessage
			if err := json.Unmarshal(prev.Body, &top); err != nil {
				return nil, err
			}
			top["refs"] = json.RawMessage(`{"refs/heads/main":{"target":"abc"}}`)
			return json.Marshal(top)
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if txID == "" || !strings.HasPrefix(txID, "tx_") {
		t.Errorf("bad tx id: %q", txID)
	}

	v, _ := r.ReadRoot(ctx)
	if v.Header.ManifestVersion != 2 {
		t.Errorf("want manifest_version=2 after one Commit, got %d", v.Header.ManifestVersion)
	}
	if v.Header.LatestTx != txID {
		t.Errorf("LatestTx mismatch: want %s, got %s", txID, v.Header.LatestTx)
	}
	if !strings.Contains(string(v.Body), "refs/heads/main") {
		t.Errorf("body did not record the ref: %s", v.Body)
	}
}

func TestCommit_CallbackError(t *testing.T) {
	s := newLocalFS(t)
	ctx := context.Background()
	r, _ := repo.Create(ctx, s, "acme", "x", repo.CreateOptions{})
	sentinel := errors.New("callback returned this")

	_, err := r.Commit(ctx, txpkg.Body{Type: "push", Actor: "u"}, func(*repo.RootView) ([]byte, error) {
		return nil, sentinel
	})
	if !errors.Is(err, repo.ErrCallbackFailed) {
		t.Errorf("want ErrCallbackFailed, got %v", err)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("err should also unwrap to caller's sentinel, got %v", err)
	}
	// No new tx record (callback runs BEFORE PutIfAbsent).
	page, _ := s.List(ctx, "tenants/acme/repos/x/tx/", nil)
	if len(page.Objects) != 1 {
		t.Errorf("want 1 tx record (the create), got %d", len(page.Objects))
	}
	v, _ := r.ReadRoot(ctx)
	if v.Header.ManifestVersion != 1 {
		t.Errorf("want manifest_version=1, got %d", v.Header.ManifestVersion)
	}
}

func TestCommit_PerAttemptFreshTxID(t *testing.T) {
	s := newLocalFS(t)
	ctx := context.Background()
	r, _ := repo.Create(ctx, s, "acme", "x", repo.CreateOptions{Actor: "u"})

	calls := 0
	txID, err := r.Commit(ctx, txpkg.Body{Type: "push", Actor: "u"}, func(prev *repo.RootView) ([]byte, error) {
		calls++
		if calls == 1 {
			// Race: do a side-channel commit to invalidate prev.Version
			_, err := r.Commit(ctx, txpkg.Body{Type: "push", Actor: "u_other"}, func(p2 *repo.RootView) ([]byte, error) {
				return p2.Body, nil
			})
			if err != nil {
				return nil, err
			}
		}
		return prev.Body, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Errorf("expected callback re-invoked on conflict; called %d times", calls)
	}
	v, _ := r.ReadRoot(ctx)
	if v.Header.LatestTx != txID {
		t.Errorf("LatestTx mismatch")
	}
	if v.Header.ManifestVersion != 3 {
		t.Errorf("want manifest_version=3, got %d", v.Header.ManifestVersion)
	}
	// Tx records on disk: 1 create + 1 orphan (from the first attempt
	// that lost CAS) + 2 winners (side-channel + outer) = 4.
	page, _ := s.List(ctx, "tenants/acme/repos/x/tx/", nil)
	if len(page.Objects) != 4 {
		t.Errorf("want 4 tx records (1 create + 1 orphan + 2 committed); got %d", len(page.Objects))
	}
}

func TestCommit_RetryBudgetExhausted(t *testing.T) {
	s := newLocalFS(t)
	ctx := context.Background()
	r, _ := repo.Create(ctx, s, "acme", "x", repo.CreateOptions{Actor: "u"})

	_, err := r.Commit(ctx, txpkg.Body{Type: "push", Actor: "u"},
		func(prev *repo.RootView) ([]byte, error) {
			// Always race: side-channel commit so the main commit never wins.
			_, _ = r.Commit(ctx, txpkg.Body{Type: "push", Actor: "u_other"},
				func(p2 *repo.RootView) ([]byte, error) { return p2.Body, nil })
			return prev.Body, nil
		},
		repo.WithCommitPolicy(repo.CommitPolicy{MaxRetries: 3, BackoffBase: 0}),
	)
	var gaveUp *repo.CommitGaveUpError
	if !errors.As(err, &gaveUp) {
		t.Fatalf("want *CommitGaveUpError, got %v", err)
	}
	if gaveUp.Attempts != 3 {
		t.Errorf("want Attempts=3, got %d", gaveUp.Attempts)
	}
	if len(gaveUp.OrphanTxIDs) != 3 {
		t.Errorf("want 3 orphan tx ids, got %d", len(gaveUp.OrphanTxIDs))
	}
}

func TestCommit_CtxCancelMidCommit(t *testing.T) {
	s := newLocalFS(t)
	ctx, cancel := context.WithCancel(context.Background())
	r, _ := repo.Create(context.Background(), s, "acme", "x", repo.CreateOptions{Actor: "u"})

	_, err := r.Commit(ctx, txpkg.Body{Type: "push", Actor: "u"},
		func(prev *repo.RootView) ([]byte, error) {
			cancel() // cancel after callback runs but before CAS
			return prev.Body, nil
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
	v, _ := r.ReadRoot(context.Background())
	if v.Header.ManifestVersion != 1 {
		t.Errorf("manifest_version should remain 1, got %d", v.Header.ManifestVersion)
	}
}
