package webhooks

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

// WorkerConfig parameterizes the worker loop. Production defaults are set in
// DefaultWorkerConfig; tests override TickInterval/BackoffSchedule for speed.
type WorkerConfig struct {
	TickInterval      time.Duration
	ClaimBatchSize    int
	Concurrency       int
	HTTPTimeout       time.Duration
	BackoffSchedule   []time.Duration // delay before attempt N+1; len = max attempts - 1
	BackoffJitterFrac float64         // 0.25 → ±25% jitter per interval
	ReclaimThreshold  time.Duration
	HTTPClient        *http.Client // optional override for tests
	Logger            *slog.Logger // optional; defaults to slog.Default()
}

// DefaultWorkerConfig returns the production defaults (spec §6).
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

// StartWorker runs the worker loop until ctx is cancelled. Blocks the caller.
// Callers typically invoke this in its own goroutine from bucketvcs serve boot.
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
	// HTTPClient and Logger remain nil-default if caller didn't set them;
	// downstream code already handles nil (cfg.HTTPClient → new client below;
	// logger via "cfg.Logger fallback to slog.Default()" pattern).
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if err := Reclaim(ctx, svc.db, cfg.ReclaimThreshold); err != nil {
		logger.LogAttrs(ctx, slog.LevelError, "webhooks.reclaim_failed",
			slog.String("error", err.Error()))
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: cfg.HTTPTimeout}
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
					logger.LogAttrs(ctx, slog.LevelError, "webhooks.reclaim_failed",
						slog.String("error", err.Error()),
						slog.String("phase", "periodic"))
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
					deliver(ctx, svc, client, cfg, row, logger)
				}()
			}
		}
	}
}

// claimedRow is the per-row view the worker uses to deliver. Wraps DeliveryRow
// plus the endpoint URL + secret joined at claim time.
type claimedRow struct {
	DeliveryRow
	URL    string
	Secret string
}

// claim atomically transitions up to batch rows from pending → in_flight,
// stamping last_attempt_at and incrementing attempts. Joins the endpoint
// URL + secret in one query.
//
// SQLite already serializes writers, and bucketvcs's documented design is
// single-writer per gateway (see operator guide §11). The transaction
// boundary ensures the SELECT + UPDATE batch is atomic; concurrent worker
// processes — if any — serialize through SQLite's write lock.
//
// The JOIN filters on e.active=1, so disabling an endpoint stops draining
// its pending queue immediately (rows stay pending and resume only if the
// endpoint is re-enabled).
func claim(ctx context.Context, db sqlitestore.Querier, batch int) ([]claimedRow, error) {
	var out []claimedRow
	err := db.RunInTx(ctx, func(tx sqlitestore.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT d.id, d.endpoint_id, d.event_type, d.payload_json, d.attempts,
			        e.url, e.secret
			 FROM webhook_deliveries d
			 JOIN webhook_endpoints e ON e.id = d.endpoint_id
			 WHERE d.status='pending' AND d.next_attempt_at <= ?
			   AND e.active=1
			 ORDER BY d.next_attempt_at
			 LIMIT ?`,
			time.Now().Unix(), batch)
		if err != nil {
			return err
		}
		var claimed []claimedRow
		for rows.Next() {
			var r claimedRow
			if err := rows.Scan(&r.ID, &r.EndpointID, &r.EventType, &r.PayloadJSON, &r.Attempts, &r.URL, &r.Secret); err != nil {
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
				`UPDATE webhook_deliveries
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

// deliver performs one POST attempt for row and updates the DB based on outcome.
func deliver(ctx context.Context, svc *Service, client *http.Client, cfg WorkerConfig, row claimedRow, logger *slog.Logger) {
	start := time.Now()
	t := time.Now().Unix()
	sig := Sign(row.Secret, t, row.PayloadJSON)

	reqCtx, cancel := context.WithTimeout(ctx, cfg.HTTPTimeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, row.URL, bytes.NewReader(row.PayloadJSON))
	if err != nil {
		recordResult(ctx, svc, cfg, row, 0, fmt.Errorf("build request: %w", err), logger, time.Since(start).Milliseconds())
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("BucketVCS-Signature", sig)
	httpReq.Header.Set("X-BucketVCS-Delivery-ID", row.ID)
	httpReq.Header.Set("X-BucketVCS-Event", row.EventType)
	httpReq.Header.Set("User-Agent", "bucketvcs-webhook/M15")
	resp, err := client.Do(httpReq)
	durationMs := time.Since(start).Milliseconds()
	if err != nil {
		recordResult(ctx, svc, cfg, row, 0, err, logger, durationMs)
		return
	}
	defer resp.Body.Close()
	// Drain up to 512 bytes so the connection can be reused. Larger receiver
	// error bodies leave bytes in the socket — fresh TCP/TLS handshake on next
	// retry. Acceptable for Tier 1; widen if connection-reuse becomes a hot path.
	_, _ = io.CopyN(io.Discard, resp.Body, 512)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		recordResult(ctx, svc, cfg, row, resp.StatusCode, nil, logger, durationMs)
		return
	}
	recordResult(ctx, svc, cfg, row, resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode), logger, durationMs)
}

// recordResult writes the post-attempt state. err=nil → delivered. Otherwise,
// if attempts >= len(BackoffSchedule)+1, dead_letter; else back to pending
// with next_attempt_at = NOW + backoff.
func recordResult(ctx context.Context, svc *Service, cfg WorkerConfig, row claimedRow, statusCode int, err error, logger *slog.Logger, durationMs int64) {
	now := time.Now().Unix()
	if err == nil {
		// next_attempt_at is set to NOW (not NULL) because the schema declares
		// next_attempt_at INTEGER NOT NULL; setting it to NOW on terminal rows
		// keeps `delivery list` output clean (no stale "still due" timestamps).
		if _, uerr := svc.db.ExecContext(ctx,
			`UPDATE webhook_deliveries
			   SET status='delivered', delivered_at=?, last_status_code=?, last_error=NULL,
			       next_attempt_at=?
			 WHERE id=?`,
			now, statusCode, now, row.ID,
		); uerr != nil {
			logger.LogAttrs(ctx, slog.LevelError, "webhooks.update_failed",
				slog.String("delivery_id", row.ID),
				slog.String("terminal_status", "delivered"),
				slog.String("error", uerr.Error()))
			return
		}
		EmitDeliveryMetric(ctx, logger, "delivered")
		EmitAttemptDuration(ctx, logger, "delivered", durationMs)
		EmitDelivered(ctx, logger, row.ID, row.EndpointID, row.EventType, row.Attempts, durationMs)
		return
	}
	maxAttempts := len(cfg.BackoffSchedule) + 1
	if row.Attempts >= maxAttempts {
		// next_attempt_at reset to NOW (schema NOT NULL constraint) so
		// `delivery list` doesn't show stale "still due" timestamps on
		// terminal dead_letter rows.
		if _, uerr := svc.db.ExecContext(ctx,
			`UPDATE webhook_deliveries
			   SET status='dead_letter', last_status_code=?, last_error=?,
			       next_attempt_at=?
			 WHERE id=?`,
			statusCode, truncErr(err.Error()), now, row.ID,
		); uerr != nil {
			logger.LogAttrs(ctx, slog.LevelError, "webhooks.update_failed",
				slog.String("delivery_id", row.ID),
				slog.String("terminal_status", "dead_letter"),
				slog.String("error", uerr.Error()))
			return
		}
		EmitDeliveryMetric(ctx, logger, "dead_letter")
		EmitAttemptDuration(ctx, logger, "dead_letter", durationMs)
		EmitDeadLetter(ctx, logger, row.ID, row.EndpointID, row.EventType, row.Attempts, statusCode)
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
		`UPDATE webhook_deliveries
		   SET status='pending', next_attempt_at=?, last_status_code=?, last_error=?
		 WHERE id=?`,
		nextAt, statusCode, truncErr(err.Error()), row.ID,
	); uerr != nil {
		logger.LogAttrs(ctx, slog.LevelError, "webhooks.update_failed",
			slog.String("delivery_id", row.ID),
			slog.String("terminal_status", "retry"),
			slog.String("error", uerr.Error()))
		return
	}
	EmitDeliveryMetric(ctx, logger, "failed_retry")
	EmitAttemptDuration(ctx, logger, "failed_retry", durationMs)
	EmitFailed(ctx, logger, row.ID, row.EndpointID, row.EventType, row.Attempts, statusCode, err.Error(), nextAt)
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
// truncated (downstream display tools handle replacement bytes). Rune-
// aware truncation deferred until a real consumer reports breakage.
func truncErr(s string) string {
	const cap = 512
	if len(s) <= cap {
		return s
	}
	return s[:cap]
}
