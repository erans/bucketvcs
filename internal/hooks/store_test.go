package hooks_test

import (
	"context"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/hooks"
)

func newTestStore(t *testing.T) *hooks.Store {
	t.Helper()
	authS, err := sqlitestore.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlitestore.Open: %v", err)
	}
	t.Cleanup(func() { _ = authS.Close() })
	// Seed a repo row so the FK cascade is satisfied.
	if err := authS.RegisterRepo(context.Background(), "acme", "site"); err != nil {
		t.Fatalf("RegisterRepo: %v", err)
	}
	return hooks.NewStore(authS.DB())
}

func TestStore_AddAndList(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Add(ctx, hooks.Row{
		Tenant: "acme", Repo: "site",
		Trigger: "pre-receive", ScriptName: "lint.sh",
		SortOrder: 10, Enabled: true,
		Now: time.Unix(100, 0),
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	rows, err := s.List(ctx, "acme", "site", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 || rows[0].ScriptName != "lint.sh" || rows[0].SortOrder != 10 {
		t.Errorf("List = %+v, want one row {lint.sh, 10}", rows)
	}
}

func TestStore_AddIsIdempotentUpsert(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	row := hooks.Row{
		Tenant: "acme", Repo: "site", Trigger: "pre-receive",
		ScriptName: "lint.sh", SortOrder: 10, Enabled: true,
		Now: time.Unix(100, 0),
	}
	if err := s.Add(ctx, row); err != nil {
		t.Fatal(err)
	}
	row.SortOrder = 20
	row.Now = time.Unix(200, 0)
	if err := s.Add(ctx, row); err != nil {
		t.Fatalf("re-Add (upsert): %v", err)
	}
	rows, _ := s.List(ctx, "acme", "site", "")
	if len(rows) != 1 || rows[0].SortOrder != 20 {
		t.Errorf("after upsert: want SortOrder=20, got %+v", rows)
	}
	if rows[0].CreatedAt.Unix() != 100 {
		t.Errorf("CreatedAt should be preserved across upsert; got %v", rows[0].CreatedAt)
	}
	if rows[0].UpdatedAt.Unix() != 200 {
		t.Errorf("UpdatedAt should advance on upsert; got %v", rows[0].UpdatedAt)
	}
}

func TestStore_ListActiveForTrigger_OrderedBySortOrder(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for i, name := range []string{"c.sh", "a.sh", "b.sh"} {
		_ = s.Add(ctx, hooks.Row{
			Tenant: "acme", Repo: "site", Trigger: "pre-receive",
			ScriptName: name, SortOrder: i * 10, Enabled: true,
			Now: time.Unix(int64(i), 0),
		})
	}
	rows, err := s.ListActiveForTrigger(ctx, "acme", "site", "pre-receive")
	if err != nil {
		t.Fatal(err)
	}
	got := []string{rows[0].ScriptName, rows[1].ScriptName, rows[2].ScriptName}
	want := []string{"c.sh", "a.sh", "b.sh"} // matches SortOrder 0, 10, 20
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("rows[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestStore_ListActiveForTrigger_SkipsDisabled(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.Add(ctx, hooks.Row{Tenant: "acme", Repo: "site", Trigger: "pre-receive",
		ScriptName: "on.sh", Enabled: true, Now: time.Unix(1, 0)})
	_ = s.Add(ctx, hooks.Row{Tenant: "acme", Repo: "site", Trigger: "pre-receive",
		ScriptName: "off.sh", Enabled: false, Now: time.Unix(2, 0)})
	rows, _ := s.ListActiveForTrigger(ctx, "acme", "site", "pre-receive")
	if len(rows) != 1 || rows[0].ScriptName != "on.sh" {
		t.Errorf("active rows = %+v, want only on.sh", rows)
	}
}

func TestStore_EnableDisable(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.Add(ctx, hooks.Row{Tenant: "acme", Repo: "site", Trigger: "pre-receive",
		ScriptName: "x.sh", Enabled: true, Now: time.Unix(1, 0)})
	if err := s.SetEnabled(ctx, "acme", "site", "pre-receive", "x.sh", false, time.Unix(2, 0)); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.List(ctx, "acme", "site", "")
	if rows[0].Enabled {
		t.Errorf("Enabled should be false after SetEnabled(false)")
	}
}

func TestStore_Remove(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.Add(ctx, hooks.Row{Tenant: "acme", Repo: "site", Trigger: "pre-receive",
		ScriptName: "x.sh", Enabled: true, Now: time.Unix(1, 0)})
	if err := s.Remove(ctx, "acme", "site", "pre-receive", "x.sh"); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.List(ctx, "acme", "site", "")
	if len(rows) != 0 {
		t.Errorf("after Remove: want empty list, got %+v", rows)
	}
}

func TestStore_Remove_NotFound_ReturnsErr(t *testing.T) {
	s := newTestStore(t)
	err := s.Remove(context.Background(), "acme", "site", "pre-receive", "nonexistent.sh")
	if err == nil {
		t.Error("Remove of missing row should error")
	}
}
