package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/lfs"
	"github.com/bucketvcs/bucketvcs/internal/lfs/quota"
)

const quotaUsage = `Usage: bucketvcs quota <subcommand> [flags]

Subcommands:
  set       --auth-db=<path> --tenant=<t> --limit=<size>
  show      --auth-db=<path> {--tenant=<t> | --all} [--format=text|json]
  reconcile --auth-db=<path> --store=<URL> {--tenant=<t> | --all-tenants} [--dry-run] [--format=text|json]
  clear     --auth-db=<path> --tenant=<t>

Size suffixes for --limit: B (default), KB/KiB, MB/MiB, GB/GiB, TB/TiB.
Decimal (KB/MB/GB/TB) uses 1000; binary (KiB/MiB/GiB/TiB) uses 1024.

Exit codes:
  0  ok
  1  operational error (db unreachable, storage failure, ...)
  2  usage error (bad flags, malformed size, ...)
`

// runQuota dispatches the quota subcommands.
func runQuota(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, quotaUsage)
		return 2
	}
	if args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(stdout, quotaUsage)
		return 0
	}
	switch args[0] {
	case "set":
		return runQuotaSet(ctx, args[1:], stdout, stderr)
	case "show":
		return runQuotaShow(ctx, args[1:], stdout, stderr)
	case "reconcile":
		return runQuotaReconcile(ctx, args[1:], stdout, stderr)
	case "clear":
		return runQuotaClear(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "quota: unknown subcommand %q\n%s", args[0], quotaUsage)
		return 2
	}
}

func runQuotaSet(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("quota set", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	tenant := fs.String("tenant", "", "Tenant ID (required)")
	limit := fs.String("limit", "", "Limit (e.g. 100GiB; required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" || *tenant == "" || *limit == "" {
		fmt.Fprintln(stderr, "quota set: --auth-db, --tenant, --limit required")
		return 2
	}
	bytes, err := parseSize(*limit)
	if err != nil {
		fmt.Fprintf(stderr, "quota set: bad --limit %q: %v\n", *limit, err)
		return 2
	}
	svc, authStore, err := openQuotaSvc(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "quota set: %v\n", err)
		return 1
	}
	defer authStore.Close()
	if err := svc.Set(ctx, *tenant, bytes); err != nil {
		fmt.Fprintf(stderr, "quota set: %v\n", err)
		return 1
	}
	// Refresh the gauge.
	if st, gerr := svc.Get(ctx, *tenant); gerr == nil && st.Exists {
		lfs.EmitQuotaBytesUsedMetric(ctx, slog.Default(), *tenant, st.UsedBytes)
	}
	fmt.Fprintf(stdout, "tenant=%s  limit=%s\n", *tenant, formatSize(bytes))
	return 0
}

func runQuotaShow(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("quota show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	tenant := fs.String("tenant", "", "Tenant ID (mutually exclusive with --all)")
	all := fs.Bool("all", false, "Show every tenant with a quota row")
	format := fs.String("format", "text", "Output format: text|json")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" {
		fmt.Fprintln(stderr, "quota show: --auth-db required")
		return 2
	}
	if (*tenant != "") == *all {
		fmt.Fprintln(stderr, "quota show: exactly one of --tenant or --all required")
		return 2
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "quota show: --format must be text|json (got %q)\n", *format)
		return 2
	}
	svc, authStore, err := openQuotaSvc(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "quota show: %v\n", err)
		return 1
	}
	defer authStore.Close()
	var states []quota.State
	if *all {
		states, err = svc.List(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "quota show: %v\n", err)
			return 1
		}
	} else {
		s, err := svc.Get(ctx, *tenant)
		if err != nil {
			fmt.Fprintf(stderr, "quota show: %v\n", err)
			return 1
		}
		if !s.Exists {
			if *format == "json" {
				_ = json.NewEncoder(stdout).Encode(map[string]any{
					"tenant": *tenant,
					"exists": false,
				})
				return 0
			}
			fmt.Fprintf(stdout, "tenant=%s  (no quota row — unlimited)\n", *tenant)
			return 0
		}
		states = []quota.State{s}
	}
	for _, s := range states {
		if *format == "json" {
			over := int64(0)
			if s.UsedBytes > s.LimitBytes {
				over = s.UsedBytes - s.LimitBytes
			}
			_ = json.NewEncoder(stdout).Encode(map[string]any{
				"tenant":        s.Tenant,
				"limit_bytes":   s.LimitBytes,
				"used_bytes":    s.UsedBytes,
				"over_by_bytes": over,
				"updated_at":    s.UpdatedAt.Format(time.RFC3339),
				"exists":        true,
			})
			continue
		}
		extra := ""
		if s.UsedBytes > s.LimitBytes {
			extra = fmt.Sprintf("  over_by=%s", formatSize(s.UsedBytes-s.LimitBytes))
		}
		fmt.Fprintf(stdout, "tenant=%s  used=%s  limit=%s%s  updated=%s\n",
			s.Tenant, formatSize(s.UsedBytes), formatSize(s.LimitBytes), extra,
			s.UpdatedAt.Format(time.RFC3339))
	}
	return 0
}

func runQuotaReconcile(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("quota reconcile", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	storeURL := fs.String("store", "", "Storage URL (required)")
	tenant := fs.String("tenant", "", "Tenant ID (mutually exclusive with --all-tenants)")
	allTenants := fs.Bool("all-tenants", false, "Reconcile every tenant with a quota row")
	dryRun := fs.Bool("dry-run", false, "Compute drift; do not write")
	format := fs.String("format", "text", "Output format: text|json")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" || *storeURL == "" {
		fmt.Fprintln(stderr, "quota reconcile: --auth-db and --store required")
		return 2
	}
	if (*tenant != "") == *allTenants {
		fmt.Fprintln(stderr, "quota reconcile: exactly one of --tenant or --all-tenants required")
		return 2
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "quota reconcile: --format must be text|json (got %q)\n", *format)
		return 2
	}
	store, err := openStore(*storeURL)
	if err != nil {
		fmt.Fprintf(stderr, "quota reconcile: %v\n", err)
		return 1
	}
	defer closeStore(store)
	svc, authStore, err := openQuotaSvc(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "quota reconcile: %v\n", err)
		return 1
	}
	defer authStore.Close()

	var targets []string
	if *allTenants {
		states, err := svc.List(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "quota reconcile: %v\n", err)
			return 1
		}
		for _, s := range states {
			targets = append(targets, s.Tenant)
		}
	} else {
		targets = []string{*tenant}
	}
	anyError := false
	for _, tn := range targets {
		rep, err := svc.Reconcile(ctx, store, tn, *dryRun)
		if err != nil {
			fmt.Fprintf(stderr, "quota reconcile %s: %v\n", tn, err)
			// In --all-tenants mode, continue across failures so one
			// transient backend error doesn't halt a nightly cron's
			// remaining work. Mirror `bucketvcs gc --all-repos`'s
			// best-effort behavior. Single-tenant mode fail-fasts.
			if !*allTenants {
				return 1
			}
			anyError = true
			continue
		}
		lfs.EmitLFSQuotaReconcile(ctx, slog.Default(), rep.Tenant, rep.BeforeBytes, rep.AfterBytes, rep.DriftBytes, rep.DryRun)
		if !rep.DryRun {
			lfs.EmitQuotaBytesUsedMetric(ctx, slog.Default(), rep.Tenant, rep.AfterBytes)
		}
		if *format == "json" {
			_ = json.NewEncoder(stdout).Encode(map[string]any{
				"tenant":       rep.Tenant,
				"before_bytes": rep.BeforeBytes,
				"after_bytes":  rep.AfterBytes,
				"drift_bytes":  rep.DriftBytes,
				"dry_run":      rep.DryRun,
			})
			continue
		}
		note := ""
		if rep.DriftBytes > 0 {
			note = "  (counter was under-reporting)"
		} else if rep.DriftBytes < 0 {
			note = "  (counter was over-reporting)"
		}
		dryNote := ""
		if rep.DryRun {
			dryNote = "  [dry-run; not persisted]"
		}
		fmt.Fprintf(stdout, "tenant=%s  before=%s  after=%s  drift=%s%s%s\n",
			rep.Tenant, formatSize(rep.BeforeBytes), formatSize(rep.AfterBytes),
			formatSignedSize(rep.DriftBytes), note, dryNote)
	}
	if anyError {
		return 1
	}
	return 0
}

func runQuotaClear(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("quota clear", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	tenant := fs.String("tenant", "", "Tenant ID (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" || *tenant == "" {
		fmt.Fprintln(stderr, "quota clear: --auth-db and --tenant required")
		return 2
	}
	svc, authStore, err := openQuotaSvc(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "quota clear: %v\n", err)
		return 1
	}
	defer authStore.Close()
	if err := svc.Clear(ctx, *tenant); err != nil {
		fmt.Fprintf(stderr, "quota clear: %v\n", err)
		return 1
	}
	lfs.EmitQuotaBytesUsedMetric(ctx, slog.Default(), *tenant, 0)
	fmt.Fprintf(stdout, "tenant=%s  cleared (no quota; unlimited)\n", *tenant)
	return 0
}

// openQuotaSvc opens the authdb at path, runs migrations via
// sqlitestore.Open, and returns a Service plus the *sqlitestore.Store
// (caller must Close). Returning the Store wrapper rather than the
// raw *sql.DB ensures any sqlitestore-level state (prepared
// statements, write throttle, etc.) is released on Close.
func openQuotaSvc(path string) (*quota.Service, *sqlitestore.Store, error) {
	authStore, err := sqlitestore.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open authdb: %w", err)
	}
	return quota.New(authStore.DB(), nil), authStore, nil
}

// parseSize accepts decimal (KB/MB/GB/TB = 1000) and binary
// (KiB/MiB/GiB/TiB = 1024) suffixes. Bare numbers are bytes.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	type unit struct {
		suffix string
		mult   int64
	}
	units := []unit{
		{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
		{"TB", 1_000_000_000_000}, {"GB", 1_000_000_000}, {"MB", 1_000_000}, {"KB", 1_000},
		{"B", 1},
	}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			n, err := strconv.ParseInt(strings.TrimSuffix(s, u.suffix), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parse %q: %w", s, err)
			}
			if n < 0 {
				return 0, fmt.Errorf("negative value")
			}
			if u.mult > 0 && n > math.MaxInt64/u.mult {
				return 0, fmt.Errorf("value %q overflows int64", s)
			}
			return n * u.mult, nil
		}
	}
	// Bare digit string = bytes.
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %q: %w", s, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("negative value")
	}
	return n, nil
}

// formatSize pretty-prints bytes with the largest binary unit that
// doesn't lose precision; falls back to bare bytes otherwise.
func formatSize(b int64) string {
	if b == math.MinInt64 {
		// -math.MinInt64 overflows int64 (no positive representation
		// for 2^63), which would infinite-recurse through the negative
		// branch below. Unreachable today (used_bytes / limit_bytes
		// have CHECK >= 0, drift cannot reach this range) but cheap
		// defense-in-depth for future raw-int64 callers.
		return "-9223372036854775808B"
	}
	if b < 0 {
		return "-" + formatSize(-b)
	}
	if b == 0 {
		return "0B"
	}
	for _, u := range []struct {
		suffix string
		mult   int64
	}{
		{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
	} {
		if b >= u.mult && b%u.mult == 0 {
			return fmt.Sprintf("%d%s", b/u.mult, u.suffix)
		}
	}
	return fmt.Sprintf("%dB", b)
}

func formatSignedSize(b int64) string {
	if b > 0 {
		return "+" + formatSize(b)
	}
	return formatSize(b)
}
