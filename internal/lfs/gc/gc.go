package gc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/lfs"
	"github.com/bucketvcs/bucketvcs/internal/maintenance"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/refstore"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// DefaultRetention mirrors internal/gc.DefaultRetention (7 days).
const DefaultRetention = 7 * 24 * time.Hour

// MarkOptions configures RunMark.
type MarkOptions struct {
	Now              func() time.Time
	RetentionSeconds int
	BareDir          string       // if non-empty, skip Materialize and use this pre-built mirror (tests).
	Logger           *slog.Logger // Task 4 wires metrics/audit via this field.
	DryRun           bool         // surfaced in the lfs.gc.mark audit event so consumers know a mark wasn't persisted.
}

// SweepOptions configures RunSweep.
type SweepOptions struct {
	Now    func() time.Time
	DryRun bool
	Logger *slog.Logger // Task 4 wires metrics/audit via this field.
}

// RunReport aggregates a mark + sweep run.
type RunReport struct {
	RepoID        string        `json:"repo_id"` // "<tenant>/<repo>"
	MarkRecord    MarkRecord    `json:"mark_record,omitempty"`
	SweepReport   SweepReport   `json:"sweep_report,omitempty"`
	MarkDuration  time.Duration `json:"mark_duration"`
	SweepDuration time.Duration `json:"sweep_duration"`
	DryRun        bool          `json:"dry_run,omitempty"`
}

// RunMark performs one mark phase for the open repo. It materializes
// the mirror (or reuses opts.BareDir), builds the live set, lists the
// LFS storage prefix, diffs to find orphans, and writes the mark
// record carrying forward FirstSeenUnreferencedAt across runs.
func RunMark(ctx context.Context, store storage.ObjectStore, r *repo.Repo, opts MarkOptions) (MarkRecord, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.RetentionSeconds <= 0 {
		opts.RetentionSeconds = int(DefaultRetention.Seconds())
	}
	startedAt := opts.Now().UTC()
	view, err := r.ReadRoot(ctx)
	if err != nil {
		return MarkRecord{}, fmt.Errorf("lfs/gc: read root: %w", err)
	}
	body, err := manifest.UnmarshalBody(view.Body)
	if err != nil {
		return MarkRecord{}, fmt.Errorf("lfs/gc: unmarshal body: %w", err)
	}

	bareDir := opts.BareDir
	if bareDir == "" {
		tmp, err := os.MkdirTemp("", "lfs-gc-mirror-")
		if err != nil {
			return MarkRecord{}, fmt.Errorf("lfs/gc: mkdir tmp: %w", err)
		}
		defer os.RemoveAll(tmp)
		refs, err := loadRefsFromBody(ctx, store, r.TenantID(), r.RepoID(), &body)
		if err != nil {
			return MarkRecord{}, err
		}
		// Convert manifest packs to maintenance.PackRef.
		packs := make([]maintenance.PackRef, 0, len(body.Packs))
		for _, p := range body.Packs {
			packs = append(packs, maintenance.PackRef{PackKey: p.PackKey, IdxKey: p.IdxKey})
		}
		if err := maintenance.Materialize(ctx, store, maintenance.MaterializeInput{
			BareDir:       tmp,
			Packs:         packs,
			Refs:          refs,
			DefaultBranch: body.DefaultBranch,
		}); err != nil {
			return MarkRecord{}, fmt.Errorf("lfs/gc: materialize: %w", err)
		}
		bareDir = filepath.Join(tmp, "bare.git")
	}

	live, err := BuildLiveSet(ctx, bareDir)
	if err != nil {
		return MarkRecord{}, fmt.Errorf("lfs/gc: build live set: %w", err)
	}

	// List the LFS storage prefix.
	prefix := lfs.RepoLFSPrefix(r.TenantID(), r.RepoID())
	var storageObjects []storage.ObjectMetadata
	var token string
	for {
		page, err := store.List(ctx, prefix, &storage.ListOptions{ContinuationToken: token})
		if err != nil {
			return MarkRecord{}, fmt.Errorf("lfs/gc: list lfs objects: %w", err)
		}
		storageObjects = append(storageObjects, page.Objects...)
		if page.NextToken == "" {
			break
		}
		token = page.NextToken
	}

	// Read the previous mark once: used to (a) carry forward
	// FirstSeenUnreferencedAt for orphans we've seen before, and (b)
	// chain PreviousMarkID. ErrNoMarks (no prior mark exists) is not
	// an error — it's the first run for this repo.
	var prevMark MarkRecord
	var prevFound bool
	if p, perr := ReadLatestMark(ctx, store, r.TenantID(), r.RepoID()); perr == nil {
		prevMark = p
		prevFound = true
	} else if !errors.Is(perr, ErrNoMarks) {
		return MarkRecord{}, fmt.Errorf("lfs/gc: read previous mark: %w", perr)
	}
	carryForward := map[string]time.Time{}
	if prevFound {
		for _, c := range prevMark.Candidates {
			carryForward[c.OID] = c.FirstSeenUnreferencedAt
		}
	}

	// Diff: any LFS object NOT in liveset is a candidate. Initialize as
	// empty (non-nil) so the persisted MarkRecord JSON has
	// "candidates": [] rather than "candidates": null on zero-orphan
	// runs — keeps jq pipelines simple.
	candidates := []MarkCandidate{}
	for _, obj := range storageObjects {
		oid := oidFromLFSKey(obj.Key, prefix)
		if oid == "" {
			continue
		}
		if _, alive := live[oid]; alive {
			continue
		}
		first, ok := carryForward[oid]
		if !ok {
			first = startedAt
		}
		candidates = append(candidates, MarkCandidate{
			OID:                     oid,
			Key:                     obj.Key,
			SizeBytes:               obj.Size,
			FirstSeenUnreferencedAt: first,
		})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].OID < candidates[j].OID })

	rec := MarkRecord{
		SchemaVersion:         1,
		MarkID:                NewMarkID(startedAt),
		PreviousMarkID:        prevMark.MarkID, // empty when !prevFound
		StartedAt:             startedAt,
		CompletedAt:           opts.Now().UTC(),
		ManifestVersionAtMark: view.Header.ManifestVersion,
		RetentionSeconds:      opts.RetentionSeconds,
		Candidates:            candidates,
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	// Metric fires here: it counts what the mark phase found in
	// memory, independent of whether the caller persists the mark
	// record. The audit event (lfs.gc.mark) is emitted by the caller
	// AFTER WriteMark succeeds, so the audit stream never claims a
	// mark exists on disk when WriteMark may have failed.
	lfs.EmitGCObjectsMarkedMetric(ctx, logger, "candidate", int64(len(rec.Candidates)))
	return rec, nil
}

// RunSweep applies retention to the given mark and deletes candidates
// whose retention window has elapsed. Writes a SweepReport.
func RunSweep(ctx context.Context, store storage.ObjectStore, r *repo.Repo, mark MarkRecord, opts SweepOptions) (SweepReport, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	startedAt := opts.Now().UTC()
	retention := time.Duration(mark.RetentionSeconds) * time.Second
	if retention <= 0 {
		retention = DefaultRetention
	}
	deletable, skipped := ApplyRetention(mark.Candidates, startedAt, retention)

	report := SweepReport{
		SweepID:          NewSweepID(startedAt),
		MarkID:           mark.MarkID,
		StartedAt:        startedAt,
		DryRun:           opts.DryRun,
		SkippedRetention: len(skipped),
	}
	for _, c := range deletable {
		if opts.DryRun {
			report.DeletedCount++
			report.DeletedBytes += c.SizeBytes
			continue
		}
		// M8 Head-then-Delete pattern: fetch current version first to
		// avoid passing the zero-value ObjectVersion to DeleteIfVersionMatches
		// (which would always fail with ErrVersionMismatch on real objects).
		meta, herr := store.Head(ctx, c.Key)
		if herr != nil {
			if errors.Is(herr, storage.ErrNotFound) {
				// Already deleted (idempotent sweep). Count as success.
				report.DeletedCount++
				report.DeletedBytes += c.SizeBytes
				continue
			}
			report.Errors = append(report.Errors, SweepError{OID: c.OID, Err: herr.Error()})
			continue
		}
		if derr := store.DeleteIfVersionMatches(ctx, c.Key, meta.Version); derr != nil {
			switch {
			case errors.Is(derr, storage.ErrNotFound):
				// Concurrent deleter beat us; count as success.
				report.DeletedCount++
				report.DeletedBytes += c.SizeBytes
			case errors.Is(derr, storage.ErrVersionMismatch):
				// Modified between Head and Delete; skip and let next sweep retry.
				report.SkippedConcurrent++
			default:
				report.Errors = append(report.Errors, SweepError{OID: c.OID, Err: derr.Error()})
			}
			continue
		}
		report.DeletedCount++
		report.DeletedBytes += c.SizeBytes
	}
	report.CompletedAt = opts.Now().UTC()
	// Emit metrics + audit BEFORE WriteSweep. They describe the
	// reclaim work the sweep already performed in memory + storage;
	// if WriteSweep itself fails, the deletions have still happened
	// and the operator needs the observability signal to triage.
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	lfs.EmitGCObjectsSweptMetric(ctx, logger, "deleted", int64(report.DeletedCount))
	lfs.EmitGCObjectsSweptMetric(ctx, logger, "skipped_retention", int64(report.SkippedRetention))
	lfs.EmitGCObjectsSweptMetric(ctx, logger, "skipped_concurrent", int64(report.SkippedConcurrent))
	lfs.EmitGCObjectsSweptMetric(ctx, logger, "error", int64(len(report.Errors)))
	lfs.EmitGCBytesSweptMetric(ctx, logger, report.DeletedBytes)
	lfs.EmitLFSGCSweep(ctx, logger, r.TenantID()+"/"+r.RepoID(), mark.MarkID, report.SweepID,
		report.DeletedCount, report.SkippedRetention, report.SkippedConcurrent, len(report.Errors),
		report.DeletedBytes, report.DryRun)
	if !opts.DryRun {
		if err := WriteSweep(ctx, store, r.TenantID(), r.RepoID(), report); err != nil {
			return report, err
		}
	}
	return report, nil
}

// oidFromLFSKey extracts the 64-hex OID from the key suffix under the
// LFS prefix. Returns "" if the suffix isn't a valid OID.
func oidFromLFSKey(key, prefix string) string {
	if len(key) < len(prefix)+64 {
		return ""
	}
	if key[:len(prefix)] != prefix {
		return ""
	}
	tail := key[len(prefix):]
	if len(tail) != 64 || !isLowerHex64(tail) {
		return ""
	}
	return tail
}

// loadRefsFromBody enumerates every ref in the manifest body, handling
// both inline (M0–M11, refs in body.Refs) and sharded (M12 v2, refs
// across body.RefShards + per-shard storage objects) layouts.
//
// CRITICAL for LFS GC correctness: a v2 sharded repo has body.Refs
// empty by manifest invariant. If the mark phase only reads body.Refs,
// the materialized mirror has no refs, `git rev-list --all` walks zero
// commits, BuildLiveSet returns an empty set, and after retention
// every LFS object in the repo is swept — silent data loss for any
// user who has run `bucketvcs reshard-refs`. Always go through
// refstore.List so the right layout is used.
func loadRefsFromBody(ctx context.Context, store storage.ObjectStore, tenantID, repoID string, body *manifest.Body) (map[string]string, error) {
	k, err := keys.NewRepo(tenantID, repoID)
	if err != nil {
		return nil, fmt.Errorf("lfs/gc: build repo keys: %w", err)
	}
	rs, err := refstore.New(ctx, store, k, body)
	if err != nil {
		return nil, fmt.Errorf("lfs/gc: open refstore: %w", err)
	}
	refs, err := rs.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("lfs/gc: list refs: %w", err)
	}
	return refs, nil
}
