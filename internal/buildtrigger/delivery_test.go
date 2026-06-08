package buildtrigger

import (
	"context"
	"errors"
	"testing"
)

func TestDelivery_ListGet(t *testing.T) {
	svc, _ := newTestSvc(t)
	ctx := context.Background()
	tr, err := svc.Create(ctx, TriggerInput{
		Tenant: "acme", Repo: "app", Name: "main-cb", Kind: KindCloudBuild,
		Config:     Config{URL: "https://cloudbuild.example/x"},
		RefInclude: []string{"refs/heads/main"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.Enqueue(ctx, PushInfo{
		Tenant: "acme", Repo: "app", Actor: "alice", TxID: "tx1", HeadOID: "abc",
		RefUpdates: []RefUpdate{{
			Refname: "refs/heads/main",
			OldOID:  "0000000000000000000000000000000000000000",
			NewOID:  "1111111111111111111111111111111111111111",
		}},
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	all, err := svc.ListDeliveries(ctx, "", "", 0)
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 delivery, got %d", len(all))
	}
	d := all[0]
	if d.TriggerID != tr.ID {
		t.Errorf("delivery trigger_id=%q want %q", d.TriggerID, tr.ID)
	}
	if d.Status != "pending" {
		t.Errorf("delivery status=%q want pending", d.Status)
	}

	// Filter by trigger id + status narrows correctly.
	filtered, err := svc.ListDeliveries(ctx, tr.ID, "pending", 10)
	if err != nil || len(filtered) != 1 {
		t.Fatalf("filtered list: err=%v len=%d", err, len(filtered))
	}
	none, err := svc.ListDeliveries(ctx, tr.ID, "delivered", 10)
	if err != nil || len(none) != 0 {
		t.Fatalf("filtered (delivered) list: err=%v len=%d", err, len(none))
	}

	got, err := svc.GetDelivery(ctx, d.ID)
	if err != nil {
		t.Fatalf("get delivery: %v", err)
	}
	if got.ID != d.ID || got.Status != "pending" {
		t.Errorf("get delivery mismatch: %+v", got)
	}

	if _, err := svc.GetDelivery(ctx, "bvbd_does-not-exist"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get missing delivery: err=%v want ErrNotFound", err)
	}
}

func TestDelivery_Replay(t *testing.T) {
	svc, db := newTestSvc(t)
	ctx := context.Background()
	tr, err := svc.Create(ctx, TriggerInput{
		Tenant: "acme", Repo: "app", Name: "main-cb", Kind: KindCloudBuild,
		Config:     Config{URL: "https://cloudbuild.example/x"},
		RefInclude: []string{"refs/heads/main"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.Enqueue(ctx, PushInfo{
		Tenant: "acme", Repo: "app",
		RefUpdates: []RefUpdate{{Refname: "refs/heads/main", NewOID: "1111111111111111111111111111111111111111"}},
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	rows, err := svc.ListDeliveries(ctx, tr.ID, "", 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("list: err=%v len=%d", err, len(rows))
	}
	id := rows[0].ID

	// Manually dead-letter the row with a non-zero attempt count.
	if _, err := db.ExecContext(ctx,
		`UPDATE build_trigger_deliveries
		   SET status='dead_letter', attempts=5, last_error='boom', last_status_code=500
		 WHERE id=?`, id); err != nil {
		t.Fatalf("dead-letter update: %v", err)
	}

	if err := svc.ReplayDelivery(ctx, id); err != nil {
		t.Fatalf("replay dead_letter: %v", err)
	}
	after, err := svc.GetDelivery(ctx, id)
	if err != nil {
		t.Fatalf("get after replay: %v", err)
	}
	if after.Status != "pending" {
		t.Errorf("replay: status=%q want pending", after.Status)
	}
	if after.Attempts != 5 {
		t.Errorf("replay must NOT reset attempts: got %d want 5", after.Attempts)
	}
	if after.LastError != "" {
		t.Errorf("replay should clear last_error: got %q", after.LastError)
	}

	// Replaying an in_flight row is refused.
	if _, err := db.ExecContext(ctx,
		`UPDATE build_trigger_deliveries SET status='in_flight' WHERE id=?`, id); err != nil {
		t.Fatalf("in_flight update: %v", err)
	}
	if err := svc.ReplayDelivery(ctx, id); !errors.Is(err, ErrReplayInFlight) {
		t.Errorf("replay in_flight: err=%v want ErrReplayInFlight", err)
	}

	// Replaying a missing row returns ErrNotFound.
	if err := svc.ReplayDelivery(ctx, "bvbd_nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("replay missing: err=%v want ErrNotFound", err)
	}
}
