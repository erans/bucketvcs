package gc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// SweepReport summarises one RunSweep call. SkippedConcurrent has no
// ,omitempty because operators reading raw sweep records benefit from
// a stable field set across runs (parity with SkippedRetention /
// DeletedCount). Errors retains ,omitempty: it's variable-length and
// a clean sweep's report stays terse.
type SweepReport struct {
	SweepID           string       `json:"sweep_id"`
	MarkID            string       `json:"mark_id"`
	StartedAt         time.Time    `json:"started_at"`
	CompletedAt       time.Time    `json:"completed_at"`
	DryRun            bool         `json:"dry_run"`
	DeletedCount      int          `json:"deleted_count"`
	DeletedBytes      int64        `json:"deleted_bytes"`
	SkippedRetention  int          `json:"skipped_retention"`
	SkippedConcurrent int          `json:"skipped_concurrent"`
	Errors            []SweepError `json:"errors,omitempty"`
}

// SweepError is per-candidate.
type SweepError struct {
	OID string `json:"oid"`
	Err string `json:"err"`
}

func sweepPrefix(tenant, repo string) string {
	return "tenants/" + tenant + "/repos/" + repo + "/gc/lfs-sweeps/"
}

func sweepKey(tenant, repo, sweepID string) string {
	return sweepPrefix(tenant, repo) + sweepID + ".json"
}

// NewSweepID mirrors NewMarkID with an "lfs-sweep-" prefix. Suffix
// is the nanosecond-of-second (0–999_999_999), giving best-effort
// temporal ordering within a UTC second. Strict monotonicity is not
// guaranteed; see NewMarkID for the cross-call collision caveat.
func NewSweepID(now time.Time) string {
	utc := now.UTC()
	return fmt.Sprintf("lfs-sweep-%s-%09d", utc.Format("20060102T150405Z"), utc.Nanosecond())
}

// WriteSweep persists a sweep report. Collision (ErrAlreadyExists)
// logs a warning at slog.Default for the same reason as WriteMark
// (collision could be benign idempotent re-write OR a silent drop of
// a parallel writer's data — the log is the only diagnostic signal).
func WriteSweep(ctx context.Context, store storage.ObjectStore, tenant, repo string, rec SweepReport) error {
	body, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("lfs/gc: marshal sweep: %w", err)
	}
	key := sweepKey(tenant, repo, rec.SweepID)
	if _, err := store.PutIfAbsent(ctx, key, bytes.NewReader(body), nil); err != nil {
		if errors.Is(err, storage.ErrAlreadyExists) {
			slog.Default().Warn("lfs_gc.sweep_id_collision",
				"subsystem", "lfs_gc",
				"repo", tenant+"/"+repo,
				"sweep_id", rec.SweepID,
				"note", "PutIfAbsent collapsed this write — if content matches an existing sweep this is idempotent re-write; if not, the second writer's data was dropped",
			)
			return nil
		}
		return fmt.Errorf("lfs/gc: put sweep %s: %w", key, err)
	}
	return nil
}

// ApplyRetention partitions mark candidates into ones whose retention
// window has elapsed (deletable) and those still inside it (skipped).
// Deletable candidates are sorted by OID for deterministic sweep order.
func ApplyRetention(candidates []MarkCandidate, now time.Time, retention time.Duration) (deletable, skipped []MarkCandidate) {
	cutoff := now.Add(-retention)
	for _, c := range candidates {
		if c.FirstSeenUnreferencedAt.Before(cutoff) || c.FirstSeenUnreferencedAt.Equal(cutoff) {
			deletable = append(deletable, c)
		} else {
			skipped = append(skipped, c)
		}
	}
	sort.Slice(deletable, func(i, j int) bool { return deletable[i].OID < deletable[j].OID })
	return deletable, skipped
}
