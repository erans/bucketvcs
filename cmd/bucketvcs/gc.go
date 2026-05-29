package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/gc"
	"github.com/bucketvcs/bucketvcs/internal/lfs"
	lfsgc "github.com/bucketvcs/bucketvcs/internal/lfs/gc"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// runGC is the gc subcommand entry point. Returns process exit code:
//
//	0 — clean run (no errors, no version_mismatch)
//	1 — operational error (store unreachable, GC failure, per-key sweep errors)
//	2 — usage error or version_mismatch (bad flags, version_mismatch skips left work behind)
func runGC(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stdout, gcUsage)
	}

	storeURL := fs.String("store", "", "Storage URL (required)")
	repoFlag := fs.String("repo", "", "<tenant>/<repo> (mutually exclusive with --all-repos)")
	allRepos := fs.Bool("all-repos", false, "Process every repo discovered under tenants/*/repos/*")
	retention := fs.Duration("retention", gc.DefaultRetention, "Sweep candidate retention window")
	maxConcurrency := fs.Int("max-concurrency", 1, "RESERVED for future parallel sweep; currently no-op (sequential)")
	markOnly := fs.Bool("mark-only", false, "Run mark phase only; skip sweep")
	sweepOnly := fs.Bool("sweep-only", false, "Skip mark phase; sweep most recent mark")
	dryRun := fs.Bool("dry-run", false, "Compute candidates, write nothing, delete nothing")
	lfsEnabled := fs.Bool("lfs", false, "Run LFS GC (replaces Git-objects GC unless --include-git-objects is also set)")
	includeGitObjects := fs.Bool("include-git-objects", false, "When --lfs is set, also run Git-object GC (default is LFS-only)")
	format := fs.String("format", "text", "Output format: text|json")
	help := fs.Bool("help", false, "Show this help")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *help {
		fmt.Fprint(stdout, gcUsage)
		return 0
	}

	if *storeURL == "" {
		fmt.Fprintln(stderr, "gc: --store is required")
		return 2
	}
	if (*repoFlag != "") == *allRepos {
		fmt.Fprintln(stderr, "gc: exactly one of --repo or --all-repos is required")
		return 2
	}
	if *markOnly && *sweepOnly {
		fmt.Fprintln(stderr, "gc: --mark-only and --sweep-only are mutually exclusive")
		return 2
	}
	if *includeGitObjects && !*lfsEnabled {
		fmt.Fprintln(stderr, "gc: --include-git-objects requires --lfs (default invocation already runs Git GC)")
		return 2
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "gc: --format must be text or json (got %q)\n", *format)
		return 2
	}
	if *retention < 0 {
		fmt.Fprintf(stderr, "gc: --retention=%s is negative; use --retention=1s as the floor or omit the flag for the 7d default.\n", *retention)
		return 2
	}
	if *retention > 0 && *retention < time.Second {
		fmt.Fprintf(stderr, "gc: --retention=%s is below the 1s minimum; sub-second values are silently rounded to 0 and the default 7d window applies. Use --retention=1s as the floor.\n", *retention)
		return 2
	}
	if *retention > 0 && gc.ShouldWarnRetention(*retention) {
		fmt.Fprintln(stderr, gc.RetentionWarning(*retention))
	}

	store, err := openStore(*storeURL)
	if err != nil {
		fmt.Fprintf(stderr, "gc: open store: %v\n", err)
		return 1
	}
	defer closeStore(store)

	logger := slog.New(slog.NewTextHandler(stderr, nil))
	opts := gc.RunOptions{
		Retention:      *retention,
		MaxConcurrency: *maxConcurrency,
		MarkOnly:       *markOnly,
		SweepOnly:      *sweepOnly,
		DryRun:         *dryRun,
		Logger:         logger,
		Now:            time.Now,
	}

	var refs []gc.RepoRef
	if *allRepos {
		refs, err = gc.DiscoverRepos(ctx, store)
		if err != nil {
			fmt.Fprintf(stderr, "gc: discover repos: %v\n", err)
			return 1
		}
	} else {
		tenant, repoID, err := splitTenantRepo(*repoFlag)
		if err != nil {
			fmt.Fprintf(stderr, "gc: %v\n", err)
			return 2
		}
		refs = []gc.RepoRef{{TenantID: tenant, RepoID: repoID}}
	}

	var (
		anyError    bool
		anyMismatch bool
	)
	runGitGC := !*lfsEnabled || *includeGitObjects
	runLFSGC := *lfsEnabled
	for _, ref := range refs {
		r, err := repo.Open(ctx, store, ref.TenantID, ref.RepoID)
		if err != nil {
			fmt.Fprintf(stderr, "gc: open %s/%s: %v\n", ref.TenantID, ref.RepoID, err)
			if !*allRepos {
				return 1
			}
			anyError = true
			continue
		}
		var (
			gitReport *gc.RunReport
			lfsReport *lfsgc.RunReport
		)
		if runGitGC {
			rep, err := gc.Run(ctx, store, r, opts)
			if err != nil {
				fmt.Fprintf(stderr, "gc: run %s/%s: %v\n", ref.TenantID, ref.RepoID, err)
				if !*allRepos {
					// Single-repo mode: preserve the M0/M8 fail-fast
					// behavior — exit immediately on Git GC error,
					// even when --lfs --include-git-objects asked for
					// both phases. This diverges from the --all-repos
					// fall-through below: --all-repos batches across
					// many repos and silently dropping LFS reclamation
					// for one is worse than the single-repo case
					// where the operator can manually re-run with
					// --lfs alone. The asymmetry is intentional; an
					// operator who explicitly wants both phases in
					// the failure case should rerun the failed phase
					// in a follow-up command.
					return 1
				}
				anyError = true
				// In --all-repos mode, fall through to the LFS phase
				// (if enabled): the two phases operate on disjoint
				// storage prefixes, so a Git GC failure should not
				// silently block LFS reclamation for this repo.
			} else {
				gitReport = &rep
			}
		}
		if runLFSGC {
			rep, err := runLFSPhase(ctx, store, r, *markOnly, *sweepOnly, *dryRun, *retention, logger)
			if err != nil {
				fmt.Fprintf(stderr, "gc: lfs %s/%s: %v\n", ref.TenantID, ref.RepoID, err)
				if !*allRepos {
					// Single-repo fail-fast. The Git GC phase above
					// may have already persisted deletions; emit its
					// report (if any) so the operator has an audit
					// trail of what Git GC actually did before the
					// LFS error aborted the run.
					if gitReport != nil {
						_ = emitCombinedReport(stdout, *format, ref, gitReport, nil)
					}
					return 1
				}
				anyError = true
			} else {
				lfsReport = &rep
			}
		}
		if gitReport == nil && lfsReport == nil {
			// Both phases (or the only enabled phase) failed; per-repo
			// errors already logged. Nothing to emit.
			continue
		}
		// Tally exit-code-relevant signals BEFORE emit so a stdout
		// glitch on one repo can never cost us version_mismatch or
		// per-phase error tracking. A future edit to the emit path
		// can't accidentally drop signals it doesn't know to set.
		if gitReport != nil {
			if len(gitReport.SweepRecord.Errors) > 0 {
				anyError = true
			}
			for _, sk := range gitReport.SweepRecord.Skipped {
				if sk.Reason == "version_mismatch" {
					anyMismatch = true
				}
			}
		}
		if lfsReport != nil {
			if len(lfsReport.SweepReport.Errors) > 0 {
				anyError = true
			}
			// LFS SkippedConcurrent is the analog of git's
			// version_mismatch: the candidate is left behind for the
			// next sweep to retry. Propagate to anyMismatch so monitoring
			// scripts that key off exit code 2 see the "transient
			// retry signal" same as git's path.
			if lfsReport.SweepReport.SkippedConcurrent > 0 {
				anyMismatch = true
			}
		}
		if err := emitCombinedReport(stdout, *format, ref, gitReport, lfsReport); err != nil {
			fmt.Fprintf(stderr, "gc: emit report for %s/%s: %v\n", ref.TenantID, ref.RepoID, err)
			if !*allRepos {
				return 1
			}
			anyError = true
			// Continue: a stdout glitch on one repo shouldn't abort the run.
		}
	}

	// In --all-repos summary, anyError takes priority over anyMismatch:
	// operational failures are louder than left-work-behind. If both occur
	// in the same run, exit 1 wins; the per-repo logs above still show the
	// version_mismatch counts for operator triage.
	switch {
	case anyError:
		return 1
	case anyMismatch:
		return 2
	default:
		return 0
	}
}

func emitCombinedReport(w io.Writer, format string, ref gc.RepoRef, git *gc.RunReport, lfs *lfsgc.RunReport) error {
	// Both git.RunReport.RepoID and lfsgc.RunReport.RepoID are
	// canonically "<tenant>/<repo>" (set by gc.Run and runLFSPhase
	// respectively). Source repo_id from the report when we have one
	// — that way the JSON and text emitters can never disagree on the
	// repo_id field if the canonical format ever changes. Fall back
	// to ref only when neither report is populated (unreachable in
	// the current loop, kept as a safety net).
	repoID := ref.TenantID + "/" + ref.RepoID
	switch {
	case git != nil && git.RepoID != "":
		repoID = git.RepoID
	case lfs != nil && lfs.RepoID != "":
		repoID = lfs.RepoID
	}
	switch format {
	case "json":
		out := map[string]any{
			"repo_id": repoID,
		}
		if git != nil {
			deleted := git.SweepRecord.Deleted
			if deleted.TxRecords == nil {
				deleted.TxRecords = []string{}
			}
			if deleted.CanonicalPacks == nil {
				deleted.CanonicalPacks = []string{}
			}
			if deleted.Indexes == nil {
				deleted.Indexes = []string{}
			}
			out["mark_id"] = git.MarkID
			out["sweep_id"] = git.SweepID
			out["manifest_version"] = git.ManifestVersion
			out["deleted"] = deleted
			out["skipped_count"] = len(git.SweepRecord.Skipped)
			out["errors_count"] = len(git.SweepRecord.Errors)
			out["mark_duration_seconds"] = git.MarkDuration.Seconds()
			out["sweep_duration_seconds"] = git.SweepDuration.Seconds()
			out["dry_run"] = git.DryRun
		}
		if lfs != nil {
			// Build the LFS sub-document explicitly so durations are
			// emitted in seconds (parity with the Git fields above).
			// The default json tags on lfsgc.RunReport serialize
			// time.Duration as nanoseconds — emitting both side by
			// side would create a 1e9 unit mismatch with no naming
			// hint, breaking jq pipelines that sum the two timers.
			// Inner RepoID is omitted (redundant with the top-level
			// repo_id) for the same reason: a single authoritative
			// key per identifier.
			lfsOut := map[string]any{
				"mark_id":                lfs.MarkRecord.MarkID,
				"sweep_id":               lfs.SweepReport.SweepID,
				"candidates_count":       len(lfs.MarkRecord.Candidates),
				"deleted_count":          lfs.SweepReport.DeletedCount,
				"deleted_bytes":          lfs.SweepReport.DeletedBytes,
				"skipped_retention":      lfs.SweepReport.SkippedRetention,
				"skipped_concurrent":     lfs.SweepReport.SkippedConcurrent,
				"errors_count":           len(lfs.SweepReport.Errors),
				"mark_duration_seconds":  lfs.MarkDuration.Seconds(),
				"sweep_duration_seconds": lfs.SweepDuration.Seconds(),
				"dry_run":                lfs.DryRun,
			}
			out["lfs"] = lfsOut
		}
		return json.NewEncoder(w).Encode(out)
	default:
		if git != nil {
			writeGitReportText(w, *git)
		}
		if lfs != nil {
			writeLFSReportText(w, *lfs)
		}
		return nil
	}
}

func writeGitReportText(w io.Writer, r gc.RunReport) {
	if r.DryRun {
		fmt.Fprintf(w, "DRY RUN — repo %s @ manifest v%d\n", r.RepoID, r.ManifestVersion)
	} else {
		fmt.Fprintf(w, "repo %s @ manifest v%d\n", r.RepoID, r.ManifestVersion)
	}
	if r.MarkRecord.MarkID != "" {
		fmt.Fprintf(w, "  mark    %s   candidates: tx=%d packs=%d indexes=%d  (%s)\n",
			r.MarkRecord.MarkID,
			len(r.MarkRecord.Candidates.TxRecords),
			len(r.MarkRecord.Candidates.CanonicalPacks),
			len(r.MarkRecord.Candidates.Indexes),
			r.MarkDuration.Round(time.Millisecond),
		)
	}
	if r.SweepRecord.SweepID != "" {
		s := r.SweepRecord
		byReason := map[string]int{}
		for _, sk := range s.Skipped {
			byReason[sk.Reason]++
		}
		deletedLabel := "deleted"
		if r.DryRun {
			deletedLabel = "would-delete"
		}
		fmt.Fprintf(w, "  sweep   %s   %s: tx=%d packs=%d indexes=%d\n",
			s.SweepID, deletedLabel,
			len(s.Deleted.TxRecords), len(s.Deleted.CanonicalPacks), len(s.Deleted.Indexes))
		fmt.Fprintf(w, "                       skipped: revived=%d retention=%d vmismatch=%d notfound=%d disarmed=%d\n",
			byReason["revived"], byReason["retention_not_met"], byReason["version_mismatch"], byReason["not_found"], byReason["tx_sweep_disarmed"])
		fmt.Fprintf(w, "                       errors: %d  (%s)\n", len(s.Errors), r.SweepDuration.Round(time.Millisecond))
	}
}

func writeLFSReportText(w io.Writer, r lfsgc.RunReport) {
	if r.DryRun {
		fmt.Fprintf(w, "DRY RUN — LFS GC — repo %s\n", r.RepoID)
	} else {
		fmt.Fprintf(w, "LFS GC — repo %s\n", r.RepoID)
	}
	// No mark, no sweep — this happens on --sweep-only against a
	// repo with no prior mark (ErrNoMarks is handled as a graceful
	// no-op in runLFSPhase). Surface a clear hint rather than a
	// header-only line that looks like a clean sweep.
	if r.MarkRecord.MarkID == "" && r.SweepReport.SweepID == "" {
		fmt.Fprintln(w, "  (no prior mark on disk; run --mark-only or omit --sweep-only first)")
		return
	}
	if r.MarkRecord.MarkID != "" {
		notPersisted := ""
		// Only suffix "(not persisted)" when the mark phase actually
		// ran in this invocation (r.MarkDuration > 0). For
		// --sweep-only --dry-run, the mark was loaded from disk and
		// IS persisted — the dry-run only suppresses the sweep
		// record, not a non-existent mark phase.
		if r.DryRun && r.MarkDuration > 0 {
			notPersisted = " (not persisted)"
		}
		fmt.Fprintf(w, "  mark    %s   candidates=%d  (%s)%s\n",
			r.MarkRecord.MarkID,
			len(r.MarkRecord.Candidates),
			r.MarkDuration.Round(time.Millisecond),
			notPersisted,
		)
	}
	if r.SweepReport.SweepID != "" {
		s := r.SweepReport
		deletedLabel := "deleted"
		if s.DryRun {
			deletedLabel = "would-delete"
		}
		fmt.Fprintf(w, "  sweep   %s   %s=%d bytes=%d\n",
			s.SweepID, deletedLabel, s.DeletedCount, s.DeletedBytes)
		fmt.Fprintf(w, "                       skipped: retention=%d concurrent=%d\n",
			s.SkippedRetention, s.SkippedConcurrent)
		fmt.Fprintf(w, "                       errors: %d  (%s)\n",
			len(s.Errors), r.SweepDuration.Round(time.Millisecond))
	}
}

// runLFSPhase executes the mark and/or sweep phase of LFS GC for one repo,
// honoring --mark-only, --sweep-only, and --dry-run. Mirrors the gating
// pattern that gc.Run uses for the Git-objects path so the two phases
// behave consistently under a single CLI invocation.
func runLFSPhase(ctx context.Context, store storage.ObjectStore, r *repo.Repo, markOnly, sweepOnly, dryRun bool, retention time.Duration, logger *slog.Logger) (lfsgc.RunReport, error) {
	if logger == nil {
		logger = slog.Default()
	}
	report := lfsgc.RunReport{
		RepoID: r.TenantID() + "/" + r.RepoID(),
		DryRun: dryRun,
	}
	if !sweepOnly {
		markStart := time.Now()
		// retention is validated ≥ 1s upstream. The MarkOptions wire
		// format is integer seconds (persisted on the MarkRecord too),
		// so sub-second-fractional values like 1500ms truncate to 1s
		// here. This diverges from the Git path, which uses
		// time.Duration end-to-end. In practice retention is hours-days,
		// not fractional seconds, so the divergence is academic; if it
		// ever becomes operationally relevant, lfsgc.MarkOptions and
		// MarkRecord schema_version would need to grow time.Duration
		// precision.
		rec, err := lfsgc.RunMark(ctx, store, r, lfsgc.MarkOptions{
			RetentionSeconds: int(retention.Seconds()),
			Logger:           logger,
			DryRun:           dryRun,
		})
		if err != nil {
			return report, fmt.Errorf("mark: %w", err)
		}
		if !dryRun {
			if err := lfsgc.WriteMark(ctx, store, r.TenantID(), r.RepoID(), rec); err != nil {
				return report, fmt.Errorf("write mark: %w", err)
			}
		}
		// Emit the lfs.gc.mark audit event AFTER WriteMark succeeds
		// (or is intentionally skipped in dry-run). RunMark itself
		// emits only the in-memory metric; the audit event signals
		// the mark exists on disk (dry_run=true preserves the
		// "computed but not persisted" semantics).
		lfs.EmitLFSGCMark(ctx, logger, r.TenantID()+"/"+r.RepoID(),
			rec.MarkID, len(rec.Candidates), rec.ManifestVersionAtMark, dryRun)
		report.MarkRecord = rec
		report.MarkDuration = time.Since(markStart)
	}
	if !markOnly {
		sweepStart := time.Now()
		mark := report.MarkRecord
		if mark.MarkID == "" {
			// Only reachable when sweepOnly is true; the mark phase above
			// populates report.MarkRecord otherwise.
			rec, err := lfsgc.ReadLatestMark(ctx, store, r.TenantID(), r.RepoID())
			if err != nil {
				if errors.Is(err, lfsgc.ErrNoMarks) {
					// No prior mark: nothing to sweep. Leave SweepReport empty.
					return report, nil
				}
				return report, fmt.Errorf("read latest mark: %w", err)
			}
			mark = rec
			// Surface the loaded mark in the report so the text/JSON
			// emitters can show which mark the sweep operated against.
			// Without this assignment the sweep-only path would emit
			// an empty mark_id and a count of 0 candidates even though
			// the sweep is operating on a real, loaded mark.
			report.MarkRecord = mark
			// Mirror the Git path's retention-override warning: the
			// retention used by sweep is frozen on the mark, so a flag
			// override is silently ignored. Surface that to the operator.
			if retention > 0 {
				pinned := time.Duration(mark.RetentionSeconds) * time.Second
				if retention != pinned {
					logger.Warn("lfs_gc.sweep_only.retention_overridden_by_mark",
						"subsystem", "lfs_gc",
						"repo_id", r.TenantID()+"/"+r.RepoID(),
						"flag_retention", retention.String(),
						"mark_retention", pinned.String(),
						"mark_id", mark.MarkID,
					)
				}
			}
		}
		sweep, err := lfsgc.RunSweep(ctx, store, r, mark, lfsgc.SweepOptions{
			DryRun: dryRun,
			Logger: logger,
		})
		if err != nil {
			return report, fmt.Errorf("sweep: %w", err)
		}
		report.SweepReport = sweep
		report.SweepDuration = time.Since(sweepStart)
	}
	return report, nil
}

const gcUsage = `Usage: bucketvcs gc --store=<URL> {--repo=<tenant>/<repo> | --all-repos} [flags]

Garbage-collect orphan and unreachable storage from one or more
bucketvcs repos. Runs Git-objects GC by default; pass --lfs to run
the LFS-objects GC instead (or both phases with --lfs --include-git-objects).

Flags:
  --store=<URL>             Storage URL (required, e.g. localfs:/path, s3://bucket, gcs://bucket, azureblob://container)
  --repo=<tenant>/<repo>    Single repo (mutually exclusive with --all-repos)
  --all-repos               Process every repo discovered under tenants/*/repos/*
  --retention=<duration>    Sweep candidate retention window (default 168h; minimum 1s; warns if < 24h)
  --max-concurrency=<n>     RESERVED — currently no-op; sequential sweep (default 1)
  --mark-only               Run mark phase only; skip sweep
  --sweep-only              Skip mark phase; sweep most recent existing mark
  --dry-run                 Compute candidates, write nothing, delete nothing
  --lfs                     Run LFS GC (replaces Git-objects GC unless --include-git-objects is also set)
  --include-git-objects     When --lfs is set, also run Git-objects GC (Git first, then LFS)
  --format=text|json        Output format (default text)
  --help                    Show this help

Examples:
  Run Git-objects GC (default):
    bucketvcs gc --store=URL --repo=tenant/repo

  Run LFS GC only:
    bucketvcs gc --store=URL --repo=tenant/repo --lfs

  Run both phases sequentially (Git, then LFS):
    bucketvcs gc --store=URL --repo=tenant/repo --lfs --include-git-objects

Exit codes:
  0  clean (no errors, no left-behind work)
  1  operational error (store unreachable, GC failure, per-key sweep errors, LFS sweep errors)
  2  usage error or transient skip (bad flags, git version_mismatch, LFS skipped_concurrent — re-run to retry)

See docs/operator-guides/gc.md for retention guidance and the §43.6
race window.
`
