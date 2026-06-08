package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/buildtrigger"
)

const buildUsage = `Usage: bucketvcs build <object> <action> [flags]

Objects + actions:
  trigger add     --auth-db=<path> --tenant=<t> --repo=<r> --name=<n> --kind=<generic|cloudbuild|codebuild>
                  generic/cloudbuild: --url=<https://...> [--secret=<s>]
                  codebuild:          --aws-region=<r> --aws-project=<p> [--aws-connector=<c>]
                  [--ref-include=<csv>] [--ref-exclude=<csv>]
                  [--token-mode=<none|inject>] [--token-scopes=<csv|all|repo:*|lfs:*>]
                  [--token-ttl=<dur>]
  trigger list    --auth-db=<path> --tenant=<t> --repo=<r> [--format=text|json]
  trigger remove  --auth-db=<path> --id=<id>
  trigger enable  --auth-db=<path> --id=<id>
  trigger disable --auth-db=<path> --id=<id>

  delivery list   --auth-db=<path> [--trigger=<id>] [--status=<s>] [--limit=<n>] [--format=text|json]
  delivery show   --auth-db=<path> --id=<id>
  delivery replay --auth-db=<path> --id=<id>

  apply           --auth-db=<path> -f <file> [--prune]
  test            --auth-db=<path> --id=<id>

Output formats:
  text  one record per line, key=value style.
  json  NDJSON — one JSON object per line (no enclosing array). Empty
        result set emits nothing.

Exit codes:
  0  ok
  1  operational error (db unreachable, not-found on mutate, ...)
  2  usage error (bad flags, invalid input, conflict, ...)
`

func runBuild(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, buildUsage)
		return 2
	}
	if args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(stdout, buildUsage)
		return 0
	}
	switch args[0] {
	case "trigger":
		return runBuildTrigger(ctx, args[1:], stdout, stderr)
	case "delivery":
		return runBuildDelivery(ctx, args[1:], stdout, stderr)
	case "apply":
		return runBuildApply(ctx, args[1:], stdout, stderr)
	case "test":
		return runBuildTest(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "build: unknown object %q\n%s", args[0], buildUsage)
		return 2
	}
}

func runBuildTrigger(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "build trigger: action required (add|list|remove|enable|disable)")
		return 2
	}
	switch args[0] {
	case "add":
		return runBuildTriggerAdd(ctx, args[1:], stdout, stderr)
	case "list":
		return runBuildTriggerList(ctx, args[1:], stdout, stderr)
	case "remove":
		return runBuildTriggerRemove(ctx, args[1:], stdout, stderr)
	case "enable":
		return runBuildTriggerEnable(ctx, args[1:], stdout, stderr)
	case "disable":
		return runBuildTriggerDisable(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "build trigger: unknown action %q\n", args[0])
		return 2
	}
}

func runBuildTriggerAdd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("build trigger add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	tenant := fs.String("tenant", "", "Tenant ID (required)")
	repo := fs.String("repo", "", "Repo ID (required)")
	name := fs.String("name", "", "Trigger name (required)")
	kind := fs.String("kind", "", "Trigger kind: generic|cloudbuild|codebuild (required)")
	urlFlag := fs.String("url", "", "Receiver URL (generic/cloudbuild)")
	secret := fs.String("secret", "", "Shared secret (generic/cloudbuild; generated if omitted)")
	awsRegion := fs.String("aws-region", "", "AWS region (codebuild)")
	awsProject := fs.String("aws-project", "", "CodeBuild project name (codebuild)")
	awsConnector := fs.String("aws-connector", "", "AWS connector name (codebuild, optional)")
	refInclude := fs.String("ref-include", "", "Ref include globs (csv, optional)")
	refExclude := fs.String("ref-exclude", "", "Ref exclude globs (csv, optional)")
	tokenMode := fs.String("token-mode", "", "Token mode: none|inject (optional)")
	tokenScopes := fs.String("token-scopes", "", "Token scopes (csv|all|repo:*|lfs:*, optional)")
	tokenTTL := fs.String("token-ttl", "", "Token TTL (Go duration, optional)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" || *tenant == "" || *repo == "" || *name == "" || *kind == "" {
		fmt.Fprintln(stderr, "build trigger add: --auth-db, --tenant, --repo, --name, --kind required")
		return 2
	}

	in := buildtrigger.TriggerInput{
		Tenant: *tenant,
		Repo:   *repo,
		Name:   *name,
		Kind:   buildtrigger.Kind(*kind),
		Config: buildtrigger.Config{
			URL:          *urlFlag,
			Secret:       *secret,
			AWSRegion:    *awsRegion,
			AWSProject:   *awsProject,
			AWSConnector: *awsConnector,
		},
		RefInclude: splitCSV(*refInclude),
		RefExclude: splitCSV(*refExclude),
		TokenMode:  buildtrigger.TokenMode(*tokenMode),
	}
	if *tokenScopes != "" {
		scopes, err := auth.ParseScopes(*tokenScopes)
		if err != nil {
			fmt.Fprintf(stderr, "build trigger add: --token-scopes: %v\n", err)
			return 2
		}
		in.TokenScopes = scopes
	}
	if *tokenTTL != "" {
		d, err := time.ParseDuration(*tokenTTL)
		if err != nil {
			fmt.Fprintf(stderr, "build trigger add: --token-ttl %q: %v\n", *tokenTTL, err)
			return 2
		}
		in.TokenTTL = d
	}

	svc, store, err := openBuildSvc(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "build trigger add: %v\n", err)
		return 1
	}
	defer store.Close()

	tr, err := svc.Create(ctx, in)
	if err != nil {
		if errors.Is(err, buildtrigger.ErrInvalidInput) || errors.Is(err, buildtrigger.ErrConflict) {
			fmt.Fprintf(stderr, "build trigger add: %v\n", err)
			return 2
		}
		fmt.Fprintf(stderr, "build trigger add: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "trigger_id=%s  tenant=%s  repo=%s  name=%s  kind=%s\n",
		tr.ID, tr.Tenant, tr.Repo, tr.Name, tr.Kind)
	if tr.Secret != "" {
		fmt.Fprintf(stdout, "secret=%s   # store this now — it will not be shown again\n", tr.Secret)
	}
	buildtrigger.EmitTriggerLifecycle(ctx, slog.Default(), "build.trigger.added", tr.ID, tr.Tenant, tr.Repo)
	return 0
}

func runBuildTriggerList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("build trigger list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	tenant := fs.String("tenant", "", "Tenant ID (required)")
	repo := fs.String("repo", "", "Repo ID (required)")
	format := fs.String("format", "text", "Output format: text|json")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" || *tenant == "" || *repo == "" {
		fmt.Fprintln(stderr, "build trigger list: --auth-db, --tenant, --repo required")
		return 2
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "build trigger list: --format must be text|json (got %q)\n", *format)
		return 2
	}
	svc, store, err := openBuildSvc(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "build trigger list: %v\n", err)
		return 1
	}
	defer store.Close()
	triggers, err := svc.List(ctx, *tenant, *repo)
	if err != nil {
		fmt.Fprintf(stderr, "build trigger list: %v\n", err)
		return 1
	}
	if len(triggers) == 0 {
		if *format == "json" {
			return 0
		}
		fmt.Fprintf(stdout, "tenant=%s  repo=%s  (no triggers)\n", *tenant, *repo)
		return 0
	}
	for _, tr := range triggers {
		if *format == "json" {
			_ = json.NewEncoder(stdout).Encode(map[string]any{
				"id":             tr.ID,
				"tenant":         tr.Tenant,
				"repo":           tr.Repo,
				"name":           tr.Name,
				"kind":           tr.Kind,
				"token_mode":     tr.TokenMode,
				"token_scopes":   auth.FormatScopes(tr.TokenScopes),
				"ref_include":    tr.RefInclude,
				"ref_exclude":    tr.RefExclude,
				"active":         tr.Active,
				"secret_preview": tr.SecretPreview,
				"created_at":     tr.CreatedAt.Format(time.RFC3339),
			})
			continue
		}
		fmt.Fprintf(stdout,
			"id=%s  name=%s  kind=%s  token_mode=%s  scopes=%s  active=%t  ref_include=%s\n",
			tr.ID, tr.Name, tr.Kind, tr.TokenMode, auth.FormatScopes(tr.TokenScopes),
			tr.Active, strings.Join(tr.RefInclude, ","))
	}
	return 0
}

func runBuildTriggerRemove(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runBuildTriggerMutate(ctx, args, stdout, stderr, "remove", "build.trigger.removed",
		func(svc *buildtrigger.Service, id string) error { return svc.Remove(ctx, id) })
}
func runBuildTriggerEnable(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runBuildTriggerMutate(ctx, args, stdout, stderr, "enable", "build.trigger.enabled",
		func(svc *buildtrigger.Service, id string) error { return svc.Enable(ctx, id) })
}
func runBuildTriggerDisable(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runBuildTriggerMutate(ctx, args, stdout, stderr, "disable", "build.trigger.disabled",
		func(svc *buildtrigger.Service, id string) error { return svc.Disable(ctx, id) })
}

func runBuildTriggerMutate(ctx context.Context, args []string, stdout, stderr io.Writer, name, event string, do func(*buildtrigger.Service, string) error) int {
	fs := flag.NewFlagSet("build trigger "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	id := fs.String("id", "", "Trigger ID (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" || *id == "" {
		fmt.Fprintf(stderr, "build trigger %s: --auth-db, --id required\n", name)
		return 2
	}
	svc, store, err := openBuildSvc(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "build trigger %s: %v\n", name, err)
		return 1
	}
	defer store.Close()
	// Fetch the trigger first so the lifecycle audit carries tenant/repo. A
	// missing trigger surfaces here as ErrNotFound before the mutation runs.
	var tenant, repo string
	if tr, gerr := svc.Get(ctx, *id); gerr == nil {
		tenant, repo = tr.Tenant, tr.Repo
	}
	if err := do(svc, *id); err != nil {
		fmt.Fprintf(stderr, "build trigger %s: %v\n", name, err)
		return 1
	}
	fmt.Fprintf(stdout, "trigger_id=%s  %s\n", *id, name+"d")
	buildtrigger.EmitTriggerLifecycle(ctx, slog.Default(), event, *id, tenant, repo)
	return 0
}

func runBuildDelivery(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "build delivery: action required (list|show|replay)")
		return 2
	}
	switch args[0] {
	case "list":
		return runBuildDeliveryList(ctx, args[1:], stdout, stderr)
	case "show":
		return runBuildDeliveryShow(ctx, args[1:], stdout, stderr)
	case "replay":
		return runBuildDeliveryReplay(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "build delivery: unknown action %q\n", args[0])
		return 2
	}
}

func runBuildDeliveryList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("build delivery list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	trigger := fs.String("trigger", "", "Filter by trigger id (optional)")
	status := fs.String("status", "", "Filter by status (optional)")
	limit := fs.Int("limit", 500, "Max rows to return (default 500; 0 = no limit)")
	format := fs.String("format", "text", "Output format: text|json")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" {
		fmt.Fprintln(stderr, "build delivery list: --auth-db required")
		return 2
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "build delivery list: --format must be text|json (got %q)\n", *format)
		return 2
	}
	if *status != "" {
		switch *status {
		case "pending", "in_flight", "delivered", "dead_letter":
		default:
			fmt.Fprintf(stderr, "build delivery list: --status must be one of pending|in_flight|delivered|dead_letter (got %q)\n", *status)
			return 2
		}
	}
	svc, store, err := openBuildSvc(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "build delivery list: %v\n", err)
		return 1
	}
	defer store.Close()
	rows, err := svc.ListDeliveries(ctx, *trigger, *status, *limit)
	if err != nil {
		fmt.Fprintf(stderr, "build delivery list: %v\n", err)
		return 1
	}
	if len(rows) == 0 && *format == "text" {
		fmt.Fprintln(stdout, "(no deliveries)")
		return 0
	}
	for _, r := range rows {
		if *format == "json" {
			_ = json.NewEncoder(stdout).Encode(map[string]any{
				"id":               r.ID,
				"trigger_id":       r.TriggerID,
				"status":           r.Status,
				"attempts":         r.Attempts,
				"next_attempt_at":  r.NextAttemptAt.Format(time.RFC3339),
				"last_status_code": r.LastStatusCode,
				"last_error":       r.LastError,
				"created_at":       r.CreatedAt.Format(time.RFC3339),
			})
			continue
		}
		fmt.Fprintf(stdout,
			"id=%s  trigger=%s  status=%s  attempts=%d  last_code=%d  created=%s\n",
			r.ID, r.TriggerID, r.Status, r.Attempts, r.LastStatusCode,
			r.CreatedAt.Format(time.RFC3339))
	}
	return 0
}

func runBuildDeliveryShow(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("build delivery show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	id := fs.String("id", "", "Delivery ID (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" || *id == "" {
		fmt.Fprintln(stderr, "build delivery show: --auth-db, --id required")
		return 2
	}
	svc, store, err := openBuildSvc(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "build delivery show: %v\n", err)
		return 1
	}
	defer store.Close()
	d, err := svc.GetDelivery(ctx, *id)
	if err != nil {
		fmt.Fprintf(stderr, "build delivery show: %v\n", err)
		return 1
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]any{
		"id":               d.ID,
		"trigger_id":       d.TriggerID,
		"status":           d.Status,
		"attempts":         d.Attempts,
		"next_attempt_at":  d.NextAttemptAt.Format(time.RFC3339),
		"last_status_code": d.LastStatusCode,
		"last_error":       d.LastError,
		"created_at":       d.CreatedAt.Format(time.RFC3339),
	})
	return 0
}

func runBuildDeliveryReplay(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("build delivery replay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	id := fs.String("id", "", "Delivery ID (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" || *id == "" {
		fmt.Fprintln(stderr, "build delivery replay: --auth-db, --id required")
		return 2
	}
	svc, store, err := openBuildSvc(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "build delivery replay: %v\n", err)
		return 1
	}
	defer store.Close()
	if err := svc.ReplayDelivery(ctx, *id); err != nil {
		// ErrReplayInFlight and ErrNotFound are operational (the row exists
		// but cannot be replayed, or the id is unknown) — exit 1.
		fmt.Fprintf(stderr, "build delivery replay: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "id=%s  replay-scheduled\n", *id)
	return 0
}

func runBuildApply(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("build apply", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	file := fs.String("f", "", "Path to declarative triggers document (required)")
	prune := fs.Bool("prune", false, "Remove triggers not present in the document")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" || *file == "" {
		fmt.Fprintln(stderr, "build apply: --auth-db and -f required")
		return 2
	}
	data, err := os.ReadFile(*file)
	if err != nil {
		fmt.Fprintf(stderr, "build apply: read %s: %v\n", *file, err)
		return 1
	}
	svc, store, err := openBuildSvc(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "build apply: %v\n", err)
		return 1
	}
	defer store.Close()
	res, err := buildtrigger.Apply(ctx, svc, data, *prune)
	if err != nil {
		if errors.Is(err, buildtrigger.ErrInvalidInput) || errors.Is(err, buildtrigger.ErrConflict) {
			fmt.Fprintf(stderr, "build apply: %v\n", err)
			return 2
		}
		fmt.Fprintf(stderr, "build apply: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "created=%d updated=%d pruned=%d\n", res.Created, res.Updated, res.Pruned)
	return 0
}

func runBuildTest(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("build test", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	id := fs.String("id", "", "Trigger ID (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" || *id == "" {
		fmt.Fprintln(stderr, "build test: --auth-db, --id required")
		return 2
	}
	svc, store, err := openBuildSvc(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "build test: %v\n", err)
		return 1
	}
	defer store.Close()
	tr, err := svc.Get(ctx, *id)
	if err != nil {
		fmt.Fprintf(stderr, "build test: %v\n", err)
		return 1
	}
	// Synthesize a push against the first literal (non-glob) ref_include, or
	// fall back to refs/heads/main when the trigger matches every ref.
	ref := "refs/heads/main"
	for _, pat := range tr.RefInclude {
		if !strings.ContainsAny(pat, "*?[") {
			ref = pat
			break
		}
	}
	if err := svc.Enqueue(ctx, buildtrigger.PushInfo{
		Tenant: tr.Tenant,
		Repo:   tr.Repo,
		Actor:  cliActor(),
		RefUpdates: []buildtrigger.RefUpdate{{
			Refname: ref,
			NewOID:  "0000000000000000000000000000000000000000",
		}},
	}); err != nil {
		fmt.Fprintf(stderr, "build test: enqueue: %v\n", err)
		return 1
	}
	// Report the resulting pending delivery id(s) for this trigger.
	rows, err := svc.ListDeliveries(ctx, tr.ID, "pending", 0)
	if err != nil {
		fmt.Fprintf(stderr, "build test: list deliveries: %v\n", err)
		return 1
	}
	if len(rows) == 0 {
		fmt.Fprintf(stdout, "trigger_id=%s  ref=%s  (no delivery enqueued — ref did not match)\n", tr.ID, ref)
		return 0
	}
	for _, r := range rows {
		fmt.Fprintf(stdout, "delivery_id=%s  trigger_id=%s  ref=%s  status=%s\n", r.ID, tr.ID, ref, r.Status)
	}
	return 0
}

func openBuildSvc(path string) (*buildtrigger.Service, *sqlitestore.Store, error) {
	store, err := sqlitestore.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open authdb: %w", err)
	}
	return buildtrigger.New(store.DB()), store, nil
}
