package maintenance

import (
	"context"
	"io"
	"log/slog"

	"github.com/bucketvcs/bucketvcs/internal/reachability/deltaindex"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// EvaluateReachabilityCommits performs the IO-bound commit-count threshold
// check. Returns (hit, reason, error). Called by the pipeline only when
// cheaper thresholds did not fire AND thr.ReachabilityDeltaCommits > 0.
//
// Each .bvrd header is 32 bytes; we read only those bytes via GetRange so
// HTTP-backed stores transfer only the header, not the full object body.
//
// A header read failure is treated as a soft degradation: a warning is logged
// and the function returns (false, "", nil) rather than propagating the error.
// The bytes/pushes thresholds already fired or not — one transient read
// failure should not abort the entire maintenance run.
//
// total is tracked as int64 to avoid silent overflow on 32-bit platforms when
// summing NCommits across many deltas.
func EvaluateReachabilityCommits(ctx context.Context, store storage.ObjectStore, body manifest.Body, thr Thresholds) (bool, string, error) {
	if body.Indexes.Reachability == nil || thr.ReachabilityDeltaCommits <= 0 {
		return false, "", nil
	}
	var total int64
	for _, ref := range body.Indexes.Reachability.Deltas {
		header, err := readDeltaHeader(ctx, store, ref.Key)
		if err != nil {
			slog.WarnContext(ctx, "reachability.threshold_io.header_read_failed",
				"key", ref.Key, "err", err)
			// Degrade gracefully: skip this delta rather than failing the
			// whole evaluation. The bytes/pushes thresholds already fired or
			// not; don't escalate a transient read error to maintenance failure.
			return false, "", nil
		}
		total += int64(header.NCommits)
		if total > int64(thr.ReachabilityDeltaCommits) {
			return true, "delta-commits", nil
		}
	}
	return false, "", nil
}

// readDeltaHeader reads the first HeaderSize bytes from the object at
// key and parses them as a .bvrd header. Uses a range-read (GetRange) so
// HTTP-backed stores transfer only the header bytes, not the full object body.
func readDeltaHeader(ctx context.Context, store storage.ObjectStore, key string) (deltaindex.Header, error) {
	rc, err := store.GetRange(ctx, key, 0, int64(deltaindex.HeaderSize-1))
	if err != nil {
		return deltaindex.Header{}, err
	}
	defer rc.Close()
	buf := make([]byte, deltaindex.HeaderSize)
	if _, err := io.ReadFull(rc, buf); err != nil {
		return deltaindex.Header{}, err
	}
	return deltaindex.ParseHeader(buf)
}
