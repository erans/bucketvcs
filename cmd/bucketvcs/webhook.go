package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

const webhookUsage = `Usage: bucketvcs webhook <object> <action> [flags]

Objects + actions:
  endpoint add     --auth-db=<path> --tenant=<t> --repo=<r>
                   --url=<https://...> --events=<csv|all|lfs.*|repo.*>
  endpoint list    --auth-db=<path> --tenant=<t> --repo=<r> [--format=text|json]
  endpoint remove  --auth-db=<path> --id=<N>
  endpoint enable  --auth-db=<path> --id=<N>
  endpoint disable --auth-db=<path> --id=<N>

  delivery list    --auth-db=<path> [--endpoint-id=<N>]
                   [--status=pending|in_flight|delivered|dead_letter]
                   [--since=<RFC3339>] [--format=text|json]
  delivery show    --auth-db=<path> --id=<uuid>
  delivery replay  --auth-db=<path> --id=<uuid>

Output formats:
  text  one record per line, key=value style.
  json  NDJSON — one JSON object per line (no enclosing array). Empty
        result set emits nothing.

Events:
  Canonical names: push, lfs.upload, lfs.lock.created, lfs.lock.released,
                   repo.created, repo.deleted, repo.renamed, policy.ref.rejected
  Shortcuts: all, lfs.* (lfs.upload + lfs.lock.*), repo.* (repo.created/deleted/renamed)

Exit codes:
  0  ok
  1  operational error (db unreachable, ...)
  2  usage error (bad flags, malformed url, unknown event, ...)
`

func runWebhook(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, webhookUsage)
		return 2
	}
	if args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(stdout, webhookUsage)
		return 0
	}
	switch args[0] {
	case "endpoint":
		return runWebhookEndpoint(ctx, args[1:], stdout, stderr)
	case "delivery":
		return runWebhookDelivery(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "webhook: unknown object %q\n%s", args[0], webhookUsage)
		return 2
	}
}

func runWebhookEndpoint(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "webhook endpoint: action required (add|list|remove|enable|disable)")
		return 2
	}
	switch args[0] {
	case "add":
		return runWebhookEndpointAdd(ctx, args[1:], stdout, stderr)
	case "list":
		return runWebhookEndpointList(ctx, args[1:], stdout, stderr)
	case "remove":
		return runWebhookEndpointRemove(ctx, args[1:], stdout, stderr)
	case "enable":
		return runWebhookEndpointEnable(ctx, args[1:], stdout, stderr)
	case "disable":
		return runWebhookEndpointDisable(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "webhook endpoint: unknown action %q\n", args[0])
		return 2
	}
}

func runWebhookEndpointAdd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("webhook endpoint add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	tenant := fs.String("tenant", "", "Tenant ID (required)")
	repo := fs.String("repo", "", "Repo ID (required)")
	urlFlag := fs.String("url", "", "Receiver URL (required, http(s)://)")
	events := fs.String("events", "", "Event filter (required, csv|all|lfs.*|repo.*)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" || *tenant == "" || *repo == "" || *urlFlag == "" || *events == "" {
		fmt.Fprintln(stderr, "webhook endpoint add: --auth-db, --tenant, --repo, --url, --events required")
		return 2
	}
	mask, err := webhooks.ParseEvents(*events)
	if err != nil {
		fmt.Fprintf(stderr, "webhook endpoint add: %v\n", err)
		return 2
	}
	svc, store, err := openWebhookSvc(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "webhook endpoint add: %v\n", err)
		return 1
	}
	defer store.Close()
	ep, err := svc.Create(ctx, webhooks.EndpointInput{
		Tenant: *tenant, Repo: *repo, URL: *urlFlag, EventMask: mask,
	})
	if err != nil {
		if isWebhookUsageError(err) {
			fmt.Fprintf(stderr, "webhook endpoint add: %v\n", err)
			return 2
		}
		fmt.Fprintf(stderr, "webhook endpoint add: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout,
		"endpoint_id=%d  tenant=%s  repo=%s  url=%s  events=%s\n"+
			"secret=%s   # store this now — it will not be shown again\n",
		ep.ID, ep.Tenant, ep.Repo, ep.URL, webhooks.FormatEvents(ep.EventMask),
		ep.Secret)
	return 0
}

func runWebhookEndpointList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("webhook endpoint list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	tenant := fs.String("tenant", "", "Tenant ID (required)")
	repo := fs.String("repo", "", "Repo ID (required)")
	format := fs.String("format", "text", "Output format: text|json")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" || *tenant == "" || *repo == "" {
		fmt.Fprintln(stderr, "webhook endpoint list: --auth-db, --tenant, --repo required")
		return 2
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "webhook endpoint list: --format must be text|json (got %q)\n", *format)
		return 2
	}
	svc, store, err := openWebhookSvc(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "webhook endpoint list: %v\n", err)
		return 1
	}
	defer store.Close()
	eps, err := svc.List(ctx, *tenant, *repo)
	if err != nil {
		fmt.Fprintf(stderr, "webhook endpoint list: %v\n", err)
		return 1
	}
	if len(eps) == 0 {
		if *format == "json" {
			return 0
		}
		fmt.Fprintf(stdout, "tenant=%s  repo=%s  (no endpoints)\n", *tenant, *repo)
		return 0
	}
	for _, ep := range eps {
		if *format == "json" {
			_ = json.NewEncoder(stdout).Encode(map[string]any{
				"id":             ep.ID,
				"tenant":         ep.Tenant,
				"repo":           ep.Repo,
				"url":            ep.URL,
				"secret_preview": ep.SecretPreview,
				"events":         webhooks.FormatEvents(ep.EventMask),
				"active":         ep.Active,
				"created_at":     ep.CreatedAt.Format(time.RFC3339),
			})
			continue
		}
		fmt.Fprintf(stdout,
			"id=%d  tenant=%s  repo=%s  url=%s  events=%s  active=%t  secret_preview=%s  created=%s\n",
			ep.ID, ep.Tenant, ep.Repo, ep.URL, webhooks.FormatEvents(ep.EventMask),
			ep.Active, ep.SecretPreview, ep.CreatedAt.Format(time.RFC3339))
	}
	return 0
}

func runWebhookEndpointRemove(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runWebhookEndpointMutate(ctx, args, stdout, stderr, "remove",
		func(svc *webhooks.Service, id int64) error { return svc.Remove(ctx, id) })
}
func runWebhookEndpointEnable(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runWebhookEndpointMutate(ctx, args, stdout, stderr, "enable",
		func(svc *webhooks.Service, id int64) error { return svc.Enable(ctx, id) })
}
func runWebhookEndpointDisable(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runWebhookEndpointMutate(ctx, args, stdout, stderr, "disable",
		func(svc *webhooks.Service, id int64) error { return svc.Disable(ctx, id) })
}

func runWebhookEndpointMutate(ctx context.Context, args []string, stdout, stderr io.Writer, name string, do func(*webhooks.Service, int64) error) int {
	fs := flag.NewFlagSet("webhook endpoint "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	id := fs.Int64("id", 0, "Endpoint ID (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" || *id == 0 {
		fmt.Fprintf(stderr, "webhook endpoint %s: --auth-db, --id required\n", name)
		return 2
	}
	svc, store, err := openWebhookSvc(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "webhook endpoint %s: %v\n", name, err)
		return 1
	}
	defer store.Close()
	if err := do(svc, *id); err != nil {
		fmt.Fprintf(stderr, "webhook endpoint %s: %v\n", name, err)
		return 1
	}
	fmt.Fprintf(stdout, "endpoint_id=%d  %s\n", *id, name+"d")
	return 0
}

func runWebhookDelivery(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "webhook delivery: action required (list|show|replay)")
		return 2
	}
	switch args[0] {
	case "list":
		return runWebhookDeliveryList(ctx, args[1:], stdout, stderr)
	case "show":
		return runWebhookDeliveryShow(ctx, args[1:], stdout, stderr)
	case "replay":
		return runWebhookDeliveryReplay(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "webhook delivery: unknown action %q\n", args[0])
		return 2
	}
}

func runWebhookDeliveryList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("webhook delivery list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	endpointID := fs.Int64("endpoint-id", 0, "Filter by endpoint id (optional)")
	status := fs.String("status", "", "Filter by status (optional)")
	since := fs.String("since", "", "Filter by created_at >= since (RFC3339, optional)")
	limit := fs.Int("limit", 500, "Max rows to return (default 500, max 10000). Use --since to narrow further.")
	format := fs.String("format", "text", "Output format: text|json")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" {
		fmt.Fprintln(stderr, "webhook delivery list: --auth-db required")
		return 2
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "webhook delivery list: --format must be text|json (got %q)\n", *format)
		return 2
	}
	var sinceUnix int64
	if *since != "" {
		t, err := time.Parse(time.RFC3339, *since)
		if err != nil {
			fmt.Fprintf(stderr, "webhook delivery list: bad --since: %v\n", err)
			return 2
		}
		sinceUnix = t.Unix()
	}
	if *status != "" {
		switch *status {
		case "pending", "in_flight", "delivered", "dead_letter":
		default:
			fmt.Fprintf(stderr, "webhook delivery list: --status must be one of pending|in_flight|delivered|dead_letter (got %q)\n", *status)
			return 2
		}
	}
	svc, store, err := openWebhookSvc(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "webhook delivery list: %v\n", err)
		return 1
	}
	defer store.Close()
	rows, err := svc.ListDeliveries(ctx, webhooks.ListDeliveriesFilter{
		EndpointID: *endpointID,
		Status:     *status,
		SinceUnix:  sinceUnix,
		Limit:      *limit,
	})
	if err != nil {
		fmt.Fprintf(stderr, "webhook delivery list: %v\n", err)
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
				"endpoint_id":      r.EndpointID,
				"event_type":       r.EventType,
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
			"id=%s  endpoint=%d  event=%s  status=%s  attempts=%d  last_code=%d  created=%s\n",
			r.ID, r.EndpointID, r.EventType, r.Status, r.Attempts,
			r.LastStatusCode, r.CreatedAt.Format(time.RFC3339))
	}
	return 0
}

func runWebhookDeliveryShow(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	// Note: --format=json is deferred to a follow-up milestone. Operators
	// piping payloads to jq today can use `delivery list --format=json` and
	// pick out the row, or read the payload field from the text output.
	fs := flag.NewFlagSet("webhook delivery show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	id := fs.String("id", "", "Delivery ID (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" || *id == "" {
		fmt.Fprintln(stderr, "webhook delivery show: --auth-db, --id required")
		return 2
	}
	svc, store, err := openWebhookSvc(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "webhook delivery show: %v\n", err)
		return 1
	}
	defer store.Close()
	d, err := svc.ShowDelivery(ctx, *id)
	if err != nil {
		fmt.Fprintf(stderr, "webhook delivery show: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "id=%s\nendpoint_id=%d\nevent_type=%s\nstatus=%s\nattempts=%d\n",
		d.ID, d.EndpointID, d.EventType, d.Status, d.Attempts)
	fmt.Fprintf(stdout, "next_attempt_at=%s\n", d.NextAttemptAt.Format(time.RFC3339))
	if d.LastStatusCode != 0 {
		// strconv.Quote escapes newlines/tabs/control chars so receiver bodies
		// with multi-line errors don't break the key=value output contract.
		fmt.Fprintf(stdout, "last_status_code=%d\nlast_error=%s\n", d.LastStatusCode, strconv.Quote(d.LastError))
	}
	fmt.Fprintf(stdout, "payload:\n%s\n", string(d.PayloadJSON))
	return 0
}

func runWebhookDeliveryReplay(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("webhook delivery replay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	id := fs.String("id", "", "Delivery ID (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" || *id == "" {
		fmt.Fprintln(stderr, "webhook delivery replay: --auth-db, --id required")
		return 2
	}
	svc, store, err := openWebhookSvc(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "webhook delivery replay: %v\n", err)
		return 1
	}
	defer store.Close()
	if err := svc.ReplayDelivery(ctx, *id); err != nil {
		if errors.Is(err, webhooks.ErrNotFound) {
			// Usage error — bad --id, not an operational failure.
			fmt.Fprintf(stderr, "webhook delivery replay: %v\n", err)
			return 2
		}
		fmt.Fprintf(stderr, "webhook delivery replay: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "id=%s  replay-scheduled\n", *id)
	return 0
}

func openWebhookSvc(path string) (*webhooks.Service, *sqlitestore.Store, error) {
	store, err := sqlitestore.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open authdb: %w", err)
	}
	return webhooks.New(store.DB()), store, nil
}

func isWebhookUsageError(err error) bool {
	return errors.Is(err, webhooks.ErrInvalidInput)
}
