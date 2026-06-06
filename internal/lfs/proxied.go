package lfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/lfs/quota"
	"github.com/bucketvcs/bucketvcs/internal/proxiedurl"
	"github.com/bucketvcs/bucketvcs/internal/replica"
	"github.com/bucketvcs/bucketvcs/internal/shiplog"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

// maxLFSObjectSize is the hard contract for /_lfs/ PUT body size. The
// `size` field in Batch responses is advisory; the gateway accepts any
// body up to this limit and rejects with 413 otherwise. Per the M13
// spec §4, this matches the S3 single-PUT limit and is generous for
// localfs / Azure / GCS deployments.
const maxLFSObjectSize = 5 << 30

// ProxiedDeps is the dependency surface NewProxiedObjectHandler needs.
type ProxiedDeps struct {
	// Store is the underlying object store; LFS object bytes are
	// written via PutIfAbsent and read via Get.
	Store storage.ObjectStore

	// Key is the HMAC signing key shared with Store.WithProxied.
	Key []byte

	// Logger is used for metric + audit emission. Nil falls back to
	// slog.Default().
	Logger *slog.Logger

	// Quota is OPTIONAL. When non-nil, the verify handler calls
	// Quota.Add on successful verify (after the size check passes).
	// When nil, no counter changes occur.
	Quota *quota.Service

	// Webhooks is OPTIONAL. When non-nil, the verify handler enqueues
	// EventLFSUpload after a successful verify (200 path). Fail-open.
	Webhooks *webhooks.Service

	// ReadOnlyReplica refuses proxied LFS upload (PUT) and verify (POST)
	// with a clean 403 carrying WriteRegionURL — even when a valid
	// write-region token is replayed against this replica. Downloads
	// (GET/HEAD) are unaffected.
	ReadOnlyReplica bool
	WriteRegionURL  string

	// Usage is OPTIONAL. When non-nil, the verify handler emits an
	// lfs_upload usage event after a successful verify (the object is
	// durably accepted). Nil-safe.
	Usage UsageSink
}

// NewProxiedObjectHandler returns the http.Handler mounted at /_lfs/
// for gateway-proxied LFS object PUT and GET. The handler is the
// terminal owner of the request — no upstream auth runs; the token
// is the authorization.
func NewProxiedObjectHandler(deps ProxiedDeps) http.Handler {
	if deps.Store == nil {
		panic("lfs.NewProxiedObjectHandler: ProxiedDeps.Store is required")
	}
	if len(deps.Key) < 16 {
		panic("lfs.NewProxiedObjectHandler: ProxiedDeps.Key must be >= 16 bytes")
	}
	h := &proxiedObjectHandler{
		store:           deps.Store,
		key:             deps.Key,
		logger:          deps.Logger,
		now:             time.Now,
		quota:           deps.Quota,
		webhooks:        deps.Webhooks,
		readOnlyReplica: deps.ReadOnlyReplica,
		writeRegionURL:  deps.WriteRegionURL,
		usage:           deps.Usage,
	}
	if h.logger == nil {
		h.logger = slog.Default()
	}
	return h
}

type proxiedObjectHandler struct {
	store           storage.ObjectStore
	key             []byte
	logger          *slog.Logger
	now             func() time.Time
	quota           *quota.Service
	webhooks        *webhooks.Service
	readOnlyReplica bool
	writeRegionURL  string
	usage           UsageSink
}

func (h *proxiedObjectHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Method gate first so unsupported methods take a uniform 405 path
	// regardless of path content.
	switch r.Method {
	case http.MethodPut, http.MethodGet, http.MethodHead, http.MethodPost:
	default:
		w.Header().Set("Allow", "GET, HEAD, POST, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Token presence check BEFORE path validation. This denies an
	// unauthenticated caller the ability to distinguish "valid path
	// syntax" from "invalid path syntax" — both return 403 uniformly.
	// Path-format validation still runs below, but only AFTER we've
	// confirmed a token is present.
	tok := r.URL.Query().Get("token")
	if tok == "" {
		emitMetric(ctx, h.logger, "lfs_object_token_invalid_total", 1, "reason", "missing")
		http.Error(w, "missing token", http.StatusForbidden)
		return
	}

	tenant, repo, oid, ok := splitProxiedLFSPath(r.URL.Path)
	if !ok {
		// Path invalid AND token was present — return 403 with
		// "invalid token" so we don't leak the path-validity signal.
		emitMetric(ctx, h.logger, "lfs_object_token_invalid_total", 1, "reason", "invalid")
		http.Error(w, "invalid token", http.StatusForbidden)
		return
	}

	// Determine operation and setup the expected kind and hash for verification.
	// POST is the verify endpoint (M13.1); PUT is upload; GET/HEAD is download.
	var expectedKind, op string
	switch r.Method {
	case http.MethodPut:
		expectedKind, op = "lfs-put", "upload"
	case http.MethodPost:
		expectedKind, op = "lfs-verify", "verify"
	default: // GET, HEAD
		expectedKind, op = "lfs-get", "download"
	}
	expectedHash := tenant + "/" + repo + "/" + oid

	// Token verification AFTER path parsing but BEFORE storage. Token
	// presence has already been checked above; here we verify the token
	// matches the parsed (tenant, repo, oid) tuple. A token minted for a
	// different repo or OID gets a uniform 403 — matching the path-invalid
	// branch above so attackers can't enumerate via response code.
	if _, err := proxiedurl.Verify(h.key, tok, expectedKind, expectedHash, h.now()); err != nil {
		reason := "invalid"
		msg := "invalid token"
		switch {
		case errors.Is(err, proxiedurl.ErrTokenExpired):
			reason, msg = "expired", "token expired"
		case errors.Is(err, proxiedurl.ErrKindMismatch):
			reason = "kind_mismatch"
		}
		emitMetric(ctx, h.logger, "lfs_object_token_invalid_total", 1, "reason", reason)
		http.Error(w, msg, http.StatusForbidden)
		return
	}

	storageKey := RepoLFSPrefix(tenant, repo) + oid
	hash := tenant + "/" + repo + "/" + oid

	// Replica refusal for the write-region surfaces (upload PUT + verify
	// POST). The token is valid, but this gateway is a read-only replica;
	// short-circuit BEFORE any store call so the operator gets a clean 403
	// instead of a 500 from ErrReadOnlyReplica in the storage error branch.
	// GET/HEAD downloads are intentionally unaffected.
	if h.readOnlyReplica && (r.Method == http.MethodPut || r.Method == http.MethodPost) {
		WriteError(w, http.StatusForbidden, replica.RefusalMessage(h.writeRegionURL))
		return
	}

	if r.Method == http.MethodPost {
		h.serveVerify(ctx, w, r, tenant, repo, oid)
		return
	}
	if r.Method == http.MethodPut {
		h.servePut(ctx, w, r, op, hash, oid, storageKey)
		return
	}
	h.serveGet(ctx, w, r, op, hash, storageKey)
}

// splitProxiedLFSPath parses /_lfs/<tenant>/<repo>/<oid>. Validates
// tenant/repo against validRouteName (handler.go) and OID against
// validOID (batch.go).
func splitProxiedLFSPath(p string) (tenant, repo, oid string, ok bool) {
	rest := strings.TrimPrefix(p, "/_lfs/")
	if rest == p { // prefix mismatch
		return "", "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return "", "", "", false
	}
	tenant, repo, oid = parts[0], parts[1], parts[2]
	if !validRouteName(tenant) || !validRouteName(repo) {
		return "", "", "", false
	}
	if !validOID.MatchString(oid) {
		return "", "", "", false
	}
	return tenant, repo, oid, true
}

func (h *proxiedObjectHandler) servePut(ctx context.Context, w http.ResponseWriter, r *http.Request, op, hash, oid, key string) {
	body := http.MaxBytesReader(w, r.Body, maxLFSObjectSize)
	defer body.Close()

	hasher := sha256.New()
	teed := io.TeeReader(body, hasher)
	cr := &countingReader{r: teed}

	version, err := h.store.PutIfAbsent(ctx, key, cr, nil)

	// MaxBytesReader error short-circuits before hash check — the hash
	// is incomplete and the bytes were not stored. 413 with no
	// verification side effects.
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		emitObjectServedMetric(ctx, h.logger, op, "too_large")
		emitLFSObjectServed(ctx, h.logger, op, hash, cr.n, http.StatusRequestEntityTooLarge)
		return
	}

	// On ErrAlreadyExists, the backend may have short-circuited after
	// the existence check without consuming the body. Drain the rest
	// so the hasher sees the complete payload and we can confirm the
	// client's intended bytes match the stored OID.
	if errors.Is(err, storage.ErrAlreadyExists) {
		if _, copyErr := io.Copy(io.Discard, cr); copyErr != nil {
			// Drain failed (e.g. MaxBytesReader trip mid-drain) —
			// surface 413 if that's the cause, else 500.
			if errors.As(copyErr, &maxErr) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				emitObjectServedMetric(ctx, h.logger, op, "too_large")
				emitLFSObjectServed(ctx, h.logger, op, hash, cr.n, http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "storage error", http.StatusInternalServerError)
			emitObjectServedMetric(ctx, h.logger, op, "error")
			emitLFSObjectServed(ctx, h.logger, op, hash, cr.n, http.StatusInternalServerError)
			return
		}
	} else if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		emitObjectServedMetric(ctx, h.logger, op, "error")
		emitLFSObjectServed(ctx, h.logger, op, hash, cr.n, http.StatusInternalServerError)
		return
	}

	// Verify SHA-256(body) == oid. LFS content-addressing is a hard
	// invariant: a tenant with write perms must NOT be able to plant
	// mismatched bytes at an OID slot.
	actualHash := hex.EncodeToString(hasher.Sum(nil))
	if actualHash != oid {
		// If we just wrote bad bytes, best-effort delete.
		if err == nil {
			_ = h.store.DeleteIfVersionMatches(ctx, key, version)
		}
		http.Error(w, "content hash mismatch", http.StatusUnprocessableEntity)
		emitObjectServedMetric(ctx, h.logger, op, "hash_mismatch")
		emitLFSObjectServed(ctx, h.logger, op, hash, cr.n, http.StatusUnprocessableEntity)
		return
	}

	// Success path. Distinguish "newly stored" from "already existed"
	// for the metric label, but the wire response is identical 200.
	w.WriteHeader(http.StatusOK)
	if errors.Is(err, storage.ErrAlreadyExists) {
		emitObjectServedMetric(ctx, h.logger, op, "exists")
	} else {
		emitObjectServedMetric(ctx, h.logger, op, "ok")
	}
	emitLFSObjectServed(ctx, h.logger, op, hash, cr.n, http.StatusOK)
}

func (h *proxiedObjectHandler) serveGet(ctx context.Context, w http.ResponseWriter, r *http.Request, op, hash, key string) {
	meta, err := h.store.Head(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		emitObjectServedMetric(ctx, h.logger, op, "missing")
		emitLFSObjectServed(ctx, h.logger, op, hash, 0, http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		emitObjectServedMetric(ctx, h.logger, op, "error")
		emitLFSObjectServed(ctx, h.logger, op, hash, 0, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	if r.Method == http.MethodHead {
		emitObjectServedMetric(ctx, h.logger, op, "ok")
		emitLFSObjectServed(ctx, h.logger, op, hash, 0, http.StatusOK)
		return
	}
	obj, err := h.store.Get(ctx, key, nil)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		emitObjectServedMetric(ctx, h.logger, op, "error")
		emitLFSObjectServed(ctx, h.logger, op, hash, 0, http.StatusInternalServerError)
		return
	}
	defer obj.Body.Close()
	cw := &countingWriter{ResponseWriter: w}
	copied, _ := io.Copy(cw, obj.Body)
	if copied != meta.Size {
		// Truncated mid-stream — Content-Length was already sent so we
		// can't change the status code, but we can emit a different
		// metric/audit label so dashboards see the failure.
		emitObjectServedMetric(ctx, h.logger, op, "error")
		emitLFSObjectServed(ctx, h.logger, op, hash, cw.n, http.StatusOK)
		return
	}
	emitObjectServedMetric(ctx, h.logger, op, "ok")
	emitLFSObjectServed(ctx, h.logger, op, hash, cw.n, http.StatusOK)
}

// serveVerify handles POST /_lfs/<tenant>/<repo>/<oid>. The body is a
// VerifyRequest carrying {oid, size}. Behavior mirrors handler.go's
// handleVerify exactly for storage-side outcomes — the only difference
// vs. the route-based verify endpoint is the auth mechanism (kind=5
// HMAC token instead of a route-mounted RunAuth check).
//
// The token has already been verified by ServeHTTP before this method
// is called; serveVerify is only responsible for body validation and
// the Verify(store, oid, size) dispatch.
//
// Auth note: token-authenticated, no actor in context. The audit event's
// user attribute is empty string — operators correlate via the repo and
// oid fields.
//
// M17 scope note: verify-token (M13.1) is an HMAC bearer minted by the
// gateway alongside the upload presign — it does not carry an auth.Actor
// or token scopes. The token's binding to (tenant, repo, oid) IS the
// authorization. The corresponding lfs:write scope check is enforced
// upstream at the Batch handler (handler.go handleBatch) when the
// "upload" operation request is received and the verify URL is minted.
func (h *proxiedObjectHandler) serveVerify(ctx context.Context, w http.ResponseWriter, r *http.Request, tenant, repo, oid string) {
	verifyStart := h.now()
	repoFQN := tenant + "/" + repo

	// LFS spec uses application/vnd.git-lfs+json. Mirror handler.go's
	// tolerate-empty / reject-mismatched policy.
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mt, _, perr := mime.ParseMediaType(ct)
		if perr != nil || mt != ContentType {
			WriteError(w, http.StatusUnsupportedMediaType, "expected Content-Type: "+ContentType)
			emitVerifyRequestMetric(ctx, h.logger, "error")
			emitLFSVerify(ctx, h.logger, repoFQN, "", oid, 0, "error")
			return
		}
	}

	// Body is small (just {oid, size}); cap at 64 KiB.
	body := http.MaxBytesReader(w, r.Body, 64*1024)
	var vreq VerifyRequest
	if err := json.NewDecoder(body).Decode(&vreq); err != nil {
		WriteError(w, http.StatusUnprocessableEntity, "unprocessable: "+err.Error())
		emitVerifyRequestMetric(ctx, h.logger, "error")
		emitLFSVerify(ctx, h.logger, repoFQN, "", oid, 0, "error")
		return
	}
	if vreq.OID != oid {
		WriteError(w, http.StatusUnprocessableEntity, "body oid does not match URL oid")
		emitVerifyRequestMetric(ctx, h.logger, "error")
		emitLFSVerify(ctx, h.logger, repoFQN, "", oid, vreq.Size, "error")
		return
	}

	store := NewStore(h.store, RepoLFSPrefix(tenant, repo))
	err := Verify(ctx, store, oid, vreq.Size)
	switch {
	case err == nil:
		w.Header().Set("Content-Type", ContentType)
		w.WriteHeader(http.StatusOK)
		if h.quota != nil {
			if qerr := h.quota.Add(ctx, tenant, oid, vreq.Size); qerr != nil {
				// Counter write failed — log but do NOT fail the
				// verify response. The client already uploaded; we
				// don't want them to retry. Reconcile is the safety
				// net (see §6.5 of the design spec).
				h.logger.Warn("lfs.quota.add_failed",
					"subsystem", "lfs_quota",
					"tenant", tenant,
					"oid", oid,
					"bytes", vreq.Size,
					"err", qerr.Error(),
				)
			} else {
				// Refresh the gauge for this tenant with the new value.
				if st, gerr := h.quota.Get(ctx, tenant); gerr == nil && st.Exists {
					EmitQuotaBytesUsedMetric(ctx, h.logger, tenant, st.UsedBytes)
				}
			}
		}
		emitVerifyRequestMetric(ctx, h.logger, "ok")
		emitLFSVerify(ctx, h.logger, repoFQN, "", oid, vreq.Size, "ok")
		// M15 webhook: lfs.upload after a successful verify (object is
		// durably accepted into storage). Fail-open — enqueue errors do
		// not affect the verify response. Token-authenticated path has
		// no actor in context; emit with empty actor (operators correlate
		// via tenant/repo/oid in the payload).
		if h.webhooks != nil {
			payload := webhooks.LFSUploadPayload{
				OID:       oid,
				SizeBytes: vreq.Size,
			}
			if werr := h.webhooks.Enqueue(ctx, webhooks.EventLFSUpload,
				tenant, repo, "", payload); werr != nil {
				webhooks.EmitEnqueueFailed(ctx, h.logger,
					tenant, repo, "lfs.upload", werr.Error())
			}
		}
		// Usage metering: the object is now durably accepted; meter the
		// verified object size as an lfs_upload. Token-authenticated path
		// has no actor in context, so the actor is "anonymous". Nil-safe.
		if h.usage != nil {
			h.usage.Usage(shiplog.UsageEvent{
				Kind:       shiplog.KindLFSUpload,
				Tenant:     tenant,
				Repo:       repo,
				Actor:      "anonymous",
				Transport:  "https",
				Bytes:      vreq.Size,
				DurationMS: h.now().Sub(verifyStart).Milliseconds(),
				Status:     "ok",
			})
		}
	case errors.Is(err, ErrVerifyNotFound):
		WriteError(w, http.StatusNotFound, "object not uploaded")
		emitVerifyRequestMetric(ctx, h.logger, "missing")
		emitLFSVerify(ctx, h.logger, repoFQN, "", oid, vreq.Size, "missing")
	case errors.Is(err, ErrVerifySizeMismatch):
		WriteError(w, http.StatusUnprocessableEntity, err.Error())
		emitVerifyRequestMetric(ctx, h.logger, "size_mismatch")
		emitLFSVerify(ctx, h.logger, repoFQN, "", oid, vreq.Size, "size_mismatch")
	default:
		WriteError(w, http.StatusInternalServerError, "internal error")
		emitVerifyRequestMetric(ctx, h.logger, "error")
		emitLFSVerify(ctx, h.logger, repoFQN, "", oid, vreq.Size, "error")
	}
}

// countingReader wraps an io.Reader and counts bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// countingWriter wraps an http.ResponseWriter and counts bytes written through it.
type countingWriter struct {
	http.ResponseWriter
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n += int64(n)
	return n, err
}
