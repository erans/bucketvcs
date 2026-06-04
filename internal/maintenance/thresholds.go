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
//
// Reachability byte/push thresholds are also evaluated cheaply from the
// manifest body. The commit-count threshold is IO-bound and evaluated
// separately via EvaluateReachabilityCommits.
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
	computeBitmapCoverage(body, &rep)
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

	// Reachability cheap checks (bytes + pushes) — no IO.
	evaluateReachabilityPure(body, thresh, &rep)

	if thresh.RecentPackCount <= 0 {
		// No recent-pack IO needed. Bitmap coverage is the lowest-
		// priority repack trigger; check before returning.
		maybeFireBitmapCoverage(thresh, &rep)
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
	maybeFireBitmapCoverage(thresh, &rep)
	return rep, nil
}

// maybeFireBitmapCoverage sets Triggered/Reason from bitmap coverage,
// but only when no higher-priority trigger already fired. The coverage
// percentage was already computed by computeBitmapCoverage.
//
// The early-return on TotalPackCount==0 captures "no canonical packs
// means no missing bitmaps to worry about" — a freshly-created repo
// (no pushes yet) MUST NOT trip the bitmap trigger. This is the
// load-bearing semantic; the field-equality form is just how
// TriggerReport carries the count.
func maybeFireBitmapCoverage(thresh Thresholds, rep *TriggerReport) {
	if thresh.BitmapCoveragePct <= 0 || rep.TotalPackCount == 0 {
		return
	}
	if rep.BitmapCoveragePct < thresh.BitmapCoveragePct && !rep.Triggered {
		rep.Triggered = true
		rep.Reason = fmt.Sprintf("bitmap_coverage(%d%%<%d%%)", rep.BitmapCoveragePct, thresh.BitmapCoveragePct)
	}
}

// evaluatePure is the substrate that pure unit tests exercise. Pass
// recentOverride non-nil to inject a recent-pack count without a
// real object store. Trigger priority matches Evaluate (cheap first):
// total_pack_count, manifest_pack_bytes, recent_pack_count, then
// (lowest priority) bitmap_coverage.
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
	computeBitmapCoverage(body, &rep)
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

	// Reachability cheap checks — only when pack triggers haven't already fired.
	if !rep.Triggered {
		evaluateReachabilityPure(body, thresh, &rep)
	}

	// Bitmap coverage — lowest-priority repack trigger. Shared with
	// the storage-backed Evaluate path so the reason format and the
	// "first trigger wins" semantics are defined in one place.
	maybeFireBitmapCoverage(thresh, &rep)
	return rep, nil
}

// computeBitmapCoverage populates rep.BitmapCoveragePct from body.Packs.
// Coverage is by count (not bytes); 0 packs → 0% (no packs to bitmap).
func computeBitmapCoverage(body manifest.Body, rep *TriggerReport) {
	if len(body.Packs) == 0 {
		rep.BitmapCoveragePct = 0
		return
	}
	with := 0
	for _, p := range body.Packs {
		if p.BitmapKey != "" {
			with++
		}
	}
	rep.BitmapCoveragePct = (with * 100) / len(body.Packs)
}

// evaluateReachabilityPure applies the cheap (bytes + pushes) reachability
// thresholds to rep in place. It is called from both Evaluate and
// evaluatePure after pack triggers have been checked.
func evaluateReachabilityPure(body manifest.Body, thresh Thresholds, rep *TriggerReport) {
	if body.Indexes.Reachability == nil {
		return
	}
	if thresh.ReachabilityDeltaBytes > 0 {
		var totalBytes int64
		for _, ref := range body.Indexes.Reachability.Deltas {
			totalBytes += ref.SizeBytes
		}
		if totalBytes > thresh.ReachabilityDeltaBytes {
			rep.CompactReachability = true
			rep.CompactReachabilityReason = "delta-bytes"
			return
		}
	}
	if thresh.ReachabilityDeltaPushes > 0 {
		if len(body.Indexes.Reachability.Deltas) > thresh.ReachabilityDeltaPushes {
			rep.CompactReachability = true
			rep.CompactReachabilityReason = "delta-pushes"
		}
	}
}
