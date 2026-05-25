package hooks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// InternalErrorBehavior controls what RunPreReceive does when a hook fails
// for a non-rejection reason (script missing, sandbox setup, fork error).
type InternalErrorBehavior int

const (
	// InternalErrorReject is the default: fail-closed (push rejected).
	InternalErrorReject InternalErrorBehavior = iota
	// InternalErrorAllow flips to fail-open (push proceeds).
	InternalErrorAllow
)

// ServiceConfig is the orchestration-layer config (separate from RunnerConfig
// which is per-subprocess). Populated from operator flags.
type ServiceConfig struct {
	PostReceiveConcurrency int                   // worker pool goroutines
	PostReceiveQueueSize   int                   // channel buffer
	OnInternalError        InternalErrorBehavior // pre-receive only
	Logger                 *slog.Logger
}

// Service is the public surface called from receivepack. Owns the Store, the
// Runner, and the post-receive worker pool.
type Service struct {
	store  *Store
	runner *Runner
	cfg    ServiceConfig
	worker *Worker
	logger *slog.Logger
}

func NewService(store *Store, runnerCfg RunnerConfig, svcCfg ServiceConfig) *Service {
	logger := svcCfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if runnerCfg.Logger == nil {
		runnerCfg.Logger = logger
	}
	runner := NewRunner(runnerCfg)
	svc := &Service{
		store:  store,
		runner: runner,
		cfg:    svcCfg,
		logger: logger,
	}
	svc.worker = newWorker(svc, svcCfg.PostReceiveConcurrency, svcCfg.PostReceiveQueueSize, logger)
	svc.worker.start()
	return svc
}

// Close shuts down the post-receive worker pool. Blocks until in-flight jobs
// finish (bounded by RunnerConfig.TimeoutSec via context).
// closeDrainTimeout bounds Close's wait for the post-receive worker pool.
// With defaults (queue 256 × timeout 30s ÷ concurrency 8) a fully-loaded
// queue could otherwise block shutdown ~16 minutes; this cap matches the
// "fire-and-forget, best-effort" contract for post-receive. Operators
// needing durable retry use M15 webhooks.
const closeDrainTimeout = 10 * time.Second

func (s *Service) Close() error {
	if s.worker != nil {
		s.worker.stop(closeDrainTimeout)
	}
	return nil
}

// RunPreReceive executes each enabled pre-receive hook for (tenant, repo) in
// sort_order. Fail-fast on first non-zero exit: returns *HookRejection.
// Internal errors map to *HookRejection (default) or pass (InternalErrorAllow).
func (s *Service) RunPreReceive(ctx context.Context, p PreReceivePayload) error {
	rows, err := s.store.ListActiveForTrigger(ctx, p.Tenant, p.Repo, TriggerPreReceive)
	if err != nil {
		EmitHookInternalError(ctx, s.logger, p.Tenant, p.Repo, TriggerPreReceive, "", p.PushID, err)
		if s.cfg.OnInternalError == InternalErrorAllow {
			return nil
		}
		return &HookRejection{ScriptName: "(list-active)", ExitCode: -1,
			Stderr: []byte(fmt.Sprintf("hooks list error: %v", err))}
	}
	if len(rows) == 0 {
		return nil
	}
	stdin := PreReceiveStdin(p)
	env := s.preReceiveEnv(p)
	for _, row := range rows {
		startNanos := nowNanos()
		res := s.runner.Run(ctx, p.BareDir, row.ScriptName, stdin, env)
		dur := nowNanos() - startNanos
		if res.Err != nil {
			// Internal error path. Detailed error (with absolute script
			// path, fork failure, etc.) stays in the policy.hook.internal_error
			// audit event; only the generic sentinel propagates to the
			// caller so we don't leak server-side filesystem paths to the
			// pushing client via the report-status `ng` line.
			EmitHookInternalError(ctx, s.logger, p.Tenant, p.Repo, TriggerPreReceive, row.ScriptName, p.PushID, res.Err)
			EmitPreReceiveMetric(ctx, s.logger, p.Tenant, p.Repo, "error")
			if errors.Is(res.Err, ErrTimeout) {
				// Timeouts are the script's fault, not the server's; surface
				// "hook timed out" through the user-facing HookRejection
				// path so the client gets a meaningful single-line reason.
				return &HookRejection{ScriptName: row.ScriptName, ExitCode: -1, Stderr: []byte("hook timed out")}
			}
			if s.cfg.OnInternalError == InternalErrorAllow {
				continue
			}
			// Return the bare sentinel so receivepack's errors.As check
			// distinguishes internal-error (generic reason) from rejection
			// (script-supplied reason). Wrap with %w so test code can still
			// errors.Is for the underlying cause.
			return fmt.Errorf("%w: %s: %w", ErrInternal, row.ScriptName, res.Err)
		}
		if res.ExitCode != 0 {
			EmitHookRejected(ctx, s.logger, p.Tenant, p.Repo, TriggerPreReceive, row.ScriptName,
				res.ExitCode, p.PushID, p.Actor, res.Stderr)
			EmitPreReceiveMetric(ctx, s.logger, p.Tenant, p.Repo, "rejected")
			return &HookRejection{ScriptName: row.ScriptName, ExitCode: res.ExitCode, Stderr: res.Stderr}
		}
		EmitPreReceiveDuration(ctx, s.logger, p.Tenant, p.Repo, dur)
	}
	EmitPreReceiveMetric(ctx, s.logger, p.Tenant, p.Repo, "accepted")
	return nil
}

// EnqueuePostReceive hands the payload off to the worker. Non-blocking; full
// queue drops the job with a metric + WARN.
func (s *Service) EnqueuePostReceive(p PostReceivePayload) {
	if s.worker == nil {
		return
	}
	s.worker.enqueue(p)
}

// preReceiveEnv assembles the per-hook env merged on top of RunnerConfig.ExtraEnv.
func (s *Service) preReceiveEnv(p PreReceivePayload) map[string]string {
	return map[string]string{
		"BUCKETVCS_TENANT":  p.Tenant,
		"BUCKETVCS_REPO":    p.Repo,
		"BUCKETVCS_TRIGGER": TriggerPreReceive,
		"BUCKETVCS_PUSH_ID": p.PushID,
		"BUCKETVCS_ACTOR":   p.Actor,
	}
}

// postReceiveEnv mirrors preReceiveEnv with TxID + ManifestVersion exposed too.
func (s *Service) postReceiveEnv(p PostReceivePayload) map[string]string {
	return map[string]string{
		"BUCKETVCS_TENANT":          p.Tenant,
		"BUCKETVCS_REPO":            p.Repo,
		"BUCKETVCS_TRIGGER":         TriggerPostReceive,
		"BUCKETVCS_PUSH_ID":         p.PushID,
		"BUCKETVCS_ACTOR":           p.Actor,
		"BUCKETVCS_TX_ID":           p.TxID,
		"BUCKETVCS_STORAGE_BACKEND": p.StorageBackend,
	}
}

func nowNanos() int64 { return timeNow().UnixNano() }

// timeNow is overridable in tests; production uses time.Now.
var timeNow = func() time.Time { return time.Now() }
