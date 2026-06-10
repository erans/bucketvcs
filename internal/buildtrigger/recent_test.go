package buildtrigger

import (
	"context"
	"testing"
	"time"
)

func TestRecentDeliveryIDs(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestSvc(t)
	base := time.Now().Unix()
	for i, id := range []string{"d1", "d2", "d3", "d4"} {
		insertDelivery(t, s, id, "bvbt_t", "delivered", base+int64(i))
	}
	got, err := s.RecentDeliveryIDs(ctx, "bvbt_t", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "d4" || got[1] != "d3" {
		t.Fatalf("recent = %v", got)
	}
}

func TestRecentDeliveryIDs_ZeroN(t *testing.T) {
	s, _ := newTestSvc(t)
	got, err := s.RecentDeliveryIDs(context.Background(), "bvbt_t", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty for n=0, got %v", got)
	}
}
