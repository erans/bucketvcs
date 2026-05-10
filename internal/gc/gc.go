package gc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/gc/marks"
	"github.com/bucketvcs/bucketvcs/internal/gc/sweeps"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// RunOptions configures one Run invocation against one repo.
type RunOptions struct {
	Retention      time.Duration    // sweep candidate retention; defaults to DefaultRetention
	MaxConcurrency int              // MaxConcurrency is reserved for future parallel-sweep implementation; the current implementation processes candidates sequentially.
	MarkOnly       bool             // execute mark phase only
	SweepOnly      bool             // execute sweep phase only against most recent mark
	DryRun         bool             // compute candidates, write nothing, delete nothing
	Logger         *slog.Logger     // optional; defaults to slog.Default()
	Now            func() time.Time // clock injection; defaults to time.Now
}

// RunReport summarizes one Run.
type RunReport struct {
	RepoID          string        // "<tenant>/<repo>"
	MarkID          string        // empty if mark phase did not run
	SweepID         string        // empty if sweep phase did not run
	ManifestVersion uint64
	MarkRecord      marks.Record  // populated when mark phase ran
	SweepRecord     sweeps.Record // populated when sweep phase ran
	MarkDuration    time.Duration
	SweepDuration   time.Duration
	DryRun          bool
}

// Run executes mark and/or sweep phases for one open repo.
func Run(ctx context.Context, s storage.ObjectStore, r *repo.Repo, opts RunOptions) (RunReport, error) {
	if opts.MarkOnly && opts.SweepOnly {
		return RunReport{}, ErrInvalidPhaseCombo
	}
	if opts.Retention <= 0 {
		opts.Retention = DefaultRetention
	}
	if opts.Retention > 0 && opts.Retention < time.Second {
		return RunReport{}, fmt.Errorf("gc: Retention=%s is below the 1s minimum; use Retention >= 1*time.Second", opts.Retention)
	}
	if opts.MaxConcurrency < 1 {
		opts.MaxConcurrency = 1
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	repoIDStr := r.TenantID() + "/" + r.RepoID()
	rep := RunReport{RepoID: repoIDStr, DryRun: opts.DryRun}

	k, err := keys.NewRepo(r.TenantID(), r.RepoID())
	if err != nil {
		return rep, fmt.Errorf("gc: keys: %w", err)
	}

	var markRecord marks.Record

	if !opts.SweepOnly {
		markStart := opts.Now()
		mark, err := RunMark(ctx, s, r, MarkOptions{
			Now:              opts.Now,
			RetentionSeconds: int(opts.Retention.Seconds()),
		})
		if err != nil {
			return rep, fmt.Errorf("gc: mark: %w", err)
		}
		rep.MarkDuration = opts.Now().Sub(markStart)
		rep.ManifestVersion = mark.CurrentManifestVersion
		rep.MarkRecord = mark
		markRecord = mark

		if !opts.DryRun {
			if err := marks.Write(ctx, s, k, mark); err != nil {
				return rep, fmt.Errorf("gc: write mark: %w", err)
			}
			rep.MarkID = mark.MarkID
		}
		if !mark.TxOrphanSweepArmed {
			LogDisarmed(opts.Logger, repoIDStr)
		}
		if !opts.DryRun {
			LogMarkCompleted(opts.Logger, repoIDStr, mark.MarkID, mark.CurrentManifestVersion,
				len(mark.Candidates.TxRecords), len(mark.Candidates.CanonicalPacks), len(mark.Candidates.Indexes))
		} else {
			opts.Logger.Info("gc.mark.dry_run",
				"subsystem", "gc",
				"repo_id", repoIDStr,
				"manifest_version", mark.CurrentManifestVersion,
				"candidate_tx_records", len(mark.Candidates.TxRecords),
				"candidate_canonical_packs", len(mark.Candidates.CanonicalPacks),
				"candidate_indexes", len(mark.Candidates.Indexes),
			)
		}
	}

	if opts.MarkOnly {
		return rep, nil
	}

	if opts.SweepOnly {
		// Load most recent mark from disk.
		latest, err := marks.ReadLatest(ctx, s, k)
		if err != nil {
			if errors.Is(err, marks.ErrNotFound) {
				return rep, ErrNoMarkForSweep
			}
			return rep, fmt.Errorf("gc: read latest mark: %w", err)
		}
		markRecord = latest
		rep.MarkID = latest.MarkID
		rep.MarkRecord = latest
		rep.ManifestVersion = latest.CurrentManifestVersion

		if opts.Retention > 0 {
			pinnedRetention := time.Duration(latest.RetentionSeconds) * time.Second
			if opts.Retention != pinnedRetention {
				opts.Logger.Warn("gc.sweep_only.retention_overridden_by_mark",
					"subsystem", "gc",
					"repo_id", repoIDStr,
					"flag_retention", opts.Retention.String(),
					"mark_retention", pinnedRetention.String(),
					"mark_id", latest.MarkID,
				)
			}
		}
	}

	sweepStart := opts.Now()
	sweep, err := RunSweep(ctx, s, r, markRecord, SweepOptions{
		Now:            opts.Now,
		MaxConcurrency: opts.MaxConcurrency,
		DryRun:         opts.DryRun,
	})
	if err != nil {
		return rep, fmt.Errorf("gc: sweep: %w", err)
	}
	rep.SweepDuration = opts.Now().Sub(sweepStart)
	rep.SweepRecord = sweep

	if !opts.DryRun {
		if err := sweeps.Write(ctx, s, k, sweep); err != nil {
			// The sweep deletes already happened on disk but the audit
			// record is now lost. Emit an audit-tagged log line so the
			// forensic trail isn't completely silent. Operators reading
			// §7.4 of the operator guide should cross-reference this log.
			opts.Logger.Error("gc.sweep.audit_write_failed",
				"audit", true,
				"subsystem", "gc",
				"repo_id", repoIDStr,
				"sweep_id", sweep.SweepID,
				"mark_id", sweep.MarkID,
				"deleted_tx_records", len(sweep.Deleted.TxRecords),
				"deleted_canonical_packs", len(sweep.Deleted.CanonicalPacks),
				"deleted_indexes", len(sweep.Deleted.Indexes),
				"error", err.Error(),
			)
			return rep, fmt.Errorf("gc: write sweep (deletes already executed): %w", err)
		}
		rep.SweepID = sweep.SweepID
		if err := PruneMarks(ctx, s, k, DefaultMarkRecordRetention); err != nil {
			// Prune is housekeeping; failures here do not invalidate the
			// sweep that just succeeded. Log as a warning and continue. A
			// future run will retry the prune.
			opts.Logger.Warn("gc.prune.failed",
				"subsystem", "gc",
				"repo_id", repoIDStr,
				"error", err.Error(),
			)
		}
	}

	skipped := countSkipped(sweep.Skipped)
	if !opts.DryRun {
		LogSweepCompleted(opts.Logger, repoIDStr, sweep.SweepID, sweep.MarkID,
			len(sweep.Deleted.TxRecords), len(sweep.Deleted.CanonicalPacks), len(sweep.Deleted.Indexes),
			skipped["revived"], skipped["retention_not_met"], skipped["version_mismatch"],
			skipped["not_found"], skipped["tx_sweep_disarmed"],
			len(sweep.Errors),
		)
	} else {
		opts.Logger.Info("gc.sweep.dry_run",
			"subsystem", "gc",
			"repo_id", repoIDStr,
			"would_delete_tx_records", len(sweep.Deleted.TxRecords),
			"would_delete_canonical_packs", len(sweep.Deleted.CanonicalPacks),
			"would_delete_indexes", len(sweep.Deleted.Indexes),
			"skipped_revived", skipped["revived"],
			"skipped_retention_not_met", skipped["retention_not_met"],
			"skipped_version_mismatch", skipped["version_mismatch"],
			"skipped_not_found", skipped["not_found"],
			"skipped_tx_sweep_disarmed", skipped["tx_sweep_disarmed"],
			"errors_count", len(sweep.Errors),
		)
	}
	return rep, nil
}

func countSkipped(entries []sweeps.SkippedEntry) map[string]int {
	out := map[string]int{}
	for _, e := range entries {
		out[e.Reason]++
	}
	return out
}
