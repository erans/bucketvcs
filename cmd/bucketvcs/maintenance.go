package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/maintenance"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// parseByteSize parses a human-readable byte size with optional suffix K, M, or G
// (case-insensitive). "0" disables the threshold. Returns an error on invalid input.
//
// Examples: "64M" → 67108864, "1024K" → 1048576, "2G" → 2147483648, "0" → 0.
func parseByteSize(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty byte-size string")
	}
	upper := strings.ToUpper(strings.TrimSpace(s))
	var multiplier int64 = 1
	switch {
	case strings.HasSuffix(upper, "G"):
		multiplier = 1 << 30
		upper = upper[:len(upper)-1]
	case strings.HasSuffix(upper, "M"):
		multiplier = 1 << 20
		upper = upper[:len(upper)-1]
	case strings.HasSuffix(upper, "K"):
		multiplier = 1 << 10
		upper = upper[:len(upper)-1]
	}
	n, err := strconv.ParseInt(upper, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("byte-size %q: %w", s, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("byte-size %q must be non-negative", s)
	}
	if multiplier != 1 && n > math.MaxInt64/multiplier {
		return 0, fmt.Errorf("byte-size %q overflows int64", s)
	}
	return n * multiplier, nil
}

const maintenanceUsage = `usage: bucketvcs maintenance --store=<URL> {--repo=<t>/<r> | --all-repos} [flags]

Run a single full repack against one repo (or every repo discovered
under tenants/*/repos/*). Default thresholds match spec §15.3
recommendations; --force runs unconditionally.

Flags:
  --store=URL                           Storage URL (required)
  --repo=<tenant>/<repo>                Single repo (mutex with --all-repos)
  --all-repos                           Process every discovered repo
  --force                               Skip threshold check
  --dry-run                             Walk + plan only; no writes
  --recent-pack-threshold=N             Default 1000 (0 disables)
  --total-pack-threshold=N              Default 10000 (0 disables)
  --manifest-pack-bytes-threshold=N     Default 8388608 (0 disables)
  --recent-window=DURATION              Default 24h, minimum 1h
  --cas-retry=N                         Default 5
  --output=text|json                    Default text
  --reachability-delta-commits=N        Default 1000 (0 disables)
  --reachability-delta-pushes=N         Default 100 (0 disables)
  --reachability-delta-bytes=SIZE       Default 64M, suffix K/M/G (0 disables)
  --bundle-only                         Run only the bundle-refresh phase (mutex with --no-bundle)
  --no-bundle                           Skip the bundle-refresh phase (mutex with --bundle-only)
  --bundle-commits=N                    Default 100; regenerate bundle when default-branch moved >= N commits (0 disables)
  --bundle-age=DURATION                 Default 24h; regenerate bundle when older than this (0 disables)
  --bundle-default-branch=REF           Override default-branch detection (e.g. refs/heads/main)
  --bitmap-coverage-pct=N               Default 100; force-repack when fewer than N% of canonical packs carry a .bitmap (0 disables, M9.5)
  --help                                Show this help

Exit codes:
  0 success or dry-run completed
  1 at least one repo failed (incl. CAS exhaustion)
  2 invalid flags
`

func runMaintenance(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("maintenance", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, maintenanceUsage) }

	storeURL := fs.String("store", "", "Storage URL (required)")
	repoFlag := fs.String("repo", "", "<tenant>/<repo>")
	allRepos := fs.Bool("all-repos", false, "Process every repo discovered under tenants/*/repos/*")
	force := fs.Bool("force", false, "Skip threshold check")
	dryRun := fs.Bool("dry-run", false, "Walk + plan; no writes")
	recentPackT := fs.Int("recent-pack-threshold", 1000, "")
	totalPackT := fs.Int("total-pack-threshold", 10000, "")
	manifestPackBytesT := fs.Int64("manifest-pack-bytes-threshold", 8<<20, "")
	recentWindow := fs.Duration("recent-window", 24*time.Hour, "")
	casRetry := fs.Int("cas-retry", maintenance.DefaultCASRetry, "")
	output := fs.String("output", "text", "text|json")
	help := fs.Bool("help", false, "")
	fs.BoolVar(help, "h", false, "alias for --help")

	deltaCommits := fs.Int("reachability-delta-commits", 1000, "compact when delta chain exceeds this commit count (0 disables)")
	deltaPushes := fs.Int("reachability-delta-pushes", 100, "compact when delta chain exceeds this push count (0 disables)")
	deltaBytesStr := fs.String("reachability-delta-bytes", "64M", "compact when delta chain exceeds this byte size (suffix K/M/G; 0 disables)")

	bundleOnly := fs.Bool("bundle-only", false, "Run only the bundle-refresh phase (skip repack and compact)")
	noBundle := fs.Bool("no-bundle", false, "Skip the bundle-refresh phase")
	bundleCommits := fs.Int("bundle-commits", 100, "Regenerate bundle when default-branch tip moved by >= N commits since last bundle (0 disables)")
	bundleAge := fs.Duration("bundle-age", 24*time.Hour, "Regenerate bundle when older than this (0 disables)")
	bundleDefaultBranch := fs.String("bundle-default-branch", "", "Override default-branch detection for bundle generation (e.g. refs/heads/main)")

	bitmapCoveragePct := fs.Int("bitmap-coverage-pct", 100, "Minimum percent of canonical packs that must carry a .bitmap before maintenance considers the repo bitmap-healthy. Below this triggers a force-repack. 0 disables.")

	authDBFlag := fs.String("auth-db", "", "Path to auth DB (optional; enables BYOB store lookup for --repo)")
	byobKeyFile := fs.String("byob-encryption-key", "", "Path to BYOB encryption key file (optional)")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *help {
		fmt.Fprint(stdout, maintenanceUsage)
		return 0
	}
	if *repoFlag == "" && !*allRepos {
		fmt.Fprintln(stderr, "maintenance: one of --repo or --all-repos is required")
		return 2
	}
	if *repoFlag != "" && *allRepos {
		fmt.Fprintln(stderr, "maintenance: --repo and --all-repos are mutually exclusive")
		return 2
	}
	if *recentWindow < time.Hour {
		fmt.Fprintf(stderr, "maintenance: --recent-window=%s is below the 1h minimum\n", *recentWindow)
		return 2
	}
	if *output != "text" && *output != "json" {
		fmt.Fprintf(stderr, "maintenance: --output=%q must be text|json\n", *output)
		return 2
	}
	if *deltaCommits < 0 {
		fmt.Fprintln(stderr, "maintenance: --reachability-delta-commits must be >= 0")
		return 2
	}
	if *deltaPushes < 0 {
		fmt.Fprintln(stderr, "maintenance: --reachability-delta-pushes must be >= 0")
		return 2
	}
	deltaBytes, err := parseByteSize(*deltaBytesStr)
	if err != nil {
		fmt.Fprintf(stderr, "maintenance: --reachability-delta-bytes: %v\n", err)
		return 2
	}
	if *bundleOnly && *noBundle {
		fmt.Fprintln(stderr, "maintenance: --bundle-only and --no-bundle are mutually exclusive")
		return 2
	}
	if *bundleCommits < 0 {
		fmt.Fprintln(stderr, "maintenance: --bundle-commits must be >= 0")
		return 2
	}
	if *bundleAge < 0 {
		fmt.Fprintln(stderr, "maintenance: --bundle-age must be >= 0")
		return 2
	}

	var store storage.ObjectStore
	if *repoFlag != "" && *authDBFlag != "" && *byobKeyFile != "" {
		tenantID, _, err2 := splitTenantRepo(*repoFlag)
		if err2 == nil {
			if ts, ok := openByobStore(ctx, tenantID, *authDBFlag, *byobKeyFile, stderr); ok {
				store = ts
			}
		}
	}
	if store == nil {
		if *storeURL == "" {
			fmt.Fprintln(stderr, "maintenance: --store is required")
			return 2
		}
		var err error
		store, err = openStore(*storeURL)
		if err != nil {
			fmt.Fprintf(stderr, "maintenance: open store: %v\n", err)
			return 1
		}
	}
	defer closeStore(store)

	opts := maintenance.RunOptions{
		Thresholds: maintenance.Thresholds{
			RecentPackCount:          *recentPackT,
			TotalPackCount:           *totalPackT,
			ManifestPackBytes:        *manifestPackBytesT,
			ReachabilityDeltaCommits: *deltaCommits,
			ReachabilityDeltaPushes:  *deltaPushes,
			ReachabilityDeltaBytes:   deltaBytes,
			BundleCommits:            *bundleCommits,
			BundleAge:                *bundleAge,
			BitmapCoveragePct:        *bitmapCoveragePct,
		},
		RecentWindow:        *recentWindow,
		CASRetry:            *casRetry,
		Force:               *force,
		DryRun:              *dryRun,
		BundleOnly:          *bundleOnly,
		NoBundle:            *noBundle,
		BundleDefaultBranch: *bundleDefaultBranch,
	}

	if *allRepos {
		return runMaintenanceAll(ctx, store, opts, stdout, stderr, *output)
	}

	tenant, repoID, err := splitTenantRepo(*repoFlag)
	if err != nil {
		fmt.Fprintf(stderr, "maintenance: %v\n", err)
		return 2
	}
	return runMaintenanceOne(ctx, store, tenant, repoID, opts, stdout, stderr, *output)
}

func runMaintenanceOne(ctx context.Context, store storage.ObjectStore, tenantID, repoID string, opts maintenance.RunOptions, stdout, stderr io.Writer, output string) int {
	r, err := repo.Open(ctx, store, tenantID, repoID)
	if err != nil {
		fmt.Fprintf(stderr, "maintenance: open repo %s/%s: %v\n", tenantID, repoID, err)
		return 1
	}
	k, err := keys.NewRepo(tenantID, repoID)
	if err != nil {
		fmt.Fprintf(stderr, "maintenance: keys: %v\n", err)
		return 1
	}
	rep, err := maintenance.Run(ctx, store, r, k, opts)
	if err != nil {
		fmt.Fprintf(stderr, "maintenance: %s/%s: %v\n", tenantID, repoID, err)
		emitMaintenanceReport(stdout, []maintenance.Report{rep}, output)
		return 1
	}
	emitMaintenanceReport(stdout, []maintenance.Report{rep}, output)
	return 0
}

func runMaintenanceAll(ctx context.Context, store storage.ObjectStore, opts maintenance.RunOptions, stdout, stderr io.Writer, output string) int {
	repos, err := maintenance.DiscoverRepos(ctx, store)
	if err != nil {
		fmt.Fprintf(stderr, "maintenance: discover repos: %v\n", err)
		return 1
	}
	exit := 0
	reports := make([]maintenance.Report, 0, len(repos))
	var succeeded, noop, failed int
	for _, ref := range repos {
		repoID := ref.TenantID + "/" + ref.RepoID
		r, err := repo.Open(ctx, store, ref.TenantID, ref.RepoID)
		if err != nil {
			fmt.Fprintf(stderr, "maintenance: open %s: %v\n", repoID, err)
			exit = 1
			failed++
			reports = append(reports, maintenance.Report{
				RepoID:           repoID,
				Outcome:          "failed_other",
				RepackedPackKeys: []string{}, // schema invariant: never null
			})
			continue
		}
		k, err := keys.NewRepo(ref.TenantID, ref.RepoID)
		if err != nil {
			fmt.Fprintf(stderr, "maintenance: keys %s: %v\n", repoID, err)
			exit = 1
			failed++
			// Append a placeholder so reports[] indices align with repos[].
			reports = append(reports, maintenance.Report{
				RepoID:           repoID,
				Outcome:          "failed_other",
				RepackedPackKeys: []string{}, // schema invariant: never null
			})
			continue
		}
		rep, err := maintenance.Run(ctx, store, r, k, opts)
		if err != nil {
			fmt.Fprintf(stderr, "maintenance: %s: %v\n", repoID, err)
			exit = 1
			failed++
		} else {
			switch rep.Outcome {
			case "success", "success_bundle_only":
				succeeded++
			case "noop", "noop_bundle_only":
				noop++
			}
			// runPipeline only returns success/noop variants when err == nil; the
			// failed_* outcomes are paired with a non-nil err and handled
			// in the err != nil arm above.
		}
		reports = append(reports, rep)
	}
	emitMaintenanceReport(stdout, reports, output)
	if output == "text" {
		var bundleGenerated int
		for _, rep := range reports {
			if rep.BundleResult != nil && rep.BundleResult.Generated {
				bundleGenerated++
			}
		}
		fmt.Fprintf(stdout, "summary: processed=%d succeeded=%d noop=%d failed=%d bundle_generated=%d\n",
			len(repos), succeeded, noop, failed, bundleGenerated)
	}
	// Surface buckets that didn't sum to processed — e.g. an outcome
	// value the switch didn't recognize. Emitted in both text and JSON
	// modes (always to stderr) so JSON consumers don't silently miss
	// it. CI scrapers can grep for "summary divergence".
	if succeeded+noop+failed != len(repos) {
		fmt.Fprintf(stderr, "summary divergence: bucket counts (%d) != processed (%d); a new outcome value may have been added without updating the CLI\n",
			succeeded+noop+failed, len(repos))
	}
	return exit
}

func emitMaintenanceReport(w io.Writer, reports []maintenance.Report, output string) {
	if output == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(reports)
		return
	}
	for _, r := range reports {
		marker := ""
		if r.DryRun {
			marker = "[DRY RUN] "
		}
		// Early-failure reports (e.g. Validate failure, repo.Open failure)
		// never read the manifest, so before-counts are zero and the
		// standard "pack_count=0→0" line misleads operators into thinking
		// all packs were lost. Print a placeholder line for those cases.
		if r.ManifestVersionAt == 0 && r.BeforePackCount == 0 && r.Outcome != "noop" && r.Outcome != "success" {
			fmt.Fprintf(w, "%s%s: outcome=%s (no manifest snapshot taken)\n", marker, r.RepoID, r.Outcome)
			continue
		}
		fmt.Fprintf(w, "%s%s: outcome=%s pack_count=%d→%d manifest_pack_bytes=%d→%d cas_attempts=%d duration=%dms",
			marker, r.RepoID, r.Outcome, r.BeforePackCount, r.AfterPackCount,
			r.BeforeManifestPB, r.AfterManifestPB, r.CASAttempts, r.DurationMS)
		if r.TriggerEval.Reason != "" {
			fmt.Fprintf(w, " trigger=%s", r.TriggerEval.Reason)
		}
		fmt.Fprintln(w)
	}
}
