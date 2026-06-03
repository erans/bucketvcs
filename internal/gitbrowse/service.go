// Package gitbrowse implements read-only git-content browsing for the web UI,
// using the hybrid strategy: refstore (manifest path, no mirror) for refs, and
// the shared mirror.Manager + git shell-outs for tree/blob/log/commit/diff.
// It returns browsemodel DTOs so it structurally satisfies web.ContentStore.
package gitbrowse

import (
	"context"
	"errors"
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
}

// NewService constructs a Service. timeout <= 0 uses DefaultTimeout.
func NewService(store storage.ObjectStore, mgr *mirror.Manager, timeout time.Duration) *Service {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Service{store: store, mgr: mgr, timeout: timeout}
}

// openMirror opens (materializing if cold) the bare mirror and takes its read
// lock. The returned release func MUST be called to drop the lock. A
// materialization that exceeds s.timeout is reported as browsemodel.ErrWarming.
func (s *Service) openMirror(ctx context.Context, tenant, repo string) (*mirror.Mirror, func(), error) {
	octx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	m, err := s.mgr.Open(octx, tenant, repo)
	if err != nil {
		if errors.Is(octx.Err(), context.DeadlineExceeded) {
			return nil, nil, browsemodel.ErrWarming
		}
		return nil, nil, err
	}
	m.RLock()
	return m, m.RUnlock, nil
}
