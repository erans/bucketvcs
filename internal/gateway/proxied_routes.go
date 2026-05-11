package gateway

import (
	"context"
	"errors"
	"io"
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
func NewProxiedHandler(store storage.ObjectStore, key []byte, bundlePrefix, packPrefix string, resolver ProxiedKeyResolver) http.Handler {
	return &proxiedHandler{
		store:        store,
		key:          key,
		bundlePrefix: bundlePrefix,
		packPrefix:   packPrefix,
		resolver:     resolver,
		now:          time.Now,
	}
}

type proxiedHandler struct {
	store        storage.ObjectStore
	key          []byte
	bundlePrefix string
	packPrefix   string
	resolver     ProxiedKeyResolver
	now          func() time.Time
}

func (h *proxiedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var kind, hash string
	switch {
	case strings.HasPrefix(r.URL.Path, h.bundlePrefix):
		kind = "bundle"
		hash = strings.TrimPrefix(r.URL.Path, h.bundlePrefix)
	case strings.HasPrefix(r.URL.Path, h.packPrefix):
		kind = "pack"
		hash = strings.TrimPrefix(r.URL.Path, h.packPrefix)
	default:
		http.NotFound(w, r)
		return
	}
	if hash == "" {
		http.NotFound(w, r)
		return
	}
	// Defense-in-depth: reject hashes that don't match the documented
	// charset before they reach the resolver. URL-decoded path segments
	// can contain "/" or ".." which would otherwise be passed verbatim
	// to a resolver that trusts callers to validate. We accept only
	// "sha256-<64-hex>" for bundles and 40-hex for packs; everything
	// else 404s indistinguishably from an unadvertised hash.
	if !validProxiedHash(kind, hash) {
		http.NotFound(w, r)
		return
	}
	// Token verification BEFORE resolver dispatch: a probe with no/bad
	// token gets a uniform 403 regardless of whether the hash is
	// advertised, so an unauthenticated attacker can't enumerate which
	// hashes this gateway serves by toggling between 403 and 404.
	tok := r.URL.Query().Get("token")
	if tok == "" {
		http.Error(w, "missing token", http.StatusForbidden)
		return
	}
	if _, err := proxiedurl.Verify(h.key, tok, kind, hash, h.now()); err != nil {
		if errors.Is(err, proxiedurl.ErrTokenExpired) {
			http.Error(w, "token expired", http.StatusForbidden)
			return
		}
		http.Error(w, "invalid token", http.StatusForbidden)
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
		http.NotFound(w, r)
		return
	}
	h.serveObject(r.Context(), w, r, storageKey)
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

func (h *proxiedHandler) serveObject(ctx context.Context, w http.ResponseWriter, r *http.Request, key string) {
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
