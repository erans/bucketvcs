package authreplica

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/benbjohnson/litestream"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// DefaultLeaseTTL is the lease validity window; renewal runs every TTL/3.
const DefaultLeaseTTL = 60 * time.Second

// Config configures authdb replication.
type Config struct {
	DBPath      string
	Store       storage.ObjectStore
	Prefix      string        // defaults to DefaultPrefix
	LeaseTTL    time.Duration // defaults to DefaultLeaseTTL
	SkipRestore bool          // operator escape hatch: --auth-db-replica-skip-restore
	Logger      *slog.Logger
}

// Runner ties the lease, restore-on-boot, and litestream replication to the
// serve lifecycle. Two phases, matching serve's boot order:
//
//	r, err := Prepare(ctx, cfg)   // lease + restore-iff-missing; BEFORE sqlitestore.Open
//	...open authdb...
//	err = r.StartReplication(ctx) // litestream store + heartbeat;  AFTER sqlitestore.Open
//	...
//	r.Close(ctx)                  // shutdown sync → litestream close → lease release
type Runner struct {
	cfg    Config
	logger *slog.Logger
	lease  *Lease
	client *Client

	mu      sync.Mutex
	lsdb    *litestream.DB
	lsstore *litestream.Store
	stopped bool

	// hbCancel/hbDone are written only during single-threaded boot
	// (StartReplication) and read in Close; the lifecycle contract is
	// single-caller — Close must not race StartReplication or itself.
	hbCancel context.CancelFunc
	hbDone   chan struct{}
}

// Prepare acquires the lease and restores the DB from the replica iff the
// local file is missing. Restore failures are fatal by design (fail-closed):
// booting with an empty authdb while a replica exists would fork history.
func Prepare(ctx context.Context, cfg Config) (*Runner, error) {
	if cfg.Prefix == "" {
		cfg.Prefix = DefaultPrefix
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = DefaultLeaseTTL
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With(slog.String("subsystem", "authreplica"))

	r := &Runner{
		cfg:    cfg,
		logger: logger,
		lease:  NewLease(cfg.Store, cfg.Prefix, cfg.LeaseTTL),
		client: NewClient(cfg.Store, cfg.Prefix),
	}
	r.client.SetLogger(slog.New(newLevelFilterHandler(logger.Handler(), slog.LevelWarn)))

	if err := r.lease.Acquire(ctx); err != nil {
		return nil, err
	}
	if took, prev := r.lease.TookOver(); took {
		logger.LogAttrs(ctx, slog.LevelInfo, "authdb.replica.lease_takeover",
			slog.Bool("audit", true),
			slog.String("event", "authdb.replica.lease_takeover"),
			slog.String("instance_id", r.lease.InstanceID()),
			slog.String("previous_instance_id", prev),
		)
	}

	lsdb := litestream.NewDB(cfg.DBPath)
	lsdb.Logger = slog.New(newLevelFilterHandler(logger.Handler(), slog.LevelWarn))
	lsdb.Replica = litestream.NewReplicaWithClient(lsdb, r.client)
	r.lsdb = lsdb

	if !cfg.SkipRestore {
		// EnsureExists is a no-op when the local DB already exists, so emitting
		// the restored audit event unconditionally would fire a phantom event
		// on every clean restart. Only emit when the file was actually missing
		// AND EnsureExists materialized it (an empty bucket leaves it missing).
		_, statErr := os.Stat(cfg.DBPath)
		wasMissing := os.IsNotExist(statErr)
		start := time.Now()
		if err := lsdb.EnsureExists(ctx); err != nil {
			_ = r.lease.Release(ctx)
			// This path means the replica could not be READ (storage fault) —
			// an empty replica location no-ops successfully. Do not advertise
			// --auth-db-replica-skip-restore here: bypassing restore while
			// live replica data exists would fork history.
			return nil, fmt.Errorf("authreplica: restore-on-boot: %w "+
				"(fail-closed: the replica location could not be read; fix storage "+
				"access and restart — do NOT bypass with --auth-db-replica-skip-restore "+
				"unless the replica location is known to hold no data)", err)
		}
		_, postStatErr := os.Stat(cfg.DBPath)
		fileNowExists := postStatErr == nil
		if wasMissing && fileNowExists {
			logger.LogAttrs(ctx, slog.LevelInfo, "authdb.replica.restored",
				slog.Bool("audit", true),
				slog.String("event", "authdb.replica.restored"),
				slog.String("db_path", cfg.DBPath),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			)
		}
	}
	return r, nil
}

// StartReplication opens the litestream store (replication + compaction) and
// the lease heartbeat. Call after the authdb file exists (sqlitestore.Open).
func (r *Runner) StartReplication(ctx context.Context) error {
	levels := litestream.CompactionLevels{
		{Level: 0},
		{Level: 1, Interval: 30 * time.Second},
		{Level: 2, Interval: 5 * time.Minute},
	}
	st := litestream.NewStore([]*litestream.DB{r.lsdb}, levels)
	st.Logger = slog.New(newLevelFilterHandler(r.logger.Handler(), slog.LevelWarn))
	if err := st.Open(ctx); err != nil {
		_ = st.Close(ctx) // best-effort: reap any goroutines a partial Open started
		return fmt.Errorf("authreplica: open litestream store: %w", err)
	}
	r.mu.Lock()
	r.lsstore = st
	r.mu.Unlock()

	hbCtx, cancel := context.WithCancel(context.Background())
	r.hbCancel = cancel
	r.hbDone = make(chan struct{})
	go r.heartbeat(hbCtx)
	r.logger.Info("authdb replication started",
		slog.String("backend", r.cfg.Store.Name()), slog.String("prefix", r.cfg.Prefix))
	return nil
}

// heartbeat renews the lease every TTL/3. Lease loss stops replication but
// NOT the server: the lease protects the replica lineage, not the local DB.
func (r *Runner) heartbeat(ctx context.Context) {
	defer close(r.hbDone)
	t := time.NewTicker(r.cfg.LeaseTTL / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			err := r.lease.Renew(ctx)
			if err == nil {
				continue
			}
			// Clean shutdown: Close cancels hbCtx while a renew may be in
			// flight — that is not a renew failure, don't meter/log it.
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				return
			}
			if errors.Is(err, ErrLeaseLost) {
				r.logger.LogAttrs(ctx, slog.LevelError, "authdb.replica.lease_lost",
					slog.Bool("audit", true),
					slog.String("event", "authdb.replica.lease_lost"),
					slog.String("instance_id", r.lease.InstanceID()),
				)
				r.stopReplication(ctx)
				return
			}
			r.logger.LogAttrs(ctx, slog.LevelInfo, "metric",
				slog.String("metric_name", "authdb_replica_lease_renew_errors_total"),
				slog.Int64("value", 1))
			r.logger.Warn("lease renew failed (will retry)", slog.Any("error", err))
		}
	}
}

// stopReplication closes the litestream store without releasing the lease
// (we no longer own it). Safe to call once; Close handles the normal path.
func (r *Runner) stopReplication(ctx context.Context) {
	r.mu.Lock()
	st := r.lsstore
	r.lsstore = nil
	r.stopped = true
	r.mu.Unlock()
	if st == nil {
		return
	}
	if err := st.Close(ctx); err != nil {
		r.logger.Error("close litestream store after lease loss", slog.Any("error", err))
	}
	r.logger.LogAttrs(ctx, slog.LevelError, "authdb.replica.replication_stopped",
		slog.Bool("audit", true),
		slog.String("event", "authdb.replica.replication_stopped"))
}

// SyncNow forces a full WAL→LTX→store sync. Used by tests (and available to
// callers needing a deterministic flush); shutdown's final sync happens
// inside the litestream store Close.
func (r *Runner) SyncNow(ctx context.Context) error {
	r.mu.Lock()
	lsdb := r.lsdb
	r.mu.Unlock()
	if lsdb == nil {
		return nil
	}
	return lsdb.SyncAndWait(ctx)
}

// Close performs the ordered shutdown: heartbeat off → final sync via the
// litestream store close (bounded by its ShutdownSyncTimeout) → lease release.
func (r *Runner) Close(ctx context.Context) error {
	if r.hbCancel != nil {
		r.hbCancel()
		<-r.hbDone
		r.hbCancel = nil
	}
	r.mu.Lock()
	st := r.lsstore
	r.lsstore = nil
	lost := r.stopped
	r.mu.Unlock()

	var firstErr error
	if st != nil {
		if err := st.Close(ctx); err != nil {
			firstErr = fmt.Errorf("authreplica: close litestream store: %w", err)
		}
	}
	if !lost { // only release a lease we still own
		if err := r.lease.Release(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
