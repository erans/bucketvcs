package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/proxiedurl"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// ProxiedKeyResolver maps a hash advertised on the URL path to the
// storage key the gateway should fetch. Implementations decide how to
// scope hash -> repo (typically via a single-repo gateway, or a
// multi-repo gateway with the repo embedded in the URL prefix).
//
// For M11 the simplest production deployment is one gateway per
// (tenant, repo); a multi-repo deployment can extend the URL pattern
// to include a tenant/repo segment in a successor milestone.
type ProxiedKeyResolver interface {
	// BundleKey returns the durable storage key for a bundle whose
	// content-addressed hash is `hash` (e.g., "sha256-aabbcc...").
	// ok=false means the hash is not advertised by this gateway.
	BundleKey(hash string) (string, bool)
	// PackKey returns the durable storage key for a canonical pack whose
	// pack-checksum is `hash` (40-hex SHA-1).
	PackKey(hash string) (string, bool)
}

// NewProxiedHandler returns an http.Handler serving /_bundle/<hash> and
// /_pack/<hash> from store, gated by HMAC tokens minted with key.
//
// The handler is mounted at root; the prefix arguments determine which
// path segment it serves. Pass "/_bundle/" and "/_pack/" for the M11
// defaults.
//
// logger is used for served-* metrics and the proxied.url.served audit
// event. If nil, slog.Default() is used.
func NewProxiedHandler(store storage.ObjectStore, key []byte, bundlePrefix, packPrefix string, resolver ProxiedKeyResolver, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &proxiedHandler{
		store:        store,
		key:          key,
		bundlePrefix: bundlePrefix,
		packPrefix:   packPrefix,
		resolver:     resolver,
		now:          time.Now,
		logger:       logger,
	}
}

type proxiedHandler struct {
	store        storage.ObjectStore
	key          []byte
	bundlePrefix string
	packPrefix   string
	resolver     ProxiedKeyResolver
	now          func() time.Time
	logger       *slog.Logger
}

func (h *proxiedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Wrap the writer to count body bytes written. Wrapping after the
	// method-not-allowed check means 405 responses bypass the counter
	// entirely. HEAD requests DO go through the counter (the method check
	// passes), but the HEAD short-circuits in serveObject return before
	// reaching the emitServed call sites, so served-* metrics fire only
	// on successful GET (200/206) paths regardless of HEAD's traversal.
	cw := &countingWriter{ResponseWriter: w}
	var kind, hash string
	switch {
	case strings.HasPrefix(r.URL.Path, h.bundlePrefix):
		kind = "bundle"
		hash = strings.TrimPrefix(r.URL.Path, h.bundlePrefix)
	case strings.HasPrefix(r.URL.Path, h.packPrefix):
		kind = "pack"
		hash = strings.TrimPrefix(r.URL.Path, h.packPrefix)
	default:
		http.NotFound(cw, r)
		return
	}
	if hash == "" {
		http.NotFound(cw, r)
		return
	}
	// Defense-in-depth: reject hashes that don't match the documented
	// charset before they reach the resolver. URL-decoded path segments
	// can contain "/" or ".." which would otherwise be passed verbatim
	// to a resolver that trusts callers to validate. We accept only
	// "sha256-<64-hex>" for bundles and 40-hex for packs; everything
	// else 404s indistinguishably from an unadvertised hash.
	if !validProxiedHash(kind, hash) {
		http.NotFound(cw, r)
		return
	}
	// Token verification BEFORE resolver dispatch: a probe with no/bad
	// token gets a uniform 403 regardless of whether the hash is
	// advertised, so an unauthenticated attacker can't enumerate which
	// hashes this gateway serves by toggling between 403 and 404.
	//
	// The metric's reason label is a 4-value bounded vocabulary:
	// "missing" (no token query param), "expired" (exp time passed),
	// "kind_mismatch" (token minted for a different endpoint kind), or
	// "invalid" (HMAC/base64/hash failure, the catch-all). Missing and
	// invalid have different operational remediations, so they're split
	// rather than folded. User-facing 403 bodies do NOT leak the
	// kind_mismatch distinction (collapsed to "invalid token") so
	// attackers can't probe the verifier's classification.
	tok := r.URL.Query().Get("token")
	if tok == "" {
		emitMetric(r.Context(), h.logger, "proxied_url_token_invalid_total", 1, "reason", "missing")
		http.Error(cw, "missing token", http.StatusForbidden)
		return
	}
	if _, err := proxiedurl.Verify(h.key, tok, kind, hash, h.now()); err != nil {
		reason := "invalid"
		msg := "invalid token"
		switch {
		case errors.Is(err, proxiedurl.ErrTokenExpired):
			reason, msg = "expired", "token expired"
		case errors.Is(err, proxiedurl.ErrKindMismatch):
			reason = "kind_mismatch"
		}
		emitMetric(r.Context(), h.logger, "proxied_url_token_invalid_total", 1, "reason", reason)
		http.Error(cw, msg, http.StatusForbidden)
		return
	}
	// Only after the token validates do we ask the resolver to map
	// hash -> storage key. An unadvertised hash here means the operator
	// minted a token for a hash this gateway no longer (or never)
	// serves; surface that as 404 to match content-addressed semantics.
	var storageKey string
	switch kind {
	case "bundle":
		if k, ok := h.resolver.BundleKey(hash); ok {
			storageKey = k
		}
	case "pack":
		if k, ok := h.resolver.PackKey(hash); ok {
			storageKey = k
		}
	}
	if storageKey == "" {
		http.NotFound(cw, r)
		return
	}
	h.serveObject(r.Context(), cw, r, kind, hash, storageKey)
}

// validProxiedHash returns true if hash matches the on-the-wire charset
// for the given kind. Bundles use "sha256-<64-hex>" (content-addressed
// SHA-256). Packs use 40-hex (Git's pack-trailer SHA-1, our PackChecksum).
// Anything else — slashes, dot segments, query strings that survived
// upstream cleanup — is rejected before reaching the resolver.
func validProxiedHash(kind, hash string) bool {
	switch kind {
	case "bundle":
		const prefix = "sha256-"
		if !strings.HasPrefix(hash, prefix) {
			return false
		}
		return isHex(hash[len(prefix):], 64)
	case "pack":
		return isHex(hash, 40)
	}
	return false
}

func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

func (h *proxiedHandler) serveObject(ctx context.Context, w *countingWriter, r *http.Request, kind, hash, key string) {
	rangeHdr := r.Header.Get("Range")
	if rangeHdr == "" {
		// Full object.
		meta, err := h.store.Head(ctx, key)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
		w.Header().Set("Accept-Ranges", "bytes")
		// Short-circuit on HEAD BEFORE issuing Get. On network-backed
		// adapters (S3/GCS/Azure) Get starts streaming bytes that would
		// otherwise be discarded by the writer — paying for a body the
		// client explicitly said it doesn't want.
		if r.Method == http.MethodHead {
			return
		}
		obj, err := h.store.Get(ctx, key, nil)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		defer obj.Body.Close()
		_, _ = io.Copy(w, obj.Body)
		h.emitServed(ctx, kind, hash, w.bytes, http.StatusOK, false)
		return
	}
	// Range: bytes=<start>-<end>
	start, end, ok := parseSimpleByteRange(rangeHdr)
	if !ok {
		http.Error(w, "invalid Range", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	// Head first so we can: (a) reject ranges past EOF with a definitive
	// 416 (adapters like localfs return a no-error empty reader for
	// start >= size, which would otherwise surface to the client as a
	// 206 with an empty body), and (b) populate Content-Range with the
	// total instead of "/*". The extra round-trip is acceptable because
	// the v2 client only fetches ranges a handful of times per session.
	meta, herr := h.store.Head(ctx, key)
	if herr != nil {
		writeStoreError(w, herr)
		return
	}
	if start >= meta.Size {
		http.Error(w, "range start past EOF", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if end >= meta.Size {
		end = meta.Size - 1
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	// With Head's size in hand we can emit the precise total instead of
	// RFC 9110 §14.4's "/*" wildcard.
	w.Header().Set("Content-Range",
		"bytes "+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(end, 10)+
			"/"+strconv.FormatInt(meta.Size, 10))
	// Content-Length on a 206 is the length of THIS slice, exact because
	// we clamped end above. Accept-Ranges advertises ongoing range support
	// so a client doing a HEAD probe before resumption sees it without us
	// also handling the full-object path.
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.Header().Set("Accept-Ranges", "bytes")
	// Short-circuit on HEAD with a Range header BEFORE GetRange. Adapters
	// like S3 begin streaming bytes on GetRange; the writer would discard
	// them but the read still happens. RFC 9110 §15.3.7 says a HEAD with
	// Range either returns the corresponding 206 headers (no body) or
	// ignores Range — we do the former.
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusPartialContent)
		return
	}
	rc, err := h.store.GetRange(ctx, key, start, end)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	defer rc.Close()
	w.WriteHeader(http.StatusPartialContent)
	_, _ = io.Copy(w, rc)
	h.emitServed(ctx, kind, hash, w.bytes, http.StatusPartialContent, true)
}

// writeStoreError maps storage sentinel errors to HTTP status codes.
// ErrNotFound -> 404 (object was GC'd or never existed); ErrInvalidArgument
// -> 416 (range outside object bounds or otherwise malformed past our
// pre-flight parse). Anything else is genuinely unexpected -> 500. Shared
// between the full-object and range serve paths so a transient backend
// failure on the full-object path doesn't get mis-reported as "definitive
// not found" — which the v2 client would treat as "bundle GC'd, fall back
// to full clone".
func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storage.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, storage.ErrInvalidArgument):
		http.Error(w, "invalid Range", http.StatusRequestedRangeNotSatisfiable)
	default:
		http.Error(w, "storage error", http.StatusInternalServerError)
	}
}

// parseSimpleByteRange handles the only Range forms M11 advertises:
// "bytes=N-M" with both N and M present. Multi-range, suffix ("bytes=-M"),
// and open-ended ("bytes=N-") forms are intentionally rejected — the v2
// bundle-uri / packfile-uri clients only emit explicit closed ranges,
// and rejecting the rest keeps the parser obvious. Callers requesting an
// unsupported form get 416 Requested Range Not Satisfiable.
func parseSimpleByteRange(h string) (start, end int64, ok bool) {
	if !strings.HasPrefix(h, "bytes=") {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(h, "bytes=")
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return 0, 0, false
	}
	s, err1 := strconv.ParseInt(parts[0], 10, 64)
	e, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || s < 0 || e < s {
		return 0, 0, false
	}
	return s, e, true
}

// emitServed emits the served-* metric pair and the proxied.url.served audit
// event. Called only on successful GET (200/206) paths; HEAD probes and error
// paths do not emit.
//
// Truncated-serve note: emitServed fires after io.Copy returns, regardless of
// whether the copy completed in full. A client that disconnects mid-stream
// produces a counted served_total event with bytes_served reflecting only
// what reached the wire. Operators using served_total as a fan-out measure
// of "completed downloads" should compare bytes_served against the object
// size to distinguish full from truncated serves; the metric pair does not
// surface the boolean separately.
func (h *proxiedHandler) emitServed(ctx context.Context, kind, hash string, bytesServed int64, statusCode int, rangeRequest bool) {
	// Metric: bundle_uri_served_total / pack_uri_served_total
	emitMetric(ctx, h.logger, kind+"_uri_served_total", 1, "via", "proxied")
	// Metric: bundle_uri_served_bytes / pack_uri_served_bytes
	emitMetric(ctx, h.logger, kind+"_uri_served_bytes", bytesServed, "via", "proxied")
	// Audit event
	emitProxiedURLServed(ctx, h.logger, kind, hash, bytesServed, statusCode, rangeRequest)
}

// countingWriter wraps an http.ResponseWriter to record the number of body
// bytes written. Used by the proxied handler to report actual bytes served
// in the bundle_uri_served_bytes / pack_uri_served_bytes metrics and in the
// proxied.url.served audit event's bytes_served field. The count reflects
// what reached the client (which may be less than Content-Length when the
// client disconnects mid-stream).
//
// Intentionally does NOT promote http.Flusher / http.Hijacker / http.Pusher
// from the wrapped writer — the proxied routes only do plain Write of
// bundle/pack bytes (no SSE, WebSocket, or HTTP/2 push). A future handler
// that needs those interfaces should re-wrap with explicit passthroughs.
type countingWriter struct {
	http.ResponseWriter
	bytes int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.bytes += int64(n)
	return n, err
}
