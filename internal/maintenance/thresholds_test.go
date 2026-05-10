package maintenance

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestEvaluate_TotalPackTrigger(t *testing.T) {
	body := manifest.Body{}
	for i := 0; i < 5; i++ {
		body.Packs = append(body.Packs, manifest.PackEntry{PackKey: "K" + string(rune('a'+i))})
	}
	thresh := Thresholds{TotalPackCount: 3}
	rep, err := evaluatePure(body, nil, thresh)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Triggered {
		t.Fatalf("expected triggered; got %+v", rep)
	}
	if !strings.HasPrefix(rep.Reason, "total_pack_count(") {
		t.Errorf("Reason = %q, want total_pack_count(N>M) prefix", rep.Reason)
	}
}

func TestEvaluate_ManifestPackBytesTrigger(t *testing.T) {
	body := manifest.Body{
		Packs: []manifest.PackEntry{{PackKey: "K", PackID: "ABCDEFG"}},
	}
	pb, _ := json.Marshal(body.Packs)
	thresh := Thresholds{ManifestPackBytes: int64(len(pb)) - 1}
	rep, err := evaluatePure(body, nil, thresh)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Triggered || !strings.HasPrefix(rep.Reason, "manifest_pack_bytes(") {
		t.Errorf("Reason = %q triggered = %v, want manifest_pack_bytes(N>M) prefix", rep.Reason, rep.Triggered)
	}
}

func TestEvaluate_NoTriggerIsZeroTrigger(t *testing.T) {
	rep, err := evaluatePure(manifest.Body{}, nil, Thresholds{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Triggered {
		t.Errorf("zero thresholds + empty body should not trigger; got %+v", rep)
	}
}

func TestEvaluate_RecentPackCountUsesObjectStoreMTime(t *testing.T) {
	storeDir := t.TempDir()
	s, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	body := manifest.Body{
		Packs: []manifest.PackEntry{
			{PackKey: "tenants/acme/repos/site/packs/canonical/p1.pack"},
			{PackKey: "tenants/acme/repos/site/packs/canonical/p2.pack"},
		},
	}
	for _, p := range body.Packs {
		if _, err := s.PutIfAbsent(ctx, p.PackKey, bytes.NewReader([]byte("x")), nil); err != nil {
			t.Fatal(err)
		}
	}
	// Use time well after the Put calls so objects are within the recent window
	now := time.Now().Add(time.Second)
	thresh := Thresholds{RecentPackCount: 1}
	rep, err := Evaluate(ctx, s, body, thresh, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if rep.RecentPackCount != 2 {
		t.Errorf("RecentPackCount = %d, want 2", rep.RecentPackCount)
	}
	if !rep.Triggered || !strings.HasPrefix(rep.Reason, "recent_pack_count(") {
		t.Errorf("Reason = %q, want recent_pack_count(N>M) prefix", rep.Reason)
	}
}
