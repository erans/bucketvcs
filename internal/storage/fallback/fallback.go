// Package fallback composes a regional replica bucket over the canonical
// write-region bucket for M26 read gateways. Every object below the root
// manifest is immutable and content-addressed, so "regional first, canonical
// on miss" is always safe — it papers over provider replication's lack of
// ordering guarantees (a replicated root manifest may reference objects that
// have not landed regionally yet). The root manifest — the only mutable
// object — is routed by freshness mode:
//
//	RootFromCanonical (strong-current): root reads always hit the canonical
//	  bucket (the spec §26.2 "verified against the write region").
//	RootFromRegional (bounded-stale): root reads hit the regional bucket,
//	  falling back to canonical only when the regional key does not exist
//	  yet (a new repo inside its first replication window).
//
// All write methods return storage.ErrReadOnlyReplica.
package fallback

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// RootRouting selects the source for root-manifest reads.
type RootRouting int

const (
	// RootFromRegional serves root manifests from the regional bucket
	// (bounded-stale mode), with canonical fallback only on ErrNotFound.
	RootFromRegional RootRouting = iota
	// RootFromCanonical serves root manifests from the canonical bucket
	// unconditionally (strong-current mode).
	RootFromCanonical
)

// rootSuffix matches keys.Repo.RootManifestKey ("<prefix>manifest/root.json").
// Keep in sync with keys.Repo.RootManifestKey.
const rootSuffix = "/manifest/root.json"

// Store is the composed read-only replica store. Immutable after New; safe
// for concurrent use.
type Store struct {
	regional  storage.ObjectStore
	canonical storage.ObjectStore
	routing   RootRouting
	logger    *slog.Logger
}

var _ storage.ObjectStore = (*Store)(nil)

// New composes regional over canonical. logger may be nil (slog.Default).
func New(regional, canonical storage.ObjectStore, routing RootRouting, logger *slog.Logger) *Store {
	if logger == nil {
		logger = slog.Default()
	}
	return &Store{regional: regional, canonical: canonical, routing: routing, logger: logger}
}

func isRoot(key string) bool { return strings.HasSuffix(key, rootSuffix) }

// emitFallback logs one replica_fallback_reads_total sample — the "how warm
// is my regional bucket" signal. No key label (cardinality).
func (s *Store) emitFallback(ctx context.Context) {
	s.logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("metric_name", "replica_fallback_reads_total"),
		slog.Int("value", 1),
	)
}

// Name implements storage.ObjectStore.
func (s *Store) Name() string {
	return "replica(" + s.regional.Name() + "->" + s.canonical.Name() + ")"
}

// Capabilities implements storage.ObjectStore. Signed URLs are reported only
// when BOTH backing stores support them.
func (s *Store) Capabilities() storage.Capabilities {
	caps := s.regional.Capabilities()
	ccaps := s.canonical.Capabilities()
	caps.SignedURLs = caps.SignedURLs && ccaps.SignedURLs
	return caps
}

// Get implements storage.ObjectStore.
func (s *Store) Get(ctx context.Context, key string, opts *storage.GetOptions) (*storage.Object, error) {
	if isRoot(key) && s.routing == RootFromCanonical {
		// Routed read, not a fallback — deliberately no replica_fallback_reads_total emission.
		return s.canonical.Get(ctx, key, opts)
	}
	obj, err := s.regional.Get(ctx, key, opts)
	if errors.Is(err, storage.ErrNotFound) {
		s.emitFallback(ctx)
		return s.canonical.Get(ctx, key, opts)
	}
	return obj, err
}

// Head implements storage.ObjectStore.
func (s *Store) Head(ctx context.Context, key string) (*storage.ObjectMetadata, error) {
	if isRoot(key) && s.routing == RootFromCanonical {
		return s.canonical.Head(ctx, key)
	}
	md, err := s.regional.Head(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		s.emitFallback(ctx)
		return s.canonical.Head(ctx, key)
	}
	return md, err
}

// GetRange implements storage.ObjectStore.
func (s *Store) GetRange(ctx context.Context, key string, start, endInclusive int64) (io.ReadCloser, error) {
	if isRoot(key) && s.routing == RootFromCanonical {
		return s.canonical.GetRange(ctx, key, start, endInclusive)
	}
	rc, err := s.regional.GetRange(ctx, key, start, endInclusive)
	if errors.Is(err, storage.ErrNotFound) {
		s.emitFallback(ctx)
		return s.canonical.GetRange(ctx, key, start, endInclusive)
	}
	return rc, err
}

// List enumerates the REGIONAL bucket only: replica-side enumeration is a
// locality concern, and merging two paginated listings would be both
// expensive and confusing. (gc/maintenance never run against replicas.)
func (s *Store) List(ctx context.Context, prefix string, opts *storage.ListOptions) (*storage.ListPage, error) {
	return s.regional.List(ctx, prefix, opts)
}

// SignedGetURL presigns against whichever bucket actually holds the object:
// Head regional first, presign regional when present, canonical otherwise.
// One extra HEAD buys never presigning a URL to a not-yet-replicated object.
func (s *Store) SignedGetURL(ctx context.Context, key string, opts storage.SignedURLOptions) (string, http.Header, error) {
	// Root manifests are not presigned today (LFS/bundles/packs only), but
	// honour the routing mode for consistency.
	if isRoot(key) && s.routing == RootFromCanonical {
		return s.canonical.SignedGetURL(ctx, key, opts)
	}
	_, err := s.regional.Head(ctx, key)
	switch {
	case err == nil:
		return s.regional.SignedGetURL(ctx, key, opts)
	case errors.Is(err, storage.ErrNotFound):
		s.emitFallback(ctx)
		return s.canonical.SignedGetURL(ctx, key, opts)
	default:
		return "", nil, err
	}
}

// --- write surface: refused (defense in depth under gateway refusals) ---

// PutIfAbsent implements storage.ObjectStore. Always returns ErrReadOnlyReplica.
func (s *Store) PutIfAbsent(ctx context.Context, key string, body io.Reader, opts *storage.PutOptions) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, storage.ErrReadOnlyReplica
}

// PutIfVersionMatches implements storage.ObjectStore. Always returns ErrReadOnlyReplica.
func (s *Store) PutIfVersionMatches(ctx context.Context, key string, expected storage.ObjectVersion, body io.Reader, opts *storage.PutOptions) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, storage.ErrReadOnlyReplica
}

// DeleteIfVersionMatches implements storage.ObjectStore. Always returns ErrReadOnlyReplica.
func (s *Store) DeleteIfVersionMatches(ctx context.Context, key string, expected storage.ObjectVersion) error {
	return storage.ErrReadOnlyReplica
}

// CreateMultipart implements storage.ObjectStore. Always returns ErrReadOnlyReplica.
func (s *Store) CreateMultipart(ctx context.Context, key string, opts *storage.MultipartOptions) (storage.MultipartUpload, error) {
	return nil, storage.ErrReadOnlyReplica
}

// CompleteMultipartIfAbsent implements storage.ObjectStore. Always returns ErrReadOnlyReplica.
func (s *Store) CompleteMultipartIfAbsent(ctx context.Context, upload storage.MultipartUpload, parts []storage.MultipartPart) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, storage.ErrReadOnlyReplica
}
