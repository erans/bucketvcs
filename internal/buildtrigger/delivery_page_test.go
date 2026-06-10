package buildtrigger

import (
	"context"
	"testing"
	"time"
)

// insertDelivery writes a delivery row with an explicit created_at for
// deterministic keyset ordering. payload_json is NOT NULL in the schema so
// we supply a minimal placeholder.
func insertDelivery(t *testing.T, s *Service, id, triggerID, status string, createdAt int64) {
	t.Helper()
	_, err := s.db.ExecContext(context.Background(),
		`INSERT INTO build_trigger_deliveries
		   (id, trigger_id, payload_json, status, attempts, next_attempt_at, created_at)
		 VALUES (?, ?, '{}', ?, 0, ?, ?)`,
		id, triggerID, status, createdAt, createdAt)
	if err != nil {
		t.Fatalf("insert delivery: %v", err)
	}
}

func TestListDeliveriesPage_KeysetOrderAndLimit(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestSvc(t)
	base := time.Now().Unix()
	for i, id := range []string{"d1", "d2", "d3", "d4", "d5"} {
		insertDelivery(t, s, id, "bvbt_t", "delivered", base+int64(i))
	}
	page1, err := s.ListDeliveriesPage(ctx, "bvbt_t", "", time.Time{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 2 || page1[0].ID != "d5" || page1[1].ID != "d4" {
		t.Fatalf("page1 = %v", deliveryIDs(page1))
	}
	cursor := page1[1].CreatedAt
	page2, err := s.ListDeliveriesPage(ctx, "bvbt_t", "", cursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 2 || page2[0].ID != "d3" || page2[1].ID != "d2" {
		t.Fatalf("page2 = %v", deliveryIDs(page2))
	}
}

func TestListDeliveriesPage_StatusFilter(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestSvc(t)
	base := time.Now().Unix()
	insertDelivery(t, s, "a", "bvbt_t", "delivered", base+1)
	insertDelivery(t, s, "b", "bvbt_t", "dead_letter", base+2)
	got, err := s.ListDeliveriesPage(ctx, "bvbt_t", "dead_letter", time.Time{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("status filter = %v", deliveryIDs(got))
	}
}

func deliveryIDs(ds []Delivery) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.ID
	}
	return out
}
