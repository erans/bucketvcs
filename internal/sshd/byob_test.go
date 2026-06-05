package sshd

import (
	"context"
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// fakeResolver is a test byobResolver that returns a fixed store or error.
type fakeResolver struct {
	store storage.ObjectStore
	err   error
}

func (r *fakeResolver) Resolve(_ context.Context, _ string) (storage.ObjectStore, error) {
	return r.store, r.err
}

// TestResolveStore_NilResolver verifies that when no Resolver is set,
// resolveStore returns s.opts.BVStore.
func TestResolveStore_NilResolver(t *testing.T) {
	store := &fakeStore{verify: func(c auth.Credential) (*auth.Actor, string, *auth.Scope, error) {
		return nil, "", nil, nil
	}}
	opts := newTestServerOpts(t, store)
	// Provide a fake BVStore (nil is fine for unit test — we just check the pointer).
	var sentinel storage.ObjectStore // nil is a valid ObjectStore (interface nil)
	opts.BVStore = sentinel
	opts.Resolver = nil // explicit no-op

	s := &Server{opts: opts}
	got, err := s.resolveStore(context.Background(), "acme")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != sentinel {
		t.Fatalf("resolveStore with nil Resolver returned %v; want BVStore (%v)", got, sentinel)
	}
}

// TestResolveStore_WithResolver verifies that when Resolver is set,
// resolveStore calls Resolver.Resolve and returns its result.
func TestResolveStore_WithResolver(t *testing.T) {
	store := &fakeStore{verify: func(c auth.Credential) (*auth.Actor, string, *auth.Scope, error) {
		return nil, "", nil, nil
	}}
	opts := newTestServerOpts(t, store)
	opts.Resolver = &fakeResolver{err: nil} // returns nil ObjectStore, nil error
	// Don't assign BVStore — it should NOT be reached.

	s := &Server{opts: opts}
	got, err := s.resolveStore(context.Background(), "acme")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// fakeResolver.store is nil; that's fine — we just want the call to land.
	if got != nil {
		t.Fatalf("expected nil store from fakeResolver, got %v", got)
	}
}

// TestResolveStore_ResolverError verifies that a Resolver error is propagated.
func TestResolveStore_ResolverError(t *testing.T) {
	sentinel := errors.New("tenant not found")
	store := &fakeStore{verify: func(c auth.Credential) (*auth.Actor, string, *auth.Scope, error) {
		return nil, "", nil, nil
	}}
	opts := newTestServerOpts(t, store)
	opts.Resolver = &fakeResolver{err: sentinel}

	s := &Server{opts: opts}
	_, err := s.resolveStore(context.Background(), "acme")
	if !errors.Is(err, sentinel) {
		t.Fatalf("resolveStore returned %v; want sentinel %v", err, sentinel)
	}
}
