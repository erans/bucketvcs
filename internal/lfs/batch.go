package lfs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// validOID matches the LFS spec's lowercase hex SHA-256 OID format
// (64 chars, [0-9a-f]). Rejects anything containing path separators,
// control chars, uppercase hex, or wrong length — preventing per-repo
// namespace escape via crafted OIDs that would otherwise concatenate
// directly into storage keys ("../../other-tenant/file") and bypass
// the per-repo prefix on localfs.
var validOID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Build is the pure LFS Batch protocol logic: given a parsed request
// and a Store scoped to the target repo, it returns the response shape
// the HTTP handler should serialize. Build performs:
//
//   - Validation of operation (must be "upload" or "download") and
//     transfers (must include "basic" or be empty, which the LFS spec
//     treats as implicit basic).
//   - Per-object Head against the store.
//   - For upload: missing -> upload+verify actions; present with matching
//     size -> empty actions (already uploaded); present with mismatched
//     size -> per-object 422 error.
//   - For download: present -> download action; missing -> per-object 404.
//
// Build never returns top-level errors for per-object conditions. The
// only top-level errors are: unsupported operation (caller -> 422) and
// missing basic transfer (caller -> 422). The handler is responsible
// for mapping those to HTTP responses.
//
// On a presign error other than ErrNotSupported, Build records a
// per-object 500 error rather than poisoning the whole response. On
// ErrNotSupported, Build falls back to Store.ProxiedPutURL /
// ProxiedGetURL (today these return empty strings; Phase 2 wires the
// real proxied HMAC URL). When that fallback also yields an empty
// URL (the current P1 stub behavior), Build records a per-object 503
// so the LFS client sees a clear failure rather than an Action with
// an empty Href.
//
// For upload operations, Build also mints a verify action via
// Store.ProxiedVerifyURL (kind=5 HMAC token). When the Store was not
// configured with WithProxied — i.e. the operator has not provided
// --proxied-url-signing-key and --proxied-url-base — the verify URL is
// empty and Build records a per-object 503. The verify mechanism is no
// longer an Authorization-echo of the inbound bearer; the verify
// action carries a short-TTL token bound to (kind=lfs-verify, tenant,
// repo, oid) — not consume-on-use, so it can be replayed against the
// same OID within its TTL but cannot be repurposed for upload, download,
// or another object.
func Build(ctx context.Context, req BatchRequest, store *Store, presignTTL time.Duration) (BatchResponse, error) {
	if req.Operation != "upload" && req.Operation != "download" {
		return BatchResponse{}, fmt.Errorf("lfs: unsupported operation %q", req.Operation)
	}
	if len(req.Transfers) > 0 {
		// Per the spec, an explicit Transfers list must contain "basic".
		ok := false
		for _, t := range req.Transfers {
			if t == "basic" {
				ok = true
				break
			}
		}
		if !ok {
			return BatchResponse{}, fmt.Errorf("lfs: client did not advertise the 'basic' transfer")
		}
	}
	resp := BatchResponse{Transfer: "basic", Objects: make([]ObjectAction, 0, len(req.Objects))}
	for _, ref := range req.Objects {
		resp.Objects = append(resp.Objects, buildOne(ctx, ref, req.Operation, store, presignTTL))
	}
	return resp, nil
}

func buildOne(ctx context.Context, ref ObjectRef, op string, store *Store, ttl time.Duration) ObjectAction {
	out := ObjectAction{OID: ref.OID, Size: ref.Size}
	// Validate OID FIRST. The OID is concatenated into storage keys
	// downstream; an unvalidated OID like "../../other-tenant/file"
	// would escape the per-repo namespace on localfs. Reject anything
	// not matching the LFS spec format (64 lowercase hex chars).
	if !validOID.MatchString(ref.OID) {
		out.Error = &ObjectError{Code: 422, Message: "invalid oid: must be lowercase hex SHA-256"}
		return out
	}
	// Validate size: negative size always rejects, but size==0 only
	// rejects on upload (a zero-byte file download is legitimate).
	if ref.Size < 0 {
		out.Error = &ObjectError{Code: 422, Message: fmt.Sprintf("invalid size: %d", ref.Size)}
		return out
	}
	if op == "upload" && ref.Size == 0 {
		out.Error = &ObjectError{Code: 422, Message: "upload size must be > 0"}
		return out
	}
	size, exists, err := store.Head(ctx, ref.OID)
	if err != nil {
		out.Error = &ObjectError{Code: 500, Message: "head failed: " + err.Error()}
		return out
	}
	switch op {
	case "upload":
		if exists {
			if size == ref.Size {
				// Already present at the claimed size: empty actions
				// tells git-lfs "skip this upload".
				return out
			}
			out.Error = &ObjectError{Code: 422, Message: fmt.Sprintf("size mismatch: stored=%d, claimed=%d", size, ref.Size)}
			return out
		}
		url, hdr, err := store.PresignPut(ctx, ref.OID, ref.Size, ttl)
		if errors.Is(err, storage.ErrNotSupported) {
			url2, hdr2 := store.ProxiedPutURL(ref.OID, ref.Size, ttl)
			url = url2
			hdr = hdr2
			err = nil
		}
		if err != nil {
			out.Error = &ObjectError{Code: 500, Message: "presign failed: " + err.Error()}
			return out
		}
		if url == "" {
			out.Error = &ObjectError{
				Code:    503,
				Message: "object storage does not support presigned URLs and the proxied-URL fallback is not yet wired (M13 P2)",
			}
			return out
		}
		verifyURL, verifyHdr := store.ProxiedVerifyURL(ref.OID, ttl)
		if verifyURL == "" {
			out.Error = &ObjectError{
				Code:    503,
				Message: "verify URL unavailable; --proxied-url-signing-key and --proxied-url-base required when --lfs is enabled",
			}
			return out
		}
		out.Actions = map[string]Action{
			"upload": {Href: url, Header: headerMap(hdr)},
			"verify": {Href: verifyURL, Header: headerMap(verifyHdr)},
		}
	case "download":
		if !exists {
			out.Error = &ObjectError{Code: 404, Message: "object not found"}
			return out
		}
		url, hdr, err := store.PresignGet(ctx, ref.OID, ttl)
		if errors.Is(err, storage.ErrNotSupported) {
			url2, hdr2 := store.ProxiedGetURL(ref.OID, ttl)
			url = url2
			hdr = hdr2
			err = nil
		}
		if err != nil {
			out.Error = &ObjectError{Code: 500, Message: "presign failed: " + err.Error()}
			return out
		}
		if url == "" {
			out.Error = &ObjectError{
				Code:    503,
				Message: "object storage does not support presigned URLs and the proxied-URL fallback is not yet wired (M13 P2)",
			}
			return out
		}
		out.Actions = map[string]Action{
			"download": {Href: url, Header: headerMap(hdr)},
		}
	}
	return out
}

// headerMap converts an http.Header (map[string][]string) into the
// single-valued map[string]string the LFS wire format uses. Every
// header key present in h is forwarded; for multi-valued keys, only
// the first value is preserved (LFS wire format is single-valued, and
// signed-URL headers we forward are single-valued in practice).
// Nil/empty header -> nil map (matches LFS spec omitempty).
//
// If a future backend returns multi-value signed headers under one
// key, this function will need to switch to RFC 7230 list-rule
// joining or fail loudly.
func headerMap(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, vs := range h {
		if len(vs) == 0 {
			continue
		}
		out[k] = vs[0]
	}
	return out
}
