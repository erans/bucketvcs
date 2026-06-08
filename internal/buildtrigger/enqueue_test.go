package buildtrigger

import (
	"context"
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
