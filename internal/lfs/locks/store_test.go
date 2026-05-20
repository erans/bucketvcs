package locks_test

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/lfs/locks"
)

// newTestDB opens a fresh in-memory authdb with all migrations
// applied, then seeds one user so foreign-key references resolve.
// Returns the locks.Store and a userID for tests to attribute locks to.
func newTestDB(t *testing.T) (*locks.Store, string) {
	t.Helper()
	authdb, err := sqlitestore.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlitestore.Open: %v", err)
	}
	t.Cleanup(func() { _ = authdb.Close() })
	userID, err := authdb.CreateUser(context.Background(), "alice", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	store := locks.New(authdb)
	return store, userID
}

func TestStore_CreateAndGet(t *testing.T) {
	store, userID := newTestDB(t)
	ctx := context.Background()

	id, err := store.Create(ctx, locks.CreateInput{
		Tenant:      "acme",
		Repo:        "demo",
		Path:        "art/hero.psd",
		RefName:     "refs/heads/release",
		OwnerUserID: userID,
		Now:         time.Unix(1700000000, 0),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id == "" {
		t.Fatalf("Create returned empty id")
	}

	got, err := store.Get(ctx, "acme", "demo", id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Path != "art/hero.psd" {
		t.Errorf("Path=%q want art/hero.psd", got.Path)
	}
	if got.RefName != "refs/heads/release" {
		t.Errorf("RefName=%q want refs/heads/release", got.RefName)
	}
	if got.Owner.UserID != userID {
		t.Errorf("Owner.UserID=%q want %q", got.Owner.UserID, userID)
	}
	if got.Owner.Name != "alice" {
		t.Errorf("Owner.Name=%q want alice", got.Owner.Name)
	}
	if got.LockedAt.Unix() != 1700000000 {
		t.Errorf("LockedAt=%v want unix 1700000000", got.LockedAt)
	}
}

func TestStore_Create_DuplicatePathReturnsErrAlreadyLocked(t *testing.T) {
	store, userID := newTestDB(t)
	ctx := context.Background()
	in := locks.CreateInput{
		Tenant:      "acme",
		Repo:        "demo",
		Path:        "art/hero.psd",
		OwnerUserID: userID,
		Now:         time.Unix(1700000000, 0),
	}
	if _, err := store.Create(ctx, in); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	_, err := store.Create(ctx, in)
	if !errors.Is(err, locks.ErrAlreadyLocked) {
		t.Fatalf("err=%v want ErrAlreadyLocked", err)
	}
}

func TestStore_Get_NotFound(t *testing.T) {
	store, _ := newTestDB(t)
	_, err := store.Get(context.Background(), "acme", "demo", "lock_nonexistent")
	if !errors.Is(err, locks.ErrNotFound) {
		t.Fatalf("err=%v want ErrNotFound", err)
	}
}

func TestStore_Delete_NotFound(t *testing.T) {
	store, _ := newTestDB(t)
	err := store.Delete(context.Background(), "acme", "demo", "lock_nonexistent")
	if !errors.Is(err, locks.ErrNotFound) {
		t.Fatalf("err=%v want ErrNotFound", err)
	}
}

func TestStore_Delete_Success(t *testing.T) {
	store, userID := newTestDB(t)
	ctx := context.Background()
	id, err := store.Create(ctx, locks.CreateInput{
		Tenant: "acme", Repo: "demo", Path: "a.psd",
		OwnerUserID: userID, Now: time.Unix(1700000000, 0),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Delete(ctx, "acme", "demo", id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Confirm it's gone.
	if _, err := store.Get(ctx, "acme", "demo", id); !errors.Is(err, locks.ErrNotFound) {
		t.Fatalf("post-Delete Get: %v want ErrNotFound", err)
	}
}

func TestStore_List_Filters(t *testing.T) {
	store, userID := newTestDB(t)
	ctx := context.Background()
	// Seed three locks: two on release branch (one for alice path A,
	// one for alice path B), one repo-wide (null ref).
	for _, in := range []locks.CreateInput{
		{Tenant: "acme", Repo: "demo", Path: "a.psd", RefName: "refs/heads/release", OwnerUserID: userID, Now: time.Unix(100, 0)},
		{Tenant: "acme", Repo: "demo", Path: "b.psd", RefName: "refs/heads/release", OwnerUserID: userID, Now: time.Unix(200, 0)},
		{Tenant: "acme", Repo: "demo", Path: "c.psd", OwnerUserID: userID, Now: time.Unix(300, 0)},
	} {
		if _, err := store.Create(ctx, in); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	t.Run("no-filter returns all 3", func(t *testing.T) {
		got, nextCursor, err := store.List(ctx, "acme", "demo", locks.ListOptions{})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 3 {
			t.Errorf("len=%d want 3", len(got))
		}
		if nextCursor != "" {
			t.Errorf("nextCursor=%q want empty", nextCursor)
		}
	})

	t.Run("path filter", func(t *testing.T) {
		got, _, err := store.List(ctx, "acme", "demo", locks.ListOptions{Path: "a.psd"})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 1 || got[0].Path != "a.psd" {
			t.Errorf("got=%v want one a.psd", got)
		}
	})

	t.Run("ref filter includes repo-wide nulls", func(t *testing.T) {
		got, _, err := store.List(ctx, "acme", "demo", locks.ListOptions{RefName: "refs/heads/release"})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		// Two release-scoped + one repo-wide = 3.
		if len(got) != 3 {
			t.Errorf("len=%d want 3 (release-scoped + repo-wide)", len(got))
		}
	})

	t.Run("ref filter non-matching excludes specific-ref but keeps repo-wide", func(t *testing.T) {
		got, _, err := store.List(ctx, "acme", "demo", locks.ListOptions{RefName: "refs/heads/other"})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		// Zero release-scoped + one repo-wide = 1.
		if len(got) != 1 || got[0].Path != "c.psd" {
			t.Errorf("got=%v want one repo-wide c.psd", got)
		}
	})
}

func TestStore_List_Pagination(t *testing.T) {
	store, userID := newTestDB(t)
	ctx := context.Background()
	// Seed 5 locks.
	for i := 0; i < 5; i++ {
		if _, err := store.Create(ctx, locks.CreateInput{
			Tenant: "acme", Repo: "demo",
			Path:        "p/" + strconv.Itoa(i) + ".psd",
			OwnerUserID: userID, Now: time.Unix(int64(100+i), 0),
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	// Page 1: limit=2 → 2 locks, cursor for next page.
	page1, cursor1, err := store.List(ctx, "acme", "demo", locks.ListOptions{Limit: 2})
	if err != nil {
		t.Fatalf("List page 1: %v", err)
	}
	if len(page1) != 2 {
		t.Errorf("page1 len=%d want 2", len(page1))
	}
	if cursor1 == "" {
		t.Errorf("page1 cursor empty; want non-empty (more pages remain)")
	}
	// Page 2: limit=2 → 2 locks, more cursor.
	page2, cursor2, err := store.List(ctx, "acme", "demo", locks.ListOptions{Limit: 2, Cursor: cursor1})
	if err != nil {
		t.Fatalf("List page 2: %v", err)
	}
	if len(page2) != 2 {
		t.Errorf("page2 len=%d want 2", len(page2))
	}
	if cursor2 == "" {
		t.Errorf("page2 cursor empty; want non-empty (1 more page remains)")
	}
	// Page 3: limit=2 → 1 lock, empty cursor (no more).
	page3, cursor3, err := store.List(ctx, "acme", "demo", locks.ListOptions{Limit: 2, Cursor: cursor2})
	if err != nil {
		t.Fatalf("List page 3: %v", err)
	}
	if len(page3) != 1 {
		t.Errorf("page3 len=%d want 1", len(page3))
	}
	if cursor3 != "" {
		t.Errorf("page3 cursor=%q want empty (last page)", cursor3)
	}
}

func TestStore_List_BadCursor(t *testing.T) {
	store, _ := newTestDB(t)
	_, _, err := store.List(context.Background(), "acme", "demo", locks.ListOptions{Cursor: "not-a-number"})
	if !errors.Is(err, locks.ErrBadCursor) {
		t.Fatalf("err=%v want ErrBadCursor", err)
	}
}

func TestStore_List_CursorClampedAtMaxOffset(t *testing.T) {
	// Insert 3 locks; request limit=1 starting from cursor=maxOffset (10000).
	// The server should NOT emit a cursor past maxOffset — it must stop
	// pagination rather than return a cursor it would itself reject.
	store, userID := newTestDB(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := store.Create(ctx, locks.CreateInput{
			Tenant: "acme", Repo: "demo",
			Path:        "p/" + strconv.Itoa(i) + ".psd",
			OwnerUserID: userID, Now: time.Unix(int64(100+i), 0),
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	// Request limit=1 starting from offset=maxOffset (cap boundary).
	cursor := strconv.Itoa(10000) // maxOffset
	page, nextCursor, err := store.List(ctx, "acme", "demo", locks.ListOptions{Cursor: cursor, Limit: 1})
	if err != nil {
		t.Fatalf("List at cap: %v", err)
	}
	// The page is empty (offset past our 3 rows), and importantly NextCursor
	// is empty rather than pointing past the cap.
	if len(page) != 0 {
		t.Errorf("page at offset=maxOffset: len=%d want 0", len(page))
	}
	if nextCursor != "" {
		t.Errorf("nextCursor=%q want empty (server must not emit cursors past maxOffset)", nextCursor)
	}
}

func TestStore_List_OffsetTooLargeReturnsErrBadCursor(t *testing.T) {
	store, _ := newTestDB(t)
	_, _, err := store.List(context.Background(), "acme", "demo", locks.ListOptions{Cursor: "100000000"})
	if !errors.Is(err, locks.ErrBadCursor) {
		t.Fatalf("err=%v want ErrBadCursor", err)
	}
}

func TestStore_OwnerDeletionCascades(t *testing.T) {
	// Regression guard: lfs_locks.owner_user_id REFERENCES users(id)
	// ON DELETE CASCADE. This only works if PRAGMA foreign_keys = ON
	// is set on the open authdb connection. If a future sqlitestore
	// refactor relaxes that pragma, this test fires.
	authdb, err := sqlitestore.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = authdb.Close() })
	userID, err := authdb.CreateUser(context.Background(), "alice", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	store := locks.New(authdb)
	ctx := context.Background()
	id, err := store.Create(ctx, locks.CreateInput{
		Tenant: "acme", Repo: "demo", Path: "a.psd",
		OwnerUserID: userID, Now: time.Unix(100, 0),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Delete the user.
	if err := authdb.DeleteUser(context.Background(), "alice"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	// Lock should be gone too.
	_, err = store.Get(ctx, "acme", "demo", id)
	if !errors.Is(err, locks.ErrNotFound) {
		t.Fatalf("post-DeleteUser Get=%v want ErrNotFound (FK cascade should have removed the lock)", err)
	}
}

func TestStore_Verify_PartitionsOwnership(t *testing.T) {
	authdb, err := sqlitestore.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = authdb.Close() })
	aliceID, err := authdb.CreateUser(context.Background(), "alice", false)
	if err != nil {
		t.Fatalf("CreateUser alice: %v", err)
	}
	bobID, err := authdb.CreateUser(context.Background(), "bob", false)
	if err != nil {
		t.Fatalf("CreateUser bob: %v", err)
	}
	store := locks.New(authdb)
	ctx := context.Background()

	if _, err := store.Create(ctx, locks.CreateInput{
		Tenant: "acme", Repo: "demo", Path: "a.psd",
		OwnerUserID: aliceID, Now: time.Unix(100, 0),
	}); err != nil {
		t.Fatalf("Create alice: %v", err)
	}
	if _, err := store.Create(ctx, locks.CreateInput{
		Tenant: "acme", Repo: "demo", Path: "b.psd",
		OwnerUserID: bobID, Now: time.Unix(200, 0),
	}); err != nil {
		t.Fatalf("Create bob: %v", err)
	}

	result, err := store.Verify(ctx, "acme", "demo", aliceID, locks.ListOptions{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(result.Ours) != 1 || result.Ours[0].Path != "a.psd" {
		t.Errorf("Ours=%v want one a.psd", result.Ours)
	}
	if len(result.Theirs) != 1 || result.Theirs[0].Path != "b.psd" {
		t.Errorf("Theirs=%v want one b.psd", result.Theirs)
	}
}
