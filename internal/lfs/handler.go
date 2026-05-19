package lfs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/gateway/routenames"
)

const maxBatchObjects = 1000

// Deps is the dependency surface NewHTTPHandler needs. It is kept as
// a value-type struct so callers (the gateway today, a future test
// harness) configure exactly what they need without touching package
// internals.
type Deps struct {
	// AuthStore is REQUIRED. It is used for the secondary ActionWrite
	// permission check on upload operations (LookupRepoPerm +
	// GetRepoFlags). It is NOT used for credential verification — that
	// is the caller's job (typically gateway.RunAuth via routeRepo).
	// NewHTTPHandler panics if this is nil.
	AuthStore auth.Store

	// ActorFromContext returns the authed actor attached to the request
	// context by upstream middleware. The gateway path passes
	// gateway.ActorFromContext directly. nil-returned means anonymous.
	// Optional: a nil function means every request is anonymous.
	ActorFromContext func(context.Context) *auth.Actor

	// NewStore is REQUIRED. It returns the per-repo lfs.Store for the
	// given (tenant, repo). The gateway constructs this by combining
	// its top-level storage.ObjectStore with the repo's prefix.
	// NewHTTPHandler panics if this is nil.
	NewStore func(tenant, repo string) *Store

	// PresignTTL is the TTL passed into Store.PresignPut/PresignGet.
	// Optional: zero falls through to the Store's own default.
	PresignTTL time.Duration

	// Logger is used for metric + audit emission. Optional: nil falls
	// back to slog.Default() (same shape as internal/gateway/log.go).
	Logger *slog.Logger
}

// lfsRoute enumerates the LFS sub-routes the handler dispatches.
type lfsRoute int

const (
	lfsRouteNone lfsRoute = iota
	lfsRouteBatch
)

// NewHTTPHandler returns the http.Handler that serves the LFS Batch
// endpoint. The handler trusts upstream middleware (typically
// gateway.RunAuth) for credential verification and repo existence —
// the actor is recovered from context via deps.ActorFromContext. For
// Batch, the handler performs a secondary write check when the body's
// operation is "upload". The handler emits a per-route audit event
// (lfs.batch) plus metrics.
//
// Note: the LFS verify endpoint is NOT served by this handler. As of
// M13.1 verify is owned by the proxied handler (internal/lfs/proxied.go),
// which authenticates verify POSTs via a kind=5 HMAC token minted by
// Store.ProxiedVerifyURL and embedded in the Batch upload-action URL.
// The gateway route dispatcher only forwards OpLFSBatch here.
//
// The handler is intentionally constructed with a closure over deps so
// the gateway can mount it via mux.Handle without further wiring.
//
// Auth model: this handler relies on upstream middleware (typically
// gateway.RunAuth) to have already enforced the route's RequiredAction
// before the request reaches it. For Batch the handler performs an
// additional ActionWrite check on upload operations. If you mount this
// handler outside the gateway path, you MUST run your own auth
// enforcement upstream — the handler does NOT belt-and-suspenders
// re-check read perms.
//
// Panics if deps.AuthStore or deps.NewStore is nil — these are
// programmer errors at wire-up time, not request-time failures.
func NewHTTPHandler(deps Deps) http.Handler {
	if deps.AuthStore == nil {
		panic("lfs.NewHTTPHandler: Deps.AuthStore is required")
	}
	if deps.NewStore == nil {
		panic("lfs.NewHTTPHandler: Deps.NewStore is required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := deps.Logger
		if logger == nil {
			logger = slog.Default()
		}

		tenant, repo, route := parseLFSPath(r.URL.Path)
		if route == lfsRouteNone || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}

		// Actor comes from upstream middleware. RunAuth in the gateway
		// path has already verified credentials and confirmed the
		// route's RequiredAction; the batch path re-Decides for
		// ActionWrite on uploads below.
		var actor *auth.Actor
		if deps.ActorFromContext != nil {
			actor = deps.ActorFromContext(ctx)
		}

		switch route {
		case lfsRouteBatch:
			handleBatch(ctx, w, r, deps, tenant, repo, actor, logger)
		}
	})
}

// handleBatch processes POST /<tenant>/<repo>.git/info/lfs/objects/batch.
// All Batch-specific logic — Content-Type validation, body decode,
// secondary ActionWrite check on upload, response build, audit + metrics —
// lives here. The caller has already verified the route, method, and
// actor.
func handleBatch(ctx context.Context, w http.ResponseWriter, r *http.Request, deps Deps, tenant, repo string, actor *auth.Actor, logger *slog.Logger) {
	// LFS spec requires application/vnd.git-lfs+json on Batch
	// requests. Reject mismatched Content-Types to surface client
	// bugs early. We tolerate an empty Content-Type (some clients
	// don't set it on small POSTs); we only reject mismatches. Use
	// mime.ParseMediaType to robustly strip parameter suffixes like
	// "; charset=utf-8" and validate the bare media type.
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mt, _, perr := mime.ParseMediaType(ct)
		if perr != nil || mt != ContentType {
			WriteError(w, http.StatusUnsupportedMediaType, "expected Content-Type: "+ContentType)
			emitBatchRequestMetric(ctx, logger, "unknown", "error")
			return
		}
	}

	// On a malformed body or unsupported operation we emit only the
	// metric (no audit), because the parsed req.Operation is
	// unreliable and the audit event would carry sentinel data.
	// Per-attempt visibility into these errors is best obtained
	// from the metric counter, not the audit stream.
	body := http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB hard cap on Batch body
	var req BatchRequest
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			WriteError(w, http.StatusRequestEntityTooLarge, "request body too large")
			emitBatchRequestMetric(ctx, logger, "unknown", "too_large")
			return
		}
		WriteError(w, http.StatusUnprocessableEntity, "unprocessable: "+err.Error())
		emitBatchRequestMetric(ctx, logger, "unknown", "error")
		return
	}
	if len(req.Objects) > maxBatchObjects {
		WriteError(w, http.StatusUnprocessableEntity, fmt.Sprintf("too many objects (max %d)", maxBatchObjects))
		emitBatchRequestMetric(ctx, logger, req.Operation, "error")
		return
	}
	if req.Operation != "upload" && req.Operation != "download" {
		WriteError(w, http.StatusUnprocessableEntity, "unsupported operation")
		emitBatchRequestMetric(ctx, logger, req.Operation, "error")
		return
	}

	// Secondary write check for upload operations. RunAuth in the
	// gateway already passed ActionRead; we re-Decide with
	// ActionWrite here.
	if req.Operation == "upload" {
		flags, err := deps.AuthStore.GetRepoFlags(ctx, tenant, repo)
		if errors.Is(err, auth.ErrNoSuchRepo) {
			WriteError(w, http.StatusNotFound, "repository not found")
			emitBatchRequestMetric(ctx, logger, req.Operation, "notfound")
			emitLFSBatch(ctx, logger, tenant+"/"+repo, actorName(actor), req.Operation, 0, "notfound")
			return
		}
		if err != nil {
			WriteError(w, http.StatusInternalServerError, "internal error")
			emitBatchRequestMetric(ctx, logger, req.Operation, "error")
			emitLFSBatch(ctx, logger, tenant+"/"+repo, actorName(actor), req.Operation, 0, "error")
			return
		}
		var perm auth.Perm
		if actor != nil {
			p, perr := deps.AuthStore.LookupRepoPerm(ctx, actor, tenant, repo)
			if errors.Is(perr, auth.ErrNoSuchRepo) {
				WriteError(w, http.StatusNotFound, "repository not found")
				emitBatchRequestMetric(ctx, logger, req.Operation, "notfound")
				emitLFSBatch(ctx, logger, tenant+"/"+repo, actorName(actor), req.Operation, 0, "notfound")
				return
			}
			if perr != nil {
				WriteError(w, http.StatusInternalServerError, "internal error")
				emitBatchRequestMetric(ctx, logger, req.Operation, "error")
				emitLFSBatch(ctx, logger, tenant+"/"+repo, actorName(actor), req.Operation, 0, "error")
				return
			}
			perm = p
		}
		if ok, reason := auth.Decide(actor, perm, auth.ActionWrite, flags); !ok {
			if actor == nil {
				w.Header().Set("WWW-Authenticate", `Basic realm="bucketvcs"`)
				WriteError(w, http.StatusUnauthorized, "unauthorized")
				emitBatchRequestMetric(ctx, logger, req.Operation, "unauthorized")
				emitLFSBatch(ctx, logger, tenant+"/"+repo, actorName(actor), req.Operation, 0, "unauthorized")
			} else {
				WriteError(w, http.StatusForbidden, "forbidden")
				emitBatchRequestMetric(ctx, logger, req.Operation, "forbidden")
				// Log the deny reason for on-call debugging. The
				// audit shape is fixed flat-attrs with a populated
				// "result" field, so reason rides on a separate
				// info-level log line rather than the audit event.
				logger.LogAttrs(ctx, slog.LevelInfo, "lfs.batch.deny",
					slog.String("repo", tenant+"/"+repo),
					slog.String("user", actorName(actor)),
					slog.String("op", req.Operation),
					slog.String("reason", reason),
				)
				emitLFSBatch(ctx, logger, tenant+"/"+repo, actorName(actor), req.Operation, 0, "forbidden")
			}
			return
		}
	}

	// Build the response. The verify action's URL and Authorization
	// header are minted from Store.ProxiedVerifyURL (kind=5 HMAC token);
	// no inbound bearer is echoed into the response.
	store := deps.NewStore(tenant, repo)
	resp, berr := Build(ctx, req, store, deps.PresignTTL)
	if berr != nil {
		WriteError(w, http.StatusUnprocessableEntity, berr.Error())
		emitBatchRequestMetric(ctx, logger, req.Operation, "error")
		return
	}

	// Emit per-object metrics.
	for _, o := range resp.Objects {
		emitBatchObjectMetric(ctx, logger, req.Operation, perObjectResult(req.Operation, o))
	}

	// Write 200.
	w.Header().Set("Content-Type", ContentType)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)

	// Audit + request-level metric.
	emitBatchRequestMetric(ctx, logger, req.Operation, "ok")
	emitLFSBatch(ctx, logger, tenant+"/"+repo, actorName(actor), req.Operation, len(resp.Objects), "ok")
}

// handleVerify lived here in M13 P3 and processed the OLD route
// POST /<tenant>/<repo>.git/info/lfs/objects/<oid>/verify. As of M13.1
// it has been removed: verify is now served by the proxied handler
// (internal/lfs/proxied.go) gated on a kind=5 HMAC token, and the
// Batch response embeds the new proxied verify URL + Authorization
// header. The route-based path is no longer dispatched.

// actorName returns a's Name, or "" if a is nil. Used in audit emit
// calls where we want to pass through the actor identity (or empty
// for anonymous) without a multi-line nil-check at each call site.
func actorName(a *auth.Actor) string {
	if a == nil {
		return ""
	}
	return a.Name
}

// parseLFSPath parses /<tenant>/<repo>.git/info/lfs/objects/batch and
// returns (tenant, repo, lfsRouteBatch) on a match, or zero values
// with lfsRouteNone otherwise. Tenant and repo are validated via
// validRouteName.
func parseLFSPath(p string) (tenant, repo string, route lfsRoute) {
	if p != path.Clean(p) {
		return "", "", lfsRouteNone
	}
	parts := strings.SplitN(strings.TrimPrefix(p, "/"), "/", 3)
	if len(parts) < 3 {
		return "", "", lfsRouteNone
	}
	tenant = parts[0]
	repoSeg := parts[1]
	rest := parts[2]
	if !strings.HasSuffix(repoSeg, ".git") || repoSeg == ".git" {
		return "", "", lfsRouteNone
	}
	repo = strings.TrimSuffix(repoSeg, ".git")
	if tenant == "" || repo == "" {
		return "", "", lfsRouteNone
	}
	if !validRouteName(tenant) || !validRouteName(repo) {
		return "", "", lfsRouteNone
	}
	if rest == "info/lfs/objects/batch" {
		return tenant, repo, lfsRouteBatch
	}
	return "", "", lfsRouteNone
}

// validRouteName mirrors the gateway's path-segment validator so the
// standalone-mount path can apply the same rules upstream auth uses.
// Implemented as a delegation to keep the two validators in lockstep
// without circular imports (lfs is leaf, routenames is leaf, gateway
// depends on both).
func validRouteName(s string) bool {
	return routenames.ValidateName(s)
}

// perObjectResult returns the metric label for one ObjectAction. The
// label vocabulary matches the spec §7: new|exists|missing|error.
//
//   - upload, missing -> "new" (upload action returned).
//   - upload, present at matching size -> "exists" (empty actions).
//   - upload, present at mismatched size -> "error" (per-object 422).
//   - download, present -> "exists" (download action returned).
//   - download, missing -> "missing" (per-object 404).
//   - any other per-object error (presign failure, head error) -> "error".
func perObjectResult(op string, o ObjectAction) string {
	if o.Error != nil {
		if op == "download" && o.Error.Code == 404 {
			return "missing"
		}
		return "error"
	}
	switch op {
	case "upload":
		if len(o.Actions) == 0 {
			return "exists"
		}
		return "new"
	case "download":
		return "exists"
	default:
		// Unreachable today: the handler validates req.Operation
		// before reaching perObjectResult. If a new operation is
		// added (e.g., verify), this case must be updated.
		return "error"
	}
}
