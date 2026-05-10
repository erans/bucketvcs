package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/gc"
	"github.com/bucketvcs/bucketvcs/internal/repo"
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
		report, err := gc.Run(ctx, store, r, opts)
		if err != nil {
			fmt.Fprintf(stderr, "gc: run %s/%s: %v\n", ref.TenantID, ref.RepoID, err)
			if !*allRepos {
				return 1
			}
			anyError = true
			continue
		}
		if err := emitReport(stdout, *format, report); err != nil {
			fmt.Fprintf(stderr, "gc: emit report for %s/%s: %v\n", ref.TenantID, ref.RepoID, err)
			if !*allRepos {
				return 1
			}
			anyError = true
			// Continue: a stdout glitch on one repo shouldn't abort the run.
		}
		if len(report.SweepRecord.Errors) > 0 {
			anyError = true
		}
		for _, sk := range report.SweepRecord.Skipped {
			if sk.Reason == "version_mismatch" {
				anyMismatch = true
			}
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

func emitReport(w io.Writer, format string, r gc.RunReport) error {
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		deleted := r.SweepRecord.Deleted
		if deleted.TxRecords == nil {
			deleted.TxRecords = []string{}
		}
		if deleted.CanonicalPacks == nil {
			deleted.CanonicalPacks = []string{}
		}
		if deleted.Indexes == nil {
			deleted.Indexes = []string{}
		}
		return enc.Encode(map[string]any{
			"repo_id":                r.RepoID,
			"mark_id":                r.MarkID,
			"sweep_id":               r.SweepID,
			"manifest_version":       r.ManifestVersion,
			"deleted":                deleted,
			"skipped_count":          len(r.SweepRecord.Skipped),
			"errors_count":           len(r.SweepRecord.Errors),
			"mark_duration_seconds":  r.MarkDuration.Seconds(),
			"sweep_duration_seconds": r.SweepDuration.Seconds(),
			"dry_run":                r.DryRun,
		})
	default:
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
		return nil
	}
}

const gcUsage = `Usage: bucketvcs gc --store=<URL> {--repo=<tenant>/<repo> | --all-repos} [flags]

Garbage-collect orphan and unreachable storage from one or more
bucketvcs repos.

Flags:
  --store=<URL>             Storage URL (required, e.g. localfs:/path, s3://bucket, gcs://bucket, azureblob://container)
  --repo=<tenant>/<repo>    Single repo (mutually exclusive with --all-repos)
  --all-repos               Process every repo discovered under tenants/*/repos/*
  --retention=<duration>    Sweep candidate retention window (default 168h; minimum 1s; warns if < 24h)
  --max-concurrency=<n>     RESERVED — currently no-op; sequential sweep (default 1)
  --mark-only               Run mark phase only; skip sweep
  --sweep-only              Skip mark phase; sweep most recent existing mark
  --dry-run                 Compute candidates, write nothing, delete nothing
  --format=text|json        Output format (default text)
  --help                    Show this help

Exit codes:
  0  clean (no errors, no version_mismatch)
  1  operational error (store unreachable, GC failure, per-key sweep errors)
  2  usage error or version_mismatch (bad flags, version_mismatch skips left work behind)

See docs/m8-gc-operator-guide.md for retention guidance and the §43.6
race window.
`
