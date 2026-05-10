package maintenance

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Evaluate computes a TriggerReport for body against thresh. "recent"
// pack classification uses storage.ObjectMetadata.ModifiedAt relative
// to (now - recentWindow).
//
// Cheap triggers fire first: TotalPackCount and ManifestPackBytes are
// O(1) on body alone and decide the outcome before any storage I/O.
// RecentPackCount requires one Head per pack and is only computed if
// the cheap triggers haven't already fired AND the threshold is set
// (RecentPackCount > 0). This keeps a 10k-pack `--all-repos` sweep
// from doing 10k * N Head calls when a cheaper trigger would have
// answered the question already. The Head loop also early-exits as
// soon as `recent` exceeds the threshold.
func Evaluate(ctx context.Context, s storage.ObjectStore, body manifest.Body, thresh Thresholds, recentWindow time.Duration, now time.Time) (TriggerReport, error) {
	pb, err := json.Marshal(body.Packs)
	if err != nil {
		return TriggerReport{}, fmt.Errorf("evaluate: marshal packs: %w", err)
	}
	rep := TriggerReport{
		TotalPackCount:    len(body.Packs),
		ManifestPackBytes: int64(len(pb)),
		Thresholds:        thresh,
	}
	if thresh.TotalPackCount > 0 && rep.TotalPackCount > thresh.TotalPackCount {
		rep.Triggered = true
		rep.Reason = fmt.Sprintf("total_pack_count(%d>%d)", rep.TotalPackCount, thresh.TotalPackCount)
		return rep, nil
	}
	if thresh.ManifestPackBytes > 0 && rep.ManifestPackBytes > thresh.ManifestPackBytes {
		rep.Triggered = true
		rep.Reason = fmt.Sprintf("manifest_pack_bytes(%d>%d)", rep.ManifestPackBytes, thresh.ManifestPackBytes)
		return rep, nil
	}
	if thresh.RecentPackCount <= 0 {
		return rep, nil
	}

	cutoff := now.Add(-recentWindow)
	recent := 0
	for _, p := range body.Packs {
		md, err := s.Head(ctx, p.PackKey)
		if err != nil {
			return TriggerReport{}, fmt.Errorf("evaluate: head %s: %w", p.PackKey, err)
		}
		if md.ModifiedAt.After(cutoff) {
			recent++
			if recent > thresh.RecentPackCount {
				rep.RecentPackCount = recent
				rep.Triggered = true
				rep.Reason = fmt.Sprintf("recent_pack_count(%d>%d)", recent, thresh.RecentPackCount)
				return rep, nil
			}
		}
	}
	rep.RecentPackCount = recent
	return rep, nil
}

// evaluatePure is the substrate that pure unit tests exercise. Pass
// recentOverride non-nil to inject a recent-pack count without a
// real object store. Trigger priority matches Evaluate (cheap first):
// total_pack_count, manifest_pack_bytes, recent_pack_count.
func evaluatePure(body manifest.Body, recentOverride *int, thresh Thresholds) (TriggerReport, error) {
	pb, err := json.Marshal(body.Packs)
	if err != nil {
		return TriggerReport{}, fmt.Errorf("evaluate: marshal packs: %w", err)
	}
	rep := TriggerReport{
		TotalPackCount:    len(body.Packs),
		ManifestPackBytes: int64(len(pb)),
		Thresholds:        thresh,
	}
	if recentOverride != nil {
		rep.RecentPackCount = *recentOverride
	}
	switch {
	case thresh.TotalPackCount > 0 && rep.TotalPackCount > thresh.TotalPackCount:
		rep.Triggered = true
		rep.Reason = fmt.Sprintf("total_pack_count(%d>%d)", rep.TotalPackCount, thresh.TotalPackCount)
	case thresh.ManifestPackBytes > 0 && rep.ManifestPackBytes > thresh.ManifestPackBytes:
		rep.Triggered = true
		rep.Reason = fmt.Sprintf("manifest_pack_bytes(%d>%d)", rep.ManifestPackBytes, thresh.ManifestPackBytes)
	case thresh.RecentPackCount > 0 && rep.RecentPackCount > thresh.RecentPackCount:
		rep.Triggered = true
		rep.Reason = fmt.Sprintf("recent_pack_count(%d>%d)", rep.RecentPackCount, thresh.RecentPackCount)
	}
	return rep, nil
}
