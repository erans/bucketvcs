package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/policy"
)

const policyUsage = `Usage: bucketvcs policy <object> <action> [flags]

Objects + actions:
  refs add    --auth-db=<path> --tenant=<t> --repo=<r> --pattern=<glob>
              [--allow-deletion] [--allow-force-push]
  refs list   --auth-db=<path> --tenant=<t> --repo=<r> [--format=text|json]
  refs remove --auth-db=<path> --tenant=<t> --repo=<r> --pattern=<glob>

Output formats:
  text  one record per line, key=value style.
  json  NDJSON — one JSON object per line (no enclosing array). Empty
        result set emits nothing.

Defaults: a freshly-added rule blocks both deletion and force-push.
Pass --allow-deletion / --allow-force-push to loosen specific protections.

Patterns use stdlib path.Match globs: '*' matches one segment (does not
cross '/'); '?' matches one character; '[abc]' character classes.
Recursive '**' is NOT supported — add multiple rules for nested namespaces.

Exit codes:
  0  ok
  1  operational error (db unreachable, ...)
  2  usage error (bad flags, malformed pattern, ...)
`

func runPolicy(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, policyUsage)
		return 2
	}
	if args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(stdout, policyUsage)
		return 0
	}
	switch args[0] {
	case "refs":
		return runPolicyRefs(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "policy: unknown object %q\n%s", args[0], policyUsage)
		return 2
	}
}

func runPolicyRefs(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "policy refs: action required (add|list|remove)")
		return 2
	}
	switch args[0] {
	case "add":
		return runPolicyRefsAdd(ctx, args[1:], stdout, stderr)
	case "list":
		return runPolicyRefsList(ctx, args[1:], stdout, stderr)
	case "remove":
		return runPolicyRefsRemove(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "policy refs: unknown action %q (want add|list|remove)\n", args[0])
		return 2
	}
}

func runPolicyRefsAdd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("policy refs add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	tenant := fs.String("tenant", "", "Tenant ID (required)")
	repo := fs.String("repo", "", "Repo ID (required)")
	pattern := fs.String("pattern", "", "Refname glob (required, e.g. refs/heads/main)")
	allowDel := fs.Bool("allow-deletion", false, "Allow deletion (default: block)")
	allowFP := fs.Bool("allow-force-push", false, "Allow force-push (default: block)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" || *tenant == "" || *repo == "" || *pattern == "" {
		fmt.Fprintln(stderr, "policy refs add: --auth-db, --tenant, --repo, --pattern required")
		return 2
	}
	svc, store, err := openPolicySvc(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "policy refs add: %v\n", err)
		return 1
	}
	defer store.Close()
	err = svc.Add(ctx, policy.ProtectedRef{
		Tenant:         *tenant,
		Repo:           *repo,
		RefnamePattern: *pattern,
		BlockDeletion:  !*allowDel,
		BlockForcePush: !*allowFP,
	})
	if err != nil {
		// Distinguish usage (bad pattern) from operational (db) errors.
		if isPolicyUsageError(err) {
			fmt.Fprintf(stderr, "policy refs add: %v\n", err)
			return 2
		}
		fmt.Fprintf(stderr, "policy refs add: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "tenant=%s  repo=%s  pattern=%s  block_deletion=%t  block_force_push=%t\n",
		*tenant, *repo, *pattern, !*allowDel, !*allowFP)
	return 0
}

func runPolicyRefsList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("policy refs list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	tenant := fs.String("tenant", "", "Tenant ID (required)")
	repo := fs.String("repo", "", "Repo ID (required)")
	format := fs.String("format", "text", "Output format: text|json")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" || *tenant == "" || *repo == "" {
		fmt.Fprintln(stderr, "policy refs list: --auth-db, --tenant, --repo required")
		return 2
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "policy refs list: --format must be text|json (got %q)\n", *format)
		return 2
	}
	svc, store, err := openPolicySvc(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "policy refs list: %v\n", err)
		return 1
	}
	defer store.Close()
	rules, err := svc.List(ctx, *tenant, *repo)
	if err != nil {
		fmt.Fprintf(stderr, "policy refs list: %v\n", err)
		return 1
	}
	if len(rules) == 0 {
		// JSON format is NDJSON (one record per line, no enclosing
		// array — matches the M13.5 quota CLI convention). Empty
		// list emits nothing; callers parse line-by-line and an
		// empty input is correctly an empty result.
		if *format == "json" {
			return 0
		}
		fmt.Fprintf(stdout, "tenant=%s  repo=%s  (no protected refs)\n", *tenant, *repo)
		return 0
	}
	for _, r := range rules {
		if *format == "json" {
			_ = json.NewEncoder(stdout).Encode(map[string]any{
				"tenant":           r.Tenant,
				"repo":             r.Repo,
				"pattern":          r.RefnamePattern,
				"block_deletion":   r.BlockDeletion,
				"block_force_push": r.BlockForcePush,
				"created_at":       r.CreatedAt.Format(time.RFC3339),
			})
			continue
		}
		fmt.Fprintf(stdout,
			"tenant=%s  repo=%s  pattern=%s  block_deletion=%t  block_force_push=%t  created=%s\n",
			r.Tenant, r.Repo, r.RefnamePattern, r.BlockDeletion, r.BlockForcePush,
			r.CreatedAt.Format(time.RFC3339))
	}
	return 0
}

func runPolicyRefsRemove(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("policy refs remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	tenant := fs.String("tenant", "", "Tenant ID (required)")
	repo := fs.String("repo", "", "Repo ID (required)")
	pattern := fs.String("pattern", "", "Refname glob to remove (required, exact match)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" || *tenant == "" || *repo == "" || *pattern == "" {
		fmt.Fprintln(stderr, "policy refs remove: --auth-db, --tenant, --repo, --pattern required")
		return 2
	}
	svc, store, err := openPolicySvc(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "policy refs remove: %v\n", err)
		return 1
	}
	defer store.Close()
	if err := svc.Remove(ctx, *tenant, *repo, *pattern); err != nil {
		fmt.Fprintf(stderr, "policy refs remove: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "tenant=%s  repo=%s  pattern=%s  removed\n", *tenant, *repo, *pattern)
	return 0
}

func openPolicySvc(path string) (*policy.Service, *sqlitestore.Store, error) {
	store, err := sqlitestore.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open authdb: %w", err)
	}
	return policy.New(store.DB()), store, nil
}

// isPolicyUsageError reports whether the error from policy.Service.Add
// is due to a malformed user-supplied value (empty pattern, bad glob)
// vs an operational failure (sqlite). The Service surfaces both via
// fmt.Errorf with distinct prefixes; key off "must not be empty" or
// "invalid refname_pattern" substrings as a stable signal.
func isPolicyUsageError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "must not be empty") ||
		strings.Contains(msg, "invalid refname_pattern")
}
