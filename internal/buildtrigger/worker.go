package buildtrigger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

// WorkerConfig parameterizes the delivery worker loop. Production defaults are
// set in DefaultWorkerConfig; tests override TickInterval/BackoffSchedule for
// speed and inject Deliverers directly.
type WorkerConfig struct {
	TickInterval      time.Duration
	ClaimBatchSize    int
	Concurrency       int
	HTTPTimeout       time.Duration
	BackoffSchedule   []time.Duration // delay before attempt N+1; len = max attempts - 1
	BackoffJitterFrac float64         // 0.25 → ±25% jitter per interval
	ReclaimThreshold  time.Duration
	// HTTPClient is reserved to mirror the webhook WorkerConfig shape. The
	// build-trigger deliverers own their own *http.Client (built from Egress in
	// ProductionDeliverers), so the worker loop does not consult this field.
	HTTPClient *http.Client
	// Egress is the delivery egress policy used to build the production HTTP
	// deliverers when Deliverers is nil. Ignored when Deliverers is supplied.
	Egress *webhooks.EgressPolicy
	Logger *slog.Logger // optional; defaults to slog.Default()

	// Deliverers maps trigger kind → Deliverer. When nil, StartWorker builds the
	// production set via ProductionDeliverers using an egress HTTP client.
	Deliverers map[Kind]Deliverer
	// MintFn mints short-lived build tokens at delivery time. Used only when
	// building the production deliverer set (Deliverers == nil).
	MintFn MintFunc
	// Connectors is the named AWS connector map used to build the production
	// CodeBuild deliverer (Deliverers == nil).
	Connectors map[string]AWSConnector
	// AzureConnectors is the named Azure DevOps connector map used to build the
	// production Azure Pipelines deliverer (Deliverers == nil).
	AzureConnectors map[string]AzureConnector
}

// DefaultWorkerConfig returns the production defaults (mirrors webhooks §6).
func DefaultWorkerConfig() WorkerConfig {
	return WorkerConfig{
		TickInterval:      1 * time.Second,
		ClaimBatchSize:    32,
		Concurrency:       8,
		HTTPTimeout:       10 * time.Second,
		BackoffSchedule:   []time.Duration{1 * time.Minute, 30 * time.Minute, 2 * time.Hour, 12 * time.Hour},
		BackoffJitterFrac: 0.25,
		ReclaimThreshold:  60 * time.Second,
	}
}

// StartWorker runs the delivery worker loop until ctx is cancelled. Blocks the
// caller; callers typically invoke this in its own goroutine from bucketvcs
// serve boot.
//
// A nil *Service is a no-op (matches the optional-deps pattern elsewhere).
func StartWorker(ctx context.Context, svc *Service, cfg WorkerConfig) {
	if svc == nil {
		return
	}
	def := DefaultWorkerConfig()
	if cfg.TickInterval <= 0 {
		cfg.TickInterval = def.TickInterval
	}
	if cfg.ClaimBatchSize <= 0 {
		cfg.ClaimBatchSize = def.ClaimBatchSize
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = def.Concurrency
	}
	if cfg.HTTPTimeout <= 0 {
		cfg.HTTPTimeout = def.HTTPTimeout
	}
	if len(cfg.BackoffSchedule) == 0 {
		cfg.BackoffSchedule = def.BackoffSchedule
	}
	if cfg.BackoffJitterFrac == 0 {
		cfg.BackoffJitterFrac = def.BackoffJitterFrac
	}
	if cfg.ReclaimThreshold <= 0 {
		cfg.ReclaimThreshold = def.ReclaimThreshold
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	// Build the production deliverer set when the caller didn't inject one
	// (tests inject directly; production builds an egress-gated HTTP client).
	if cfg.Deliverers == nil {
		cfg.Deliverers = ProductionDeliverers(cfg.MintFn, cfg.Connectors, cfg.AzureConnectors, cfg.Egress, cfg.HTTPTimeout)
	}

	if err := Reclaim(ctx, svc.db, cfg.ReclaimThreshold); err != nil {
		logger.LogAttrs(ctx, slog.LevelError, "buildtrigger.reclaim_failed",
			slog.String("error", err.Error()))
	}
	if n, err := DeadLetterOrphans(ctx, svc.db); err != nil {
		logger.LogAttrs(ctx, slog.LevelError, "buildtrigger.dead_letter_orphans_failed",
			slog.String("error", err.Error()))
	} else if n > 0 {
		logger.LogAttrs(ctx, slog.LevelInfo, "buildtrigger.orphans_dead_lettered",
			slog.Int64("count", n))
	}

	sem := make(chan struct{}, cfg.Concurrency)
	var wg sync.WaitGroup
	tick := time.NewTicker(cfg.TickInterval)
	defer tick.Stop()
	// Periodic Reclaim: at TickInterval=1s default this runs once per minute,
	// catching rows whose recordResult UPDATE failed mid-run (context
	// cancelled, sqlite-busy with logger LogAttrs already firing) so they
	// don't stay in_flight forever waiting for the next worker boot.
	const reclaimEveryNTicks = 60
	tickCount := 0
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-tick.C:
			tickCount++
			if tickCount%reclaimEveryNTicks == 0 {
				if err := Reclaim(ctx, svc.db, cfg.ReclaimThreshold); err != nil {
					logger.LogAttrs(ctx, slog.LevelError, "buildtrigger.reclaim_failed",
						slog.String("error", err.Error()),
						slog.String("phase", "periodic"))
				}
				if n, err := DeadLetterOrphans(ctx, svc.db); err != nil {
					logger.LogAttrs(ctx, slog.LevelError, "buildtrigger.dead_letter_orphans_failed",
						slog.String("error", err.Error()),
						slog.String("phase", "periodic"))
				} else if n > 0 {
					logger.LogAttrs(ctx, slog.LevelInfo, "buildtrigger.orphans_dead_lettered",
						slog.Int64("count", n))
				}
			}
			claimed, err := claim(ctx, svc.db, cfg.ClaimBatchSize)
			if err != nil {
				continue
			}
			for _, row := range claimed {
				row := row
				sem <- struct{}{}
				wg.Add(1)
				go func() {
					defer func() { <-sem; wg.Done() }()
					deliver(ctx, svc, cfg, row, logger)
				}()
			}
		}
	}
}

// claimedRow is the per-row view the worker uses to deliver. Wraps the delivery
// row fields plus the joined trigger columns needed to reconstruct a Trigger.
type claimedRow struct {
	ID          string
	TriggerID   string
	PayloadJSON []byte
	Attempts    int

	// Joined from build_triggers t ON t.id = d.trigger_id.
	Kind            string
	ConfigJSON      []byte
	TokenMode       string
	TokenScopes     int64
	TokenTTLSeconds int64
	Tenant          string
	Repo            string
	Name            string
}

// claim transitions up to batch deliveries pending → in_flight, returning them
// with their joined trigger columns. Postgres uses FOR UPDATE SKIP LOCKED so
// multiple gateway nodes never claim the same row; sqlite/libsql use the
// serialized SELECT-then-UPDATE (single-writer).
func claim(ctx context.Context, db sqlitestore.Querier, batch int) ([]claimedRow, error) {
	if db.SupportsSkipLocked() {
		return claimSkipLocked(ctx, db, batch)
	}
	return claimSerialized(ctx, db, batch)
}

// claimSerialized atomically transitions up to batch rows from pending →
// in_flight, stamping last_attempt_at and incrementing attempts. Joins the
// trigger columns in one query.
//
// SQLite already serializes writers, and bucketvcs's documented design is
// single-writer per gateway. The transaction boundary ensures the SELECT +
// UPDATE batch is atomic; concurrent worker processes — if any — serialize
// through SQLite's write lock.
//
// The JOIN filters on t.active=1, so disabling a trigger stops draining its
// pending queue immediately (rows stay pending and resume only if the trigger
// is re-enabled).
func claimSerialized(ctx context.Context, db sqlitestore.Querier, batch int) ([]claimedRow, error) {
	var out []claimedRow
	err := db.RunInTx(ctx, func(tx sqlitestore.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT d.id, d.trigger_id, d.payload_json, d.attempts,
			        t.kind, t.config_json, t.token_mode, t.token_scopes,
			        t.token_ttl_seconds, t.tenant, t.repo, t.name
			 FROM build_trigger_deliveries d
			 JOIN build_triggers t ON t.id = d.trigger_id
			 WHERE d.status='pending' AND d.next_attempt_at <= ?
			   AND t.active=1
			 ORDER BY d.next_attempt_at
			 LIMIT ?`,
			time.Now().Unix(), batch)
		if err != nil {
			return err
		}
		var claimed []claimedRow
		for rows.Next() {
			var r claimedRow
			if err := rows.Scan(&r.ID, &r.TriggerID, &r.PayloadJSON, &r.Attempts,
				&r.Kind, &r.ConfigJSON, &r.TokenMode, &r.TokenScopes,
				&r.TokenTTLSeconds, &r.Tenant, &r.Repo, &r.Name); err != nil {
				rows.Close()
				return err
			}
			claimed = append(claimed, r)
		}
		rows.Close()
		now := time.Now().Unix()
		for i := range claimed {
			claimed[i].Attempts++
			if _, err := tx.ExecContext(ctx,
				`UPDATE build_trigger_deliveries
				   SET status='in_flight', last_attempt_at=?, attempts=?
				 WHERE id=?`,
				now, claimed[i].Attempts, claimed[i].ID); err != nil {
				return err
			}
		}
		out = claimed
		return nil
	})
	return out, err
}

// claimSkipLocked claims rows in a single atomic UPDATE … RETURNING using
// FOR UPDATE SKIP LOCKED in the row-selection subquery. Safe for concurrent
// claimers across nodes. Postgres-only syntax (gated by SupportsSkipLocked).
func claimSkipLocked(ctx context.Context, db sqlitestore.Querier, batch int) ([]claimedRow, error) {
	now := time.Now().Unix()
	rows, err := db.QueryContext(ctx, `
		UPDATE build_trigger_deliveries d
		   SET status='in_flight', last_attempt_at=?, attempts=d.attempts+1
		  FROM build_triggers t
		 WHERE t.id = d.trigger_id
		   AND d.id IN (
		       SELECT d2.id
		         FROM build_trigger_deliveries d2
		         JOIN build_triggers t2 ON t2.id = d2.trigger_id
		        WHERE d2.status='pending' AND d2.next_attempt_at <= ? AND t2.active=1
		        ORDER BY d2.next_attempt_at
		        LIMIT ?
		        FOR UPDATE SKIP LOCKED
		   )
		RETURNING d.id, d.trigger_id, d.payload_json, d.attempts,
		          t.kind, t.config_json, t.token_mode, t.token_scopes,
		          t.token_ttl_seconds, t.tenant, t.repo, t.name`,
		now, now, batch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []claimedRow
	for rows.Next() {
		var r claimedRow
		if err := rows.Scan(&r.ID, &r.TriggerID, &r.PayloadJSON, &r.Attempts,
			&r.Kind, &r.ConfigJSON, &r.TokenMode, &r.TokenScopes,
			&r.TokenTTLSeconds, &r.Tenant, &r.Repo, &r.Name); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// deliver performs one attempt for row and updates the DB based on outcome.
// Decode failures (bad config_json/payload_json) fail the delivery via
// recordResult (retry/dead-letter) rather than panicking.
func deliver(ctx context.Context, svc *Service, cfg WorkerConfig, row claimedRow, logger *slog.Logger) {
	start := time.Now()

	// Reconstruct the Trigger from the joined columns.
	tr := Trigger{
		ID:          row.TriggerID,
		Tenant:      row.Tenant,
		Repo:        row.Repo,
		Name:        row.Name,
		Kind:        Kind(row.Kind),
		TokenMode:   TokenMode(row.TokenMode),
		TokenScopes: auth.TokenScope(row.TokenScopes),
		TokenTTL:    time.Duration(row.TokenTTLSeconds) * time.Second,
	}
	if len(row.ConfigJSON) > 0 {
		if err := json.Unmarshal(row.ConfigJSON, &tr.Config); err != nil {
			recordResult(ctx, svc, cfg, row, 0, fmt.Errorf("decode config: %w", err), logger, time.Since(start).Milliseconds())
			return
		}
	}

	var payload BuildPayload
	if err := json.Unmarshal(row.PayloadJSON, &payload); err != nil {
		recordResult(ctx, svc, cfg, row, 0, fmt.Errorf("decode payload: %w", err), logger, time.Since(start).Milliseconds())
		return
	}

	EmitFiredAudit(ctx, logger, row.ID, row.TriggerID, row.Kind, 1)

	d := cfg.Deliverers[tr.Kind]
	if d == nil {
		recordResult(ctx, svc, cfg, row, 0, fmt.Errorf("no deliverer for kind %q", tr.Kind), logger, time.Since(start).Milliseconds())
		return
	}

	reqCtx, cancel := context.WithTimeout(ctx, cfg.HTTPTimeout)
	defer cancel()
	code, err := d.Deliver(reqCtx, tr, payload)
	recordResult(ctx, svc, cfg, row, code, err, logger, time.Since(start).Milliseconds())
}

// recordResult writes the post-attempt state. err=nil → delivered. Otherwise,
// if attempts >= len(BackoffSchedule)+1, dead_letter; else back to pending with
// next_attempt_at = NOW + backoff. Byte-aligned with the webhook recordResult.
func recordResult(ctx context.Context, svc *Service, cfg WorkerConfig, row claimedRow, statusCode int, err error, logger *slog.Logger, durationMs int64) {
	now := time.Now().Unix()
	if err == nil {
		// next_attempt_at is set to NOW (not NULL) because the schema declares
		// next_attempt_at INTEGER NOT NULL; setting it to NOW on terminal rows
		// keeps `delivery list` output clean (no stale "still due" timestamps).
		if _, uerr := svc.db.ExecContext(ctx,
			`UPDATE build_trigger_deliveries
			   SET status='delivered', delivered_at=?, last_status_code=?, last_error=NULL,
			       next_attempt_at=?
			 WHERE id=?`,
			now, statusCode, now, row.ID,
		); uerr != nil {
			logger.LogAttrs(ctx, slog.LevelError, "buildtrigger.update_failed",
				slog.String("delivery_id", row.ID),
				slog.String("terminal_status", "delivered"),
				slog.String("error", uerr.Error()))
			return
		}
		EmitFired(ctx, logger, row.Kind, "delivered")
		EmitAttemptDuration(ctx, logger, "delivered", durationMs)
		EmitDelivered(ctx, logger, row.ID, row.TriggerID, row.Attempts, durationMs)
		return
	}
	maxAttempts := len(cfg.BackoffSchedule) + 1
	permanent := errors.Is(err, ErrPermanent)
	if permanent || row.Attempts >= maxAttempts {
		reason := "exhausted"
		if permanent {
			reason = "permanent"
		}
		// next_attempt_at reset to NOW (schema NOT NULL constraint) so
		// `delivery list` doesn't show stale "still due" timestamps on
		// terminal dead_letter rows.
		if _, uerr := svc.db.ExecContext(ctx,
			`UPDATE build_trigger_deliveries
			   SET status='dead_letter', last_status_code=?, last_error=?,
			       next_attempt_at=?
			 WHERE id=?`,
			statusCode, truncErr(err.Error()), now, row.ID,
		); uerr != nil {
			logger.LogAttrs(ctx, slog.LevelError, "buildtrigger.update_failed",
				slog.String("delivery_id", row.ID),
				slog.String("terminal_status", "dead_letter"),
				slog.String("error", uerr.Error()))
			return
		}
		EmitFired(ctx, logger, row.Kind, "dead_letter")
		EmitAttemptDuration(ctx, logger, "dead_letter", durationMs)
		EmitDeadLetterMetric(ctx, logger, reason)
		EmitDeadLetter(ctx, logger, row.ID, row.TriggerID, row.Attempts, statusCode, reason)
		return
	}
	idx := row.Attempts - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cfg.BackoffSchedule) {
		idx = len(cfg.BackoffSchedule) - 1
	}
	delay := jitter(cfg.BackoffSchedule[idx], cfg.BackoffJitterFrac)
	nextAt := time.Now().Add(delay).Unix()
	if _, uerr := svc.db.ExecContext(ctx,
		`UPDATE build_trigger_deliveries
		   SET status='pending', next_attempt_at=?, last_status_code=?, last_error=?
		 WHERE id=?`,
		nextAt, statusCode, truncErr(err.Error()), row.ID,
	); uerr != nil {
		logger.LogAttrs(ctx, slog.LevelError, "buildtrigger.update_failed",
			slog.String("delivery_id", row.ID),
			slog.String("terminal_status", "retry"),
			slog.String("error", uerr.Error()))
		return
	}
	EmitFired(ctx, logger, row.Kind, "failed_retry")
	EmitAttemptDuration(ctx, logger, "failed_retry", durationMs)
	EmitFailed(ctx, logger, row.ID, row.TriggerID, row.Attempts, statusCode, err.Error(), nextAt)
}

func jitter(d time.Duration, frac float64) time.Duration {
	if frac <= 0 {
		return d
	}
	delta := float64(d) * frac
	jit := (rand.Float64()*2 - 1) * delta
	return d + time.Duration(jit)
}

// truncErr caps an error string at 512 bytes. May split a UTF-8 multi-byte
// rune mid-sequence; acceptable for an error message that's already being
// truncated (downstream display tools handle replacement bytes).
func truncErr(s string) string {
	const cap = 512
	if len(s) <= cap {
		return s
	}
	return s[:cap]
}

// NewMintFunc returns a MintFunc that mints a short-lived build token for a
// trigger at delivery time, emits the minted metric + audit (never the token
// value), and returns the wire-format token string.
func NewMintFunc(store *sqlitestore.Store, logger *slog.Logger) MintFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(ctx context.Context, tr Trigger, p BuildPayload) (string, error) {
		label := "build:" + tr.Tenant + "/" + tr.Repo + ":" + tr.Name
		ttlSeconds := int64(tr.TokenTTL.Seconds())
		token, err := store.MintBuildToken(ctx, sqlitestore.MintBuildParams{
			Tenant:     tr.Tenant,
			Repo:       tr.Repo,
			Scopes:     tr.TokenScopes,
			TTLSeconds: ttlSeconds,
			Label:      label,
		})
		if err != nil {
			return "", err
		}
		EmitTokenMinted(ctx, logger)
		EmitTokenMintedAudit(ctx, logger, tr.Tenant, tr.Repo, label, ttlSeconds)
		return token, nil
	}
}

// ProductionDeliverers builds the per-kind deliverer set used in production:
// generic + cloudbuild + azurewebhook share a signed-JSON HTTP deliverer over
// an egress-gated client (signature scheme selected per-kind); codebuild uses
// the SigV4 StartBuild deliverer; azurepipelines uses the PAT Run Pipeline
// REST deliverer.
func ProductionDeliverers(mint MintFunc, connectors map[string]AWSConnector, azureConnectors map[string]AzureConnector, egress *webhooks.EgressPolicy, timeout time.Duration) map[Kind]Deliverer {
	if egress == nil {
		egress = &webhooks.EgressPolicy{} // secure default: deny private/loopback/link-local
	}
	client := webhooks.NewHTTPClient(egress, timeout)
	httpD := &httpDeliverer{
		client: client,
		mintFn: mint,
	}
	cbD := &codeBuildDeliverer{
		clientFor: newCodeBuildClientFactory(connectors),
		mintFn:    mint,
	}
	azD := &azurePipelinesDeliverer{
		clientFor: newAzurePipelinesClientFactory(azureConnectors, client),
		mintFn:    mint,
	}
	return map[Kind]Deliverer{
		KindGeneric:        httpD,
		KindCloudBuild:     httpD,
		KindAzureWebhook:   httpD,
		KindCodeBuild:      cbD,
		KindAzurePipelines: azD,
	}
}
