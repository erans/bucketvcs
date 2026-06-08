package buildtrigger

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func TestEnqueue_RefFilter(t *testing.T) {
	svc, _ := newTestSvc(t)
	ctx := context.Background()
	if _, err := svc.Create(ctx, TriggerInput{
		Tenant: "acme", Repo: "app", Name: "main", Kind: KindGeneric,
		Config: Config{URL: "https://x"}, RefInclude: []string{"refs/heads/main"},
		TokenScopes: auth.ScopeRepoRead,
	}); err != nil {
		t.Fatal(err)
	}
	push := PushInfo{
		Tenant: "acme", Repo: "app", Actor: "u", TxID: "tx1", HeadOID: "abc",
		RefUpdates: []RefUpdate{
			{Refname: "refs/heads/main", OldOID: "0", NewOID: "abc"},
			{Refname: "refs/heads/dev", OldOID: "0", NewOID: "def"},
		},
	}
	if err := svc.Enqueue(ctx, push); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	rows, err := svc.PendingForTest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d deliveries, want 1 (only main matched)", len(rows))
	}
}

func TestEnqueue_InactiveSkipped(t *testing.T) {
	svc, _ := newTestSvc(t)
	ctx := context.Background()
	tr, _ := svc.Create(ctx, TriggerInput{Tenant: "acme", Repo: "app", Name: "main", Kind: KindGeneric,
		Config: Config{URL: "https://x"}, RefInclude: []string{"refs/heads/main"}})
	if err := svc.Disable(ctx, tr.ID); err != nil {
		t.Fatal(err)
	}
	push := PushInfo{Tenant: "acme", Repo: "app", RefUpdates: []RefUpdate{{Refname: "refs/heads/main", NewOID: "abc"}}}
	if err := svc.Enqueue(ctx, push); err != nil {
		t.Fatal(err)
	}
	rows, _ := svc.PendingForTest(ctx)
	if len(rows) != 0 {
		t.Fatalf("disabled trigger must not enqueue, got %d", len(rows))
	}
}

func TestEnqueue_MultipleMatchingRefs(t *testing.T) {
	svc, _ := newTestSvc(t)
	ctx := context.Background()
	svc.Create(ctx, TriggerInput{Tenant: "acme", Repo: "app", Name: "all", Kind: KindGeneric,
		Config: Config{URL: "https://x"}, RefInclude: []string{"refs/heads/**"}})
	push := PushInfo{Tenant: "acme", Repo: "app", RefUpdates: []RefUpdate{
		{Refname: "refs/heads/main", NewOID: "a"}, {Refname: "refs/heads/dev", NewOID: "b"}}}
	svc.Enqueue(ctx, push)
	rows, _ := svc.PendingForTest(ctx)
	if len(rows) != 2 {
		t.Fatalf("expected 2 deliveries (one per matching ref), got %d", len(rows))
	}
}

func TestEnqueue_NilServiceNoop(t *testing.T) {
	var svc *Service
	if err := svc.Enqueue(context.Background(), PushInfo{}); err != nil {
		t.Fatalf("nil enqueue must be no-op, got %v", err)
	}
}

func TestEnqueue_PayloadContent(t *testing.T) {
	svc, _ := newTestSvc(t)
	ctx := context.Background()
	tr, err := svc.Create(ctx, TriggerInput{
		Tenant: "acme", Repo: "app", Name: "main", Kind: KindGeneric,
		Config:      Config{URL: "https://x"},
		RefInclude:  []string{"refs/heads/main"},
		TokenScopes: auth.ScopeRepoRead,
	})
	if err != nil {
		t.Fatal(err)
	}
	push := PushInfo{
		Tenant: "acme", Repo: "app", Actor: "alice", TxID: "tx9", HeadOID: "deadbeef",
		RefUpdates: []RefUpdate{
			{Refname: "refs/heads/main", OldOID: "0000", NewOID: "deadbeef"},
		},
	}
	if err := svc.Enqueue(ctx, push); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	var raw []byte
	err = svc.db.QueryRowContext(ctx,
		`SELECT payload_json FROM build_trigger_deliveries WHERE trigger_id=?`, tr.ID,
	).Scan(&raw)
	if err != nil {
		t.Fatalf("query payload_json: %v", err)
	}

	var got BuildPayload
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if got.Tenant != "acme" {
		t.Errorf("Tenant: got %q want %q", got.Tenant, "acme")
	}
	if got.Repo != "app" {
		t.Errorf("Repo: got %q want %q", got.Repo, "app")
	}
	if got.Actor != "alice" {
		t.Errorf("Actor: got %q want %q", got.Actor, "alice")
	}
	if got.TxID != "tx9" {
		t.Errorf("TxID: got %q want %q", got.TxID, "tx9")
	}
	if got.HeadOID != "deadbeef" {
		t.Errorf("HeadOID: got %q want %q", got.HeadOID, "deadbeef")
	}
	if got.RefUpdate.Refname != "refs/heads/main" {
		t.Errorf("RefUpdate.Refname: got %q want %q", got.RefUpdate.Refname, "refs/heads/main")
	}
	if got.RefUpdate.NewOID != "deadbeef" {
		t.Errorf("RefUpdate.NewOID: got %q want %q", got.RefUpdate.NewOID, "deadbeef")
	}
}

func TestEnqueue_CrossRepoIsolation(t *testing.T) {
	svc, db := newTestSvc(t)
	ctx := context.Background()

	// Trigger exists only for acme/app.
	if _, err := svc.Create(ctx, TriggerInput{
		Tenant: "acme", Repo: "app", Name: "main", Kind: KindGeneric,
		Config:     Config{URL: "https://x"},
		RefInclude: []string{"refs/heads/main"},
	}); err != nil {
		t.Fatal(err)
	}

	// Seed a second repo acme/other so the push is for a valid repo (no FK issue).
	if _, err := db.ExecContext(ctx,
		`INSERT INTO repos (tenant, name, public_read, created_at)
		 VALUES (?, ?, 0, strftime('%s','now'))`, "acme", "other",
	); err != nil {
		t.Fatalf("seed acme/other: %v", err)
	}

	// Enqueue a push for acme/other — no triggers registered there.
	push := PushInfo{
		Tenant: "acme", Repo: "other", Actor: "bob", TxID: "tx10", HeadOID: "cafebabe",
		RefUpdates: []RefUpdate{
			{Refname: "refs/heads/main", OldOID: "0000", NewOID: "cafebabe"},
		},
	}
	if err := svc.Enqueue(ctx, push); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	rows, err := svc.PendingForTest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("cross-repo push must produce zero deliveries, got %d", len(rows))
	}
}

