package hooks

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Worker runs post-receive hooks asynchronously off a bounded queue. The
// queue is buffered (size = ServiceConfig.PostReceiveQueueSize); a full
// queue drops the incoming job with a metric + WARN. Lost on serve restart
// — durable post-receive needs M15 webhooks instead.
type Worker struct {
	svc     *Service
	in      chan PostReceivePayload
	workers int
	logger  *slog.Logger
	wg      sync.WaitGroup

	// mu protects the closed state. enqueue takes RLock for the duration
	// of the send; stop takes Lock before close(in). Together these
	// eliminate the send-on-closed-channel panic window: once stop holds
	// Lock no enqueue can be mid-send.
	mu     sync.RWMutex
	closed bool

	// ctx is the worker's own cancellable context, passed into Runner.Run
	// so an in-flight subprocess can be cancelled on shutdown.
	ctx    context.Context
	cancel context.CancelFunc
}

func newWorker(svc *Service, concurrency, queueSize int, logger *slog.Logger) *Worker {
	if concurrency < 1 {
		concurrency = 1
	}
	if queueSize < 1 {
		queueSize = 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Worker{
		svc:     svc,
		in:      make(chan PostReceivePayload, queueSize),
		workers: concurrency,
		logger:  logger,
		ctx:     ctx,
		cancel:  cancel,
	}
}

func (w *Worker) start() {
	for i := 0; i < w.workers; i++ {
		w.wg.Add(1)
		go w.loop()
	}
}

// stop signals shutdown:
//  1. Flips closed=true under Lock so subsequent enqueues short-circuit.
//  2. Closes the in channel so workers' for-range exits when the buffer drains.
//  3. Waits for workers to finish with a bounded drainTimeout. If the deadline
//     hits first, cancels w.ctx so in-flight subprocesses are SIGKILLed by
//     exec.CommandContext, then waits a brief grace before giving up.
//
// The "give up" branch leaks the worker goroutines, but that only happens
// during process shutdown — the goroutines die with the process. The bound
// keeps Service.Close() from blocking the whole gateway shutdown for minutes
// on a deep post-receive queue.
func (w *Worker) stop(drainTimeout time.Duration) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	close(w.in)
	w.mu.Unlock()

	done := make(chan struct{})
	go func() { w.wg.Wait(); close(done) }()

	if drainTimeout <= 0 {
		// Caller wants immediate cancel; signal in-flight subprocesses
		// and wait a brief grace for them to exit.
		w.cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		return
	}

	select {
	case <-done:
		// All workers drained the queue cleanly within the deadline.
	case <-time.After(drainTimeout):
		// Cancel in-flight subprocesses (Runner uses w.ctx → CommandContext
		// → SIGTERM + WaitDelay → SIGKILL grace). Give them 2s to wind
		// down, then leak any stragglers — the process is exiting anyway.
		w.cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
}

// enqueue is non-blocking. Drops on full queue OR if stop() has already
// flipped closed. RLock guarantees stop() cannot close(w.in) while a send
// is in progress, eliminating the panic window.
func (w *Worker) enqueue(p PostReceivePayload) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		EmitPostReceiveMetric(context.Background(), w.logger, p.Tenant, p.Repo, "dropped")
		w.logger.Warn("hooks: post-receive enqueue after Close, dropping job",
			"tenant", p.Tenant, "repo", p.Repo)
		return
	}
	select {
	case w.in <- p:
	default:
		EmitPostReceiveMetric(context.Background(), w.logger, p.Tenant, p.Repo, "dropped")
		w.logger.Warn("hooks: post-receive queue full, dropping job",
			"tenant", p.Tenant, "repo", p.Repo)
	}
}

func (w *Worker) loop() {
	defer w.wg.Done()
	for p := range w.in {
		w.runOne(p)
	}
}

func (w *Worker) runOne(p PostReceivePayload) {
	// Use w.ctx so stop() can cancel in-flight subprocess execution.
	ctx := w.ctx
	rows, err := w.svc.store.ListActiveForTrigger(ctx, p.Tenant, p.Repo, TriggerPostReceive)
	if err != nil {
		EmitHookInternalError(ctx, w.logger, p.Tenant, p.Repo, TriggerPostReceive, "", p.PushID, err)
		EmitPostReceiveMetric(ctx, w.logger, p.Tenant, p.Repo, "error")
		return
	}
	if len(rows) == 0 {
		return
	}
	stdin := PostReceiveStdin(p)
	env := w.svc.postReceiveEnv(p)
	for _, row := range rows {
		startNanos := nowNanos()
		res := w.svc.runner.Run(ctx, p.BareDir, row.ScriptName, stdin, env)
		dur := nowNanos() - startNanos
		switch {
		case res.Err != nil:
			EmitHookInternalError(ctx, w.logger, p.Tenant, p.Repo, TriggerPostReceive, row.ScriptName, p.PushID, res.Err)
			EmitPostReceiveMetric(ctx, w.logger, p.Tenant, p.Repo, "error")
		case res.ExitCode != 0:
			EmitHookRejected(ctx, w.logger, p.Tenant, p.Repo, TriggerPostReceive, row.ScriptName,
				res.ExitCode, p.PushID, p.Actor, res.Stderr)
			EmitPostReceiveMetric(ctx, w.logger, p.Tenant, p.Repo, "nonzero")
		default:
			EmitPostReceiveMetric(ctx, w.logger, p.Tenant, p.Repo, "ok")
			EmitPostReceiveDuration(ctx, w.logger, p.Tenant, p.Repo, dur)
		}
	}
}
