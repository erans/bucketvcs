package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

const webhookPruneUsage = `Usage: bucketvcs webhook prune [flags]

Sweep terminal-state webhook delivery rows past their retention window.
Never touches 'pending' or 'in_flight' rows.

Flags:
  --auth-db=<path>                       (required) path to auth.db
  --delivered-older-than=<duration>      retention for status='delivered'
                                         (default 720h = 30d; cutoff: delivered_at)
  --dead-letter-older-than=<duration>    retention for status='dead_letter'
                                         (default 2160h = 90d; cutoff: last_attempt_at)
  --dry-run                              report counts that WOULD be deleted;
                                         do not mutate
  --actor=<string>                       audit attribution
                                         (default: $USER, else "unknown")

Examples:
  bucketvcs webhook prune --auth-db=/var/lib/bucketvcs/auth.db
  bucketvcs webhook prune --auth-db=... --dry-run --delivered-older-than=168h
`

func runWebhookPrune(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("webhook prune", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	deliveredAge := fs.Duration("delivered-older-than", 30*24*time.Hour,
		"retention for status='delivered'")
	deadLetterAge := fs.Duration("dead-letter-older-than", 90*24*time.Hour,
		"retention for status='dead_letter'")
	dryRun := fs.Bool("dry-run", false, "report counts; do not mutate")

	actorPassed := false
	actor := ""
	fs.Func("actor", "audit attribution (default: $USER, else \"unknown\")",
		func(s string) error {
			actorPassed = true
			actor = s
			return nil
		})

	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" {
		fmt.Fprintln(stderr, "webhook prune: --auth-db is required")
		fmt.Fprint(stderr, webhookPruneUsage)
		return 2
	}
	if actorPassed && actor == "" {
		fmt.Fprintln(stderr, "webhook prune: --actor must be non-empty if specified")
		return 2
	}
	const minRetention = time.Hour
	if *deliveredAge < minRetention {
		fmt.Fprintf(stderr, "webhook prune: --delivered-older-than must be >= %s (got %s)\n",
			minRetention, *deliveredAge)
		return 2
	}
	if *deadLetterAge < minRetention {
		fmt.Fprintf(stderr, "webhook prune: --dead-letter-older-than must be >= %s (got %s)\n",
			minRetention, *deadLetterAge)
		return 2
	}

	resolvedActor := actor
	if resolvedActor == "" {
		resolvedActor = defaultActor()
	}

	svc, store, err := openWebhookSvc(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "webhook prune: %v\n", err)
		return 1
	}
	defer store.Close()

	now := time.Now()
	cfg := webhooks.PruneConfig{
		DeliveredCutoff:  now.Add(-*deliveredAge),
		DeadLetterCutoff: now.Add(-*deadLetterAge),
		DryRun:           *dryRun,
	}
	report, err := svc.Prune(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "webhook prune: %v\n", err)
		return 1
	}

	logger := slog.Default()
	webhooks.EmitWebhookPruned(ctx, logger,
		report.DeliveredDeleted, report.DeadLetterDeleted,
		cfg.DeliveredCutoff, cfg.DeadLetterCutoff, *dryRun, resolvedActor)
	webhooks.EmitWebhookPrunedMetric(ctx, logger, "delivered", report.DeliveredDeleted)
	webhooks.EmitWebhookPrunedMetric(ctx, logger, "dead_letter", report.DeadLetterDeleted)

	verb := "pruned"
	if *dryRun {
		verb = "DRY-RUN: would prune"
	}
	fmt.Fprintf(stdout, "%s: %d delivered (older than %s), %d dead-letter (older than %s)\n",
		verb, report.DeliveredDeleted, deliveredAge.String(),
		report.DeadLetterDeleted, deadLetterAge.String())
	return 0
}
