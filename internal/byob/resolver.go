package byob

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// ResolverConfig parameterizes the StoreResolver.
type ResolverConfig struct {
	// AuthDB is used to look up storage_bindings rows.
	AuthDB *sqlitestore.Store
	// Operator is the fallback store for tenants with no binding.
	Operator storage.ObjectStore
	// EncKey is the 32-byte AES-256-GCM decryption key for creds_json.
	EncKey []byte
	// CredsTTL controls how long a cached tenant store is kept before
	// re-opening on the next request.
	CredsTTL time.Duration
	// OpenStore opens a store from a URL + decrypted credential JSON.
	OpenStore func(url string, creds []byte) (storage.ObjectStore, error)
	// Now is the clock; defaults to time.Now.
	Now func() time.Time
}

type entry struct {
	store     storage.ObjectStore
	expiresAt time.Time
	inflight  chan struct{} // non-nil while opening; closed when done
}

// StoreResolver returns the correct ObjectStore per tenant. Safe for concurrent use.
type StoreResolver struct {
	cfg   ResolverConfig
	mu    sync.Mutex
	cache map[string]*entry
}

// NewResolver builds a StoreResolver. CredsTTL must be > 0.
func NewResolver(cfg ResolverConfig) *StoreResolver {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.CredsTTL <= 0 {
		cfg.CredsTTL = time.Hour
	}
	return &StoreResolver{cfg: cfg, cache: map[string]*entry{}}
}

// Resolve returns the tenant's ObjectStore, opening and caching on first call.
// Falls back to the operator store when no binding exists.
func (r *StoreResolver) Resolve(ctx context.Context, tenant string) (storage.ObjectStore, error) {
	now := r.cfg.Now()

	r.mu.Lock()
	e, ok := r.cache[tenant]
	if ok && e.inflight == nil && now.Before(e.expiresAt) {
		r.mu.Unlock()
		return e.store, nil
	}
	if ok && e.inflight != nil {
		// Another goroutine is opening; wait for it.
		ch := e.inflight
		r.mu.Unlock()
		<-ch
		return r.Resolve(ctx, tenant) // re-enter to read result
	}
	// Cache miss or expired: take responsibility for opening.
	e = &entry{inflight: make(chan struct{})}
	r.cache[tenant] = e
	r.mu.Unlock()

	s, err := r.openTenant(ctx, tenant)

	r.mu.Lock()
	ch := e.inflight
	e.inflight = nil
	if err != nil {
		delete(r.cache, tenant)
	} else {
		e.store = s
		e.expiresAt = now.Add(r.cfg.CredsTTL)
	}
	r.mu.Unlock()
	close(ch)

	return s, err
}

func (r *StoreResolver) openTenant(ctx context.Context, tenant string) (storage.ObjectStore, error) {
	b, err := r.cfg.AuthDB.GetStorageBinding(ctx, tenant)
	if err != nil {
		if err == auth.ErrNoSuchBinding {
			return r.cfg.Operator, nil
		}
		return nil, fmt.Errorf("byob: binding for %s: %w", tenant, err)
	}
	plain, err := Decrypt(r.cfg.EncKey, b.CredsJSON)
	if err != nil {
		return nil, fmt.Errorf("byob: decrypt creds for %s: %w", tenant, err)
	}
	s, err := r.cfg.OpenStore(b.StoreURL, plain)
	if err != nil {
		return nil, fmt.Errorf("byob: open store for %s: %w", tenant, err)
	}
	return s, nil
}

// Invalidate drops a cached tenant store, forcing re-open on next Resolve.
func (r *StoreResolver) Invalidate(tenant string) {
	r.mu.Lock()
	delete(r.cache, tenant)
	r.mu.Unlock()
}
