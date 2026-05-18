package lfs

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Store is a thin wrapper over storage.ObjectStore scoped to a single
// repository's LFS area. Keys passed in are object IDs (sha256 hex);
// the prefix is applied by the Store.
type Store struct {
	backend storage.ObjectStore
	prefix  string
}

// NewStore returns a Store that prepends prefix to each OID. prefix is
// normalized to end with a slash; callers in M13 always pass
// "<repo-prefix>/lfs/objects/".
func NewStore(backend storage.ObjectStore, prefix string) *Store {
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return &Store{backend: backend, prefix: prefix}
}

// Key returns the full object-store key for the given OID.
func (s *Store) Key(oid string) string { return s.prefix + oid }

// Head reports whether an object exists and its size. exists=false with
// nil err means "not found"; non-nil err means the backend failed.
func (s *Store) Head(ctx context.Context, oid string) (size int64, exists bool, err error) {
	m, err := s.backend.Head(ctx, s.Key(oid))
	if errors.Is(err, storage.ErrNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return m.Size, true, nil
}

// PresignPut returns a signed URL the client can use to PUT one LFS
// object. The size parameter is currently unused; it is reserved for
// the M13 P2 proxied fallback (where size is encoded in the HMAC
// token payload) and for any future Content-Length-bound signing on
// adapters that support it. Returns ErrNotSupported when the backend
// has no native presign (use ProxiedPutURL in that case).
//
// The returned header includes Content-Type: application/octet-stream
// as advisory only; backends do not bind Content-Type into the signed
// URL today. A client that ignores this header still produces a
// working upload; the binding is informational.
func (s *Store) PresignPut(ctx context.Context, oid string, size int64, ttl time.Duration) (string, http.Header, error) {
	_ = size
	url, err := s.backend.SignedGetURL(ctx, s.Key(oid), storage.SignedURLOptions{
		Method:  "PUT",
		Expires: ttl,
	})
	if err != nil {
		return "", nil, err
	}
	hdr := http.Header{}
	hdr.Set("Content-Type", "application/octet-stream")
	return url, hdr, nil
}

// PresignGet returns a signed URL the client can use to GET one LFS
// object. Returns ErrNotSupported when the backend has no native
// presign.
func (s *Store) PresignGet(ctx context.Context, oid string, ttl time.Duration) (string, http.Header, error) {
	url, err := s.backend.SignedGetURL(ctx, s.Key(oid), storage.SignedURLOptions{
		Method:  "GET",
		Expires: ttl,
	})
	if err != nil {
		return "", nil, err
	}
	return url, nil, nil
}

// ProxiedPutURL is a placeholder for the M13 P2 localfs fallback. It
// returns an empty URL today so P1 callers compile. P2 wires this to
// internal/proxiedurl with size encoded in the token payload.
func (s *Store) ProxiedPutURL(oid string, size int64, ttl time.Duration) (string, http.Header) {
	return "", nil
}

// ProxiedGetURL is a placeholder for the M13 P2 localfs fallback.
func (s *Store) ProxiedGetURL(oid string, ttl time.Duration) (string, http.Header) {
	return "", nil
}
