package policy_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/policy"
)

// openTestDB returns a fresh on-disk authdb with migrations applied,
// pre-seeded with a (tenant, repo) row so the FK on protected_refs
// is satisfiable. Mirrors the M13.5 quota tests' shape.
func openTestDB(t *testing.T, tenant, repo string) sqlitestore.Querier {
	t.Helper()
	tmp := t.TempDir()
	store, err := sqlitestore.Open(filepath.Join(tmp, "auth.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO repos (tenant, name, public_read, created_at)
		 VALUES (?, ?, 0, strftime('%s','now'))`,
		tenant, repo,
	); err != nil {
		t.Fatalf("seed repo row: %v", err)
	}
	return db
}

func TestService_AddListRemove(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db)
	ctx := context.Background()

	// Initial: empty list.
	got, err := svc.List(ctx, "acme", "site")
	if err != nil {
		t.Fatalf("List empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("List empty: len=%d, want 0", len(got))
	}

	// Add two rules.
	if err := svc.Add(ctx, policy.ProtectedRef{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/main",
		BlockDeletion:  true, BlockForcePush: true,
	}); err != nil {
		t.Fatalf("Add main: %v", err)
	}
	if err := svc.Add(ctx, policy.ProtectedRef{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/release/*",
		BlockDeletion:  true, BlockForcePush: false,
	}); err != nil {
		t.Fatalf("Add release: %v", err)
	}

	// List returns both, ordered by pattern.
	got, err = svc.List(ctx, "acme", "site")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List: len=%d, want 2; got=%+v", len(got), got)
	}
	if got[0].RefnamePattern != "refs/heads/main" {
		t.Errorf("List[0].Pattern=%q, want refs/heads/main", got[0].RefnamePattern)
	}
	if !got[0].BlockDeletion || !got[0].BlockForcePush {
		t.Errorf("List[0] toggles=%v/%v, want true/true", got[0].BlockDeletion, got[0].BlockForcePush)
	}
	if got[1].RefnamePattern != "refs/heads/release/*" {
		t.Errorf("List[1].Pattern=%q, want refs/heads/release/*", got[1].RefnamePattern)
	}
	if got[1].BlockForcePush {
		t.Errorf("List[1].BlockForcePush=true, want false")
	}
	// CreatedAt populated.
	if got[0].CreatedAt.IsZero() {
		t.Errorf("List[0].CreatedAt is zero")
	}

	// Remove one rule.
	if err := svc.Remove(ctx, "acme", "site", "refs/heads/main"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	got, _ = svc.List(ctx, "acme", "site")
	if len(got) != 1 || got[0].RefnamePattern != "refs/heads/release/*" {
		t.Errorf("after Remove: %+v, want only release rule", got)
	}

	// Remove non-existent pattern is a no-op (no error).
	if err := svc.Remove(ctx, "acme", "site", "refs/heads/nonexistent"); err != nil {
		t.Errorf("Remove non-existent: %v, want nil", err)
	}

	// List for unknown repo returns empty, not error.
	got, err = svc.List(ctx, "no-such", "repo")
	if err != nil {
		t.Errorf("List unknown repo: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List unknown repo: len=%d, want 0", len(got))
	}
}

func TestService_AddIsIdempotent(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db)
	ctx := context.Background()
	ref := policy.ProtectedRef{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/main",
		BlockDeletion:  true, BlockForcePush: true,
	}
	if err := svc.Add(ctx, ref); err != nil {
		t.Fatalf("Add first: %v", err)
	}
	// Re-Add with different toggles → updates the existing row.
	ref.BlockForcePush = false
	if err := svc.Add(ctx, ref); err != nil {
		t.Fatalf("Add update: %v", err)
	}
	got, _ := svc.List(ctx, "acme", "site")
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1 (update, not insert)", len(got))
	}
	if got[0].BlockForcePush {
		t.Errorf("after Add update: BlockForcePush=true, want false")
	}
}

func TestService_AddRejectsMalformedPattern(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db)
	// `[` opens a character class that never closes; path.Match
	// returns ErrBadPattern.
	ref := policy.ProtectedRef{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/[broken",
		BlockDeletion:  true, BlockForcePush: true,
	}
	if err := svc.Add(context.Background(), ref); err == nil {
		t.Errorf("Add malformed pattern returned nil; want error")
	}
}

func TestService_AddRejectsEmptyPattern(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db)
	ref := policy.ProtectedRef{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "",
		BlockDeletion:  true, BlockForcePush: true,
	}
	if err := svc.Add(context.Background(), ref); err == nil {
		t.Errorf("Add empty pattern returned nil; want error")
	}
}
