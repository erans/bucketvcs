package lfs

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/proxiedurl"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Store is a thin wrapper over storage.ObjectStore scoped to a single
// repository's LFS area. Keys passed in are object IDs (sha256 hex);
// the prefix is applied by the Store.
type Store struct {
	backend storage.ObjectStore
	prefix  string

	// Proxied-URL config; zero values disable the ProxiedPutURL/
	// ProxiedGetURL minting (they fall back to returning "", nil — the
	// P1 stub behavior — which Build then surfaces as a per-object 503).
	proxiedKey     []byte
	proxiedBaseURL string
	proxiedTenant  string
	proxiedRepo    string
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
// The returned header carries Content-Type: application/octet-stream
// (advisory; backends do not bind Content-Type into the signed URL
// today) PLUS any backend-required headers — Azure Blob, for example,
// adds `x-ms-blob-type: BlockBlob`, without which the PUT would 400.
// Callers MUST forward every header in the returned set when invoking
// the URL.
//
// Content-Type collision policy: backend wins. If the backend returns
// its own Content-Type (no adapter does today, but future ones may),
// the LFS default is replaced rather than appended-behind-it. This
// avoids the silent-drop hazard where downstream first-value emitters
// would discard whichever value lost the race. Adapters that have an
// opinion about Content-Type get to express it.
func (s *Store) PresignPut(ctx context.Context, oid string, size int64, ttl time.Duration) (string, http.Header, error) {
	_ = size
	url, backendHdr, err := s.backend.SignedGetURL(ctx, s.Key(oid), storage.SignedURLOptions{
		Method:  "PUT",
		Expires: ttl,
	})
	if err != nil {
		return "", nil, err
	}
	hdr := http.Header{}
	// Copy backend headers first, then set the LFS default Content-Type
	// only if the backend did not supply one.
	for k, vs := range backendHdr {
		for _, v := range vs {
			hdr.Add(k, v)
		}
	}
	if hdr.Get("Content-Type") == "" {
		hdr.Set("Content-Type", "application/octet-stream")
	}
	return url, hdr, nil
}

// PresignGet returns a signed URL the client can use to GET one LFS
// object. Returns ErrNotSupported when the backend has no native
// presign. The returned header is whatever the backend reports (most
// backends return nil here; Azure Blob does not require headers on
// GET).
func (s *Store) PresignGet(ctx context.Context, oid string, ttl time.Duration) (string, http.Header, error) {
	url, backendHdr, err := s.backend.SignedGetURL(ctx, s.Key(oid), storage.SignedURLOptions{
		Method:  "GET",
		Expires: ttl,
	})
	if err != nil {
		return "", nil, err
	}
	return url, backendHdr, nil
}

// WithProxied configures the Store to mint proxied transfer URLs in
// ProxiedPutURL / ProxiedGetURL. Pass an HMAC signing key (>= 16
// bytes), the external base URL of the gateway, and the (tenant,
// repo) pair the Store is scoped to.
//
// Returns the same Store so the call can be chained:
//
//     lfs.NewStore(backend, prefix).WithProxied(key, baseURL, t, r)
//
// If signingKey is empty (zero-length), proxied methods continue to
// return empty URLs — useful for tests that exercise only the presign
// path.
func (s *Store) WithProxied(signingKey []byte, baseURL, tenant, repo string) *Store {
	if len(signingKey) > 0 && len(signingKey) < 16 {
		panic("lfs.Store.WithProxied: signing key must be >= 16 bytes (got " + strconv.Itoa(len(signingKey)) + ")")
	}
	s.proxiedKey = signingKey
	s.proxiedBaseURL = baseURL
	s.proxiedTenant = tenant
	s.proxiedRepo = repo
	return s
}

// ProxiedPutURL mints a gateway-proxied URL the LFS client uses to PUT
// the object. The returned URL is HMAC-signed via internal/proxiedurl
// and expires after ttl. Returns ("", nil) if WithProxied was not
// called (preserving the P1 stub behavior).
//
// Size is currently informational — passed in so the proxied handler
// can enforce an upper bound at PUT time, but not encoded in the token
// today. The 5 GiB hard cap is applied by the proxied handler via
// http.MaxBytesReader regardless of the size argument.
func (s *Store) ProxiedPutURL(oid string, size int64, ttl time.Duration) (string, http.Header) {
	_ = size // reserved for future Content-Length-bound signing
	if len(s.proxiedKey) == 0 || s.proxiedBaseURL == "" {
		return "", nil
	}
	hash := s.proxiedTenant + "/" + s.proxiedRepo + "/" + oid
	tok, err := proxiedurl.Mint(s.proxiedKey, "lfs-put", hash, time.Now().Add(ttl))
	// TODO: if Mint fails, we return ("", nil) which the Batch handler
	// surfaces as per-object 503 without diagnostic context. Plumbing a
	// logger through Store is heavier than P2 scope warrants for this
	// extremely rare path; revisit if operators see unexplained 503s.
	if err != nil {
		return "", nil
	}
	u := s.proxiedBaseURL + "/_lfs/" + s.proxiedTenant + "/" + s.proxiedRepo + "/" + oid + "?token=" + tok
	hdr := http.Header{}
	hdr.Set("Content-Type", "application/octet-stream")
	return u, hdr
}

// ProxiedGetURL mints a gateway-proxied URL the LFS client uses to GET
// the object. The returned URL is HMAC-signed via internal/proxiedurl
// and expires after ttl. Returns ("", nil) if WithProxied was not
// called (preserving the pre-P2 stub behavior so the Batch handler
// surfaces a per-object 503 when proxied transfer is not configured).
func (s *Store) ProxiedGetURL(oid string, ttl time.Duration) (string, http.Header) {
	if len(s.proxiedKey) == 0 || s.proxiedBaseURL == "" {
		return "", nil
	}
	hash := s.proxiedTenant + "/" + s.proxiedRepo + "/" + oid
	tok, err := proxiedurl.Mint(s.proxiedKey, "lfs-get", hash, time.Now().Add(ttl))
	// TODO: if Mint fails, we return ("", nil) which the Batch handler
	// surfaces as per-object 503 without diagnostic context. Plumbing a
	// logger through Store is heavier than P2 scope warrants for this
	// extremely rare path; revisit if operators see unexplained 503s.
	if err != nil {
		return "", nil
	}
	u := s.proxiedBaseURL + "/_lfs/" + s.proxiedTenant + "/" + s.proxiedRepo + "/" + oid + "?token=" + tok
	return u, nil
}

// ProxiedVerifyURL mints a gateway-proxied URL the LFS client POSTs to
// verify an uploaded object (M13.1). The URL is the same as the
// proxied PUT/GET URL — the HTTP method (POST) selects the verify
// branch in the proxied handler. The returned header carries
// "Authorization: Bearer bvtv_<token>" with the same token encoded in
// the URL ?token= parameter; the gateway reads the URL token, the
// header is LFS-protocol-convention and lets forensics distinguish
// verify tokens (bvtv_) from M4 session tokens (bvts_).
//
// Returns ("", nil) if WithProxied was not called (stub preserved for
// tests that exercise only the presign path).
func (s *Store) ProxiedVerifyURL(oid string, ttl time.Duration) (string, http.Header) {
	if len(s.proxiedKey) == 0 || s.proxiedBaseURL == "" {
		return "", nil
	}
	hash := s.proxiedTenant + "/" + s.proxiedRepo + "/" + oid
	tok, err := proxiedurl.Mint(s.proxiedKey, "lfs-verify", hash, time.Now().Add(ttl))
	if err != nil {
		return "", nil
	}
	u := s.proxiedBaseURL + "/_lfs/" + s.proxiedTenant + "/" + s.proxiedRepo + "/" + oid + "?token=" + tok
	hdr := http.Header{}
	// The bvtv_ prefix is FORENSIC-ONLY — the gateway validates the
	// URL `?token=` parameter, not this header. Log-grep tools spot a
	// kind=5 verify token by the bvtv_ prefix (vs. M4 bvts_ session
	// tokens); changing this prefix breaks forensic grep, not auth.
	hdr.Set("Authorization", "Bearer bvtv_"+tok)
	return u, hdr
}
