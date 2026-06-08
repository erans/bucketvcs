package buildtrigger

import (
	"context"
	"testing"
)

// TestDeadLetterOrphans_RemovedTrigger asserts that a pending delivery whose
// trigger has been removed is dead-lettered by the orphan sweep (design §2.2 /
// §7), while a pending delivery whose trigger still exists is left untouched.
func TestDeadLetterOrphans_RemovedTrigger(t *testing.T) {
	svc, db := newTestSvc(t)
	ctx := context.Background()

	// Trigger A: will be removed → its delivery becomes an orphan.
	trA, err := svc.Create(ctx, TriggerInput{Tenant: "acme", Repo: "app", Name: "a",
		Kind: KindGeneric, Config: Config{URL: "https://x"}, RefInclude: []string{"refs/heads/main"}})
	if err != nil {
		t.Fatal(err)
	}
	// Trigger B: stays alive → its delivery must NOT be dead-lettered.
	trB, err := svc.Create(ctx, TriggerInput{Tenant: "acme", Repo: "app", Name: "b",
		Kind: KindGeneric, Config: Config{URL: "https://y"}, RefInclude: []string{"refs/heads/main"}})
	if err != nil {
		t.Fatal(err)
	}

	// Enqueue one pending delivery per active trigger.
	if err := svc.Enqueue(ctx, PushInfo{Tenant: "acme", Repo: "app", HeadOID: "a",
		RefUpdates: []RefUpdate{{Refname: "refs/heads/main", NewOID: "a"}}}); err != nil {
		t.Fatal(err)
	}
	if svc.countByStatus(ctx, "pending") != 2 {
		t.Fatalf("expected 2 pending deliveries, got %d", svc.countByStatus(ctx, "pending"))
	}

	// Remove trigger A; its delivery row remains (no FK cascade by design).
	if err := svc.Remove(ctx, trA.ID); err != nil {
		t.Fatal(err)
	}
	_ = trB

	n, err := DeadLetterOrphans(ctx, db)
	if err != nil {
		t.Fatalf("DeadLetterOrphans: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 orphan dead-lettered, got %d", n)
	}
	if got := svc.countByStatus(ctx, "dead_letter"); got != 1 {
		t.Fatalf("expected 1 dead_letter row, got %d", got)
	}
	// Trigger B's delivery is still pending (its trigger exists).
	if got := svc.countByStatus(ctx, "pending"); got != 1 {
		t.Fatalf("expected 1 pending row (live trigger), got %d", got)
	}
}

// TestDeadLetterOrphans_SkipsInFlight asserts the orphan sweep never touches
// in_flight rows (they are being worked by a live worker), even if the trigger
// is missing.
func TestDeadLetterOrphans_SkipsInFlight(t *testing.T) {
	svc, db := newTestSvc(t)
	ctx := context.Background()

	tr, err := svc.Create(ctx, TriggerInput{Tenant: "acme", Repo: "app", Name: "a",
		Kind: KindGeneric, Config: Config{URL: "https://x"}, RefInclude: []string{"refs/heads/main"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Enqueue(ctx, PushInfo{Tenant: "acme", Repo: "app", HeadOID: "a",
		RefUpdates: []RefUpdate{{Refname: "refs/heads/main", NewOID: "a"}}}); err != nil {
		t.Fatal(err)
	}
	// Force the lone delivery to in_flight, then orphan it.
	if _, err := db.ExecContext(ctx,
		`UPDATE build_trigger_deliveries SET status='in_flight'`); err != nil {
		t.Fatal(err)
	}
	if err := svc.Remove(ctx, tr.ID); err != nil {
		t.Fatal(err)
	}

	n, err := DeadLetterOrphans(ctx, db)
	if err != nil {
		t.Fatalf("DeadLetterOrphans: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 dead-lettered (in_flight skipped), got %d", n)
	}
	if got := svc.countByStatus(ctx, "in_flight"); got != 1 {
		t.Fatalf("expected 1 in_flight row untouched, got %d", got)
	}
}
