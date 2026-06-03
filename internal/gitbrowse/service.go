// Package gitbrowse implements read-only git-content browsing for the web UI,
// using the hybrid strategy: refstore (manifest path, no mirror) for refs, and
// the shared mirror.Manager + git shell-outs for tree/blob/log/commit/diff.
// It returns browsemodel DTOs so it structurally satisfies web.ContentStore.
package gitbrowse

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/browsemodel"
	"github.com/bucketvcs/bucketvcs/internal/mirror"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// DefaultTimeout bounds synchronous cold-mirror materialization per request.
const DefaultTimeout = 20 * time.Second

// Service reads git content for a tenant/repo.
type Service struct {
	store   storage.ObjectStore
	mgr     *mirror.Manager
	timeout time.Duration
	logger  *slog.Logger
}

// NewService constructs a Service. timeout <= 0 uses DefaultTimeout.
func NewService(store storage.ObjectStore, mgr *mirror.Manager, timeout time.Duration, logger *slog.Logger) *Service {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{store: store, mgr: mgr, timeout: timeout, logger: logger}
}

// openMirror opens (materializing if cold) the bare mirror and takes its read
// lock. The returned release func MUST be called to drop the lock.
//
// Timeout scope: s.timeout bounds cold mirror materialization (mgr.Open) only.
// The m.RLock() call that follows can additionally block if an in-flight push
// or maintenance run holds the repository write lock — this wait is unbounded,
// matching the gateway's fetch-path semantics. Similarly, the git reads that
// run after the lock is acquired operate under the caller's request context, not
// this timeout. A materialization that exceeds s.timeout is reported as
// browsemodel.ErrWarming.
func (s *Service) openMirror(ctx context.Context, tenant, repo string) (*mirror.Mirror, func(), error) {
	start := time.Now()
	octx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	m, err := s.mgr.Open(octx, tenant, repo)
	s.logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("name", "web_browse_mirror_wait_seconds"),
		slog.Float64("value", time.Since(start).Seconds()),
	)
	if err != nil {
		if errors.Is(octx.Err(), context.DeadlineExceeded) {
			// A real backend failure can coincide with the deadline; surface it
			// to operators even though the client sees a generic "warming" 503.
			s.logger.LogAttrs(ctx, slog.LevelWarn, "browse mirror open exceeded timeout",
				slog.String("tenant", tenant), slog.String("repo", repo),
				slog.String("err", err.Error()),
			)
			return nil, nil, browsemodel.ErrWarming
		}
		return nil, nil, err
	}
	m.RLock()
	return m, m.RUnlock, nil
}
