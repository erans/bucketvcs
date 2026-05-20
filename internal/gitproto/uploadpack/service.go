package uploadpack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/pktline"
	"github.com/bucketvcs/bucketvcs/internal/reachability"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/refstore"
	"github.com/bucketvcs/bucketvcs/internal/repo/repoerrs"
	"github.com/bucketvcs/bucketvcs/internal/v2proto"
)

// ErrV2Required is returned when the engine is asked to Service a non-V2 request.
var ErrV2Required = errors.New("uploadpack: protocol v2 required")

// ErrBadRequest is returned for malformed input (bad pkt-line, empty command,
// unsupported command). The HTTP adapter maps this to 400.
var ErrBadRequest = errors.New("uploadpack: bad request")

// ErrEOF is returned by Service when the client closed the stream without
// sending a command (clean EOF between commands). The SSH session handler
// uses this to exit its command loop gracefully.
var ErrEOF = errors.New("uploadpack: end of stream")

// maxWants is the upper bound on want OIDs accepted in a single fetch.
// Each want incurs a `git cat-file -t` subprocess for type validation
// plus an entry in the rev-list argv; the cap is sized to bound the
// total subprocess count rather than the OID count alone. A future task
// can lift this once a batched cat-file --batch-check helper lands.
const maxWants = 256

// maxHaves is the upper bound on have OIDs accepted in a single fetch.
// Same per-OID subprocess cost as wants. Real Git clients negotiate
// haves in 256-sized rounds (the libgit2/CGit default), so 256 covers
// the usual round size; a multi-round negotiation re-enters this
// handler for each subsequent round, so 256 per request is sufficient.
const maxHaves = 256

// maxPktLineTokens caps the number of pkt-line tokens drainPktLine will
// accumulate from one request. Even a maximal fetch body has only a few
// thousand tokens (one per want/have/capability line plus a handful of
// markers); 32k is above any legitimate request and below the point where
// the slice itself becomes a memory pressure source.
const maxPktLineTokens = 32 * 1024

// serviceImpl runs the V2 negotiation/fetch protocol: reads pkt-line tokens
// from req.Stdin, dispatches on the leading command line, writes response
// bytes to req.Stdout.
//
// Required: req.ProtocolVersion == 2. Anything else returns ErrV2Required.
// The body byte cap is the responsibility of the caller — pass an
// io.LimitedReader as req.Stdin if you need a hard cap (the HTTP adapter
// uses http.MaxBytesReader).
//
// For SSH transport, Service may be called in a loop: it returns ErrEOF when
// the stream is cleanly exhausted between commands (i.e., the client closed
// the channel without sending another command), which signals the loop to stop.
func serviceImpl(req *EngineRequest) error {
	if req.ProtocolVersion != 2 {
		return ErrV2Required
	}

	tokens, err := drainPktLine(req.Stdin)
	if err != nil {
		return errBadRequestf("bad request body: %v", err)
	}
	// Clean end-of-stream: either no tokens (EOF) or a bare Flush with no
	// leading command (which git sends when closing the SSH channel after
	// ls-refs/fetch — see drainPktLine comments on Flush-termination).
	if len(tokens) == 0 {
		return ErrEOF
	}
	if tokens[0].Type == pktline.Flush {
		// A leading Flush with no command means end-of-stream on SSH.
		return ErrEOF
	}
	// Locate the command= pkt-line. Protocol-v2 nominally puts it first, but
	// git's bundle-uri client (observed in git 2.54) sends capability lines
	// like "agent=" and "object-format=" BEFORE the "command=bundle-uri"
	// line. findCommandLine scans data tokens up to the first delim/flush
	// (or first non-Data token) so the "command=" line can be at any
	// position before the delimiter — one error path covers both
	// "no command= line at all" and "leading non-Data token" variants.
	cmdLine, ok := findCommandLine(tokens)
	if !ok {
		return errBadRequestf("missing command= pkt-line")
	}

	r, err := repo.Open(req.Ctx, req.Store, req.Tenant, req.Repo)
	if err != nil {
		if errors.Is(err, repoerrs.ErrRepoNotFound) {
			return ErrRepoNotFound
		}
		if errors.Is(err, repoerrs.ErrInvalidTenantID) || errors.Is(err, repoerrs.ErrInvalidRepoID) {
			return ErrInvalidName
		}
		return err
	}
	view, err := r.ReadRoot(req.Ctx)
	if err != nil {
		return err
	}
	body, err := manifest.UnmarshalBody(view.Body)
	if err != nil {
		return err
	}

	switch cmdLine {
	case "command=ls-refs":
		k, err := keys.NewRepo(req.Tenant, req.Repo)
		if err != nil {
			// keys.NewRepo only errors on invalid names; tenant/repo were already
			// validated by repo.Open above, so this is a defensive fallback.
			return errBadRequestf("ls-refs: keys: %v", err)
		}
		if err := v2proto.HandleLsRefsWithStore(req.Ctx, tokens, &body, req.Store, k, req.Stdout); err != nil {
			return errBadRequestf("ls-refs: %v", err)
		}
		return nil
	case "command=fetch":
		return serveFetch(req, tokens, &body)
	case "command=bundle-uri":
		return serveBundleURI(req, &body)
	default:
		return errBadRequestf("unsupported command %s", cmdLine)
	}
}

// errBadRequestf wraps the message in ErrBadRequest so callers can errors.Is
// to detect "client error" vs "server error".
func errBadRequestf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrBadRequest}, args...)...)
}

// findCommandLine scans tokens for the first Data pkt-line whose payload
// begins with "command=" and returns its trimmed payload. It stops at the
// first non-Data token (Delim or Flush), so the scan only considers the
// initial run of Data frames that protocol-v2 uses for command + capability
// advertisement.
//
// Returns (cmdLine, true) on match. Returns ("", false) when:
//   - tokens is empty
//   - the leading token is non-Data (e.g. a stray Delim/Flush at position 0)
//   - no Data token in the leading run starts with "command="
//
// Protocol-v2 nominally puts "command=" at tokens[0], but git's bundle-uri
// client (observed in git 2.54) emits capability lines like "agent=..." and
// "object-format=sha1" before "command=bundle-uri". This helper accommodates
// either layout without changing the rest of the dispatcher.
func findCommandLine(tokens []pktline.Token) (string, bool) {
	for _, tok := range tokens {
		if tok.Type != pktline.Data {
			return "", false
		}
		s := strings.TrimRight(string(tok.Payload), "\n")
		if strings.HasPrefix(s, "command=") {
			return s, true
		}
	}
	return "", false
}

// serveFetch is a verbatim port of handleFetch from internal/gateway/upload_pack.go.
// HTTP-specific calls (http.Error, http.MaxBytesReader, Header.Set) are
// REMOVED — the engine returns errors instead.
func serveFetch(req *EngineRequest, tokens []pktline.Token, body *manifest.Body) error {
	fetchReq, err := v2proto.ParseFetchArgs(tokens)
	if err != nil {
		return errBadRequestf("fetch: %v", err)
	}
	// Bound the per-OID work below: each want triggers a cat-file probe
	// and joins the rev-list argv; each have triggers the same plus a
	// reachability classification. Without these caps, a malformed
	// request can blow up CPU, argv (E2BIG), or memory.
	if len(fetchReq.Wants) > maxWants {
		return errBadRequestf("fetch: too many wants (%d > %d)", len(fetchReq.Wants), maxWants)
	}
	if len(fetchReq.Haves) > maxHaves {
		return errBadRequestf("fetch: too many haves (%d > %d)", len(fetchReq.Haves), maxHaves)
	}
	// Dedupe — clients sometimes send the same OID twice (e.g. when a
	// have appears in multiple local refs); without dedup we'd run the
	// same probe twice and pass duplicate args to rev-list.
	fetchReq.Wants = dedupOIDs(fetchReq.Wants)
	fetchReq.Haves = dedupOIDs(fetchReq.Haves)

	// Lazy-negotiation fast path: if the reachability index is available,
	// compute the shipping plan before materialising the mirror. When the
	// plan is empty (client is already up-to-date), we can respond with a
	// no-op ACK and skip the mirror entirely — avoiding a full export that
	// costs hundreds of ms for large repos.
	//
	// On any reachability error (ErrNoIndex, decode failure, etc.)
	// we fall through to the normal mirror-first path and log a structured
	// warning so operators can monitor the fallback rate.
	if done, err := serveFetchLazyPath(req, fetchReq, body); done {
		return err
	}

	m, err := req.Mirror.Open(req.Ctx, req.Tenant, req.Repo)
	if err != nil {
		return fmt.Errorf("mirror: %w", err)
	}
	m.RLock()
	defer m.RUnlock()

	// Validate every want is reachable from an advertised ref. We require
	// commit-or-tag wants (allowAnySHA1InWant is NOT enabled): a tree or
	// blob OID smuggled as a want would be packed by pack-objects --revs
	// even though git rev-list rejects it as a starting point — meaning we
	// must reject those upfront. Then rev-list <wants> --not --all over the
	// mirror tells us which (if any) reach objects outside the advertised
	// refset; empty means every want is fully covered.
	for _, oid := range fetchReq.Wants {
		kind, err := gitcli.RevParseObjectKind(req.Ctx, m.BareDir(), oid)
		if err != nil {
			return errBadRequestf("fetch: not our ref %s", oid)
		}
		if kind != "commit" && kind != "tag" {
			return errBadRequestf("fetch: want must be a commit or tag, got %s for %s", kind, oid)
		}
	}
	unreachable, err := gitcli.RevListNotAll(req.Ctx, m.BareDir(), fetchReq.Wants)
	if err != nil {
		return fmt.Errorf("fetch: reachability check failed: %w", err)
	}
	if len(unreachable) > 0 {
		// Don't echo the unreachable list verbatim; the first OID is
		// enough to diagnose and avoids leaking other hidden OIDs we may
		// have walked into.
		return errBadRequestf("fetch: not our ref %s", unreachable[0])
	}

	// Defensively reject shallow/deepen arguments. The v2 advertisement no
	// longer exposes the "shallow" feature qualifier on "fetch", so a
	// compliant client will not send these — but a misbehaving or
	// downgraded-cache client might, and silently serving a full pack to a
	// depth-bounded fetch would corrupt the client's history view. A future
	// task will add shallow-info plumbing and re-advertise the capability.
	if fetchReq.Depth > 0 || fetchReq.DeepenSince != "" || len(fetchReq.DeepenNot) > 0 || fetchReq.DeepenRelative || len(fetchReq.Shallow) > 0 {
		return errBadRequestf("fetch: shallow/deepen arguments not yet supported by gateway")
	}

	pw := pktline.NewWriter(req.Stdout)

	// Acknowledgments section — emitted whenever the client sent haves.
	// Per protocol-v2 §fetch the section is REQUIRED in that case (with
	// "NAK" when nothing is common); omitting it makes follow-on framing
	// ambiguous. We split the haves into commons (we have them AND they
	// are reachable from advertised refs) and unknowns (everything else),
	// and feed both into WriteAcknowledgments so it can NAK when commons
	// is empty.
	//
	// Reachability matters for haves the same way it matters for wants:
	// if we ACK an OID merely because the object exists in the mirror's
	// pack files (e.g. a deleted-but-not-GC'd commit), we leak the
	// existence of hidden objects to a probing client. We also must NOT
	// forward such hidden OIDs to pack-objects as ^<oid> exclusions —
	// they would either falsely trim history (if reachable from a want
	// via the hidden subgraph) or invalidly reference unreachable revs.
	//
	// We track ackEmitted so the delim before "packfile\n" is only
	// written when the acknowledgments section actually ran.
	var (
		commons    []string
		unknown    []string
		ackEmitted bool
	)
	if len(fetchReq.Haves) > 0 {
		// First pass: keep only haves that are commit-or-tag AND present
		// in the mirror. Trees, blobs, and missing objects are treated as
		// "unknown" — they never get ACKed and never reach pack-objects.
		var candidates []string
		for _, h := range fetchReq.Haves {
			kind, err := gitcli.RevParseObjectKind(req.Ctx, m.BareDir(), h)
			if err != nil || (kind != "commit" && kind != "tag") {
				unknown = append(unknown, h)
				continue
			}
			candidates = append(candidates, h)
		}
		// Second pass: confirm reachability from the advertised refset.
		// rev-list <candidates> --not --all returns the candidates'
		// objects that are reachable but NOT covered by --all; any
		// candidate that appears in that output is unreachable from any
		// advertised ref and must be treated as unknown.
		if len(candidates) > 0 {
			leaked, err := gitcli.RevListNotAll(req.Ctx, m.BareDir(), candidates)
			if err != nil {
				return fmt.Errorf("fetch: have-reachability check failed: %w", err)
			}
			leakedSet := make(map[string]struct{}, len(leaked))
			for _, oid := range leaked {
				leakedSet[oid] = struct{}{}
			}
			for _, c := range candidates {
				if _, hidden := leakedSet[c]; hidden {
					unknown = append(unknown, c)
					continue
				}
				commons = append(commons, c)
			}
		}
		// "ready" tells the client we have enough to proceed to the packfile
		// section in this response. We can only set it when at least one
		// have is common AND the client signaled "done"; otherwise the
		// client would interpret "ready" as our intent to send a packfile
		// in this round, but it still expects another negotiation round.
		ready := len(commons) > 0 && fetchReq.Done
		if err := v2proto.WriteAcknowledgments(req.Stdout, commons, unknown, ready); err != nil {
			return nil
		}
		ackEmitted = true
		if !fetchReq.Done {
			// Multi-round negotiation — flush and let the client send
			// another round.
			_ = pw.WriteFlush()
			return nil
		}
	}

	// Evaluate the packfile-uris advertise gate (M11 Phase 8). When the
	// client opted in (sent at least one packfile-uris=<proto> line), the
	// request shape matches FullPackRequested (single canonical pack
	// covers every want with no haves), and BuildURL succeeds, we emit
	// a packfile-uris response section between the acknowledgments and
	// the packfile section, AND exclude the URI-covered objects from the
	// inline pack via `--keep-pack=pack-<PackID>.pack` (M11 Phase 10).
	// Per Git protocol-v2, a packfile section MUST follow when
	// packfile-uris is present; the inline pack becomes empty/near-empty
	// once its objects are elided. The empty-inline-pack path is
	// necessary because a non-empty inline pack alongside the URI pack
	// trips a known git fetch-pack bug (b664e9ffa1); see the
	// excludeFromInlinePackID comment below. All gate inputs are pure
	// functions of fetchReq + body; no storage I/O happens until
	// BuildURL is called.
	var packURIStanza string
	// excludeFromInlinePackID is the PackID of the pack the URI advertise
	// points at, used to tell `git pack-objects` to skip those objects via
	// `--keep-pack=pack-<PackID>.pack`. Empty when no URI is advertised.
	// Per Phase 8 spec: protocol-v2 requires a packfile section even when
	// packfile-uris is sent, but the inline pack MUST NOT contain objects
	// already covered by the advertised URI — otherwise the client's
	// http-fetch -> index-pack pipeline observes a "no new objects" pack
	// and fetch-pack errors with "expected keep then TAB at start of
	// http-fetch output" (a known git fetch-pack bug; see git's
	// b664e9ffa1). Stock GitHub/Gitlab elide URI-pack objects exactly
	// this way.
	var excludeFromInlinePackID string
	if req.PackURIEnabled && req.PackURIBuildURL != nil && len(fetchReq.PackfileURIs) > 0 && len(fetchReq.Haves) == 0 {
		// tenant/repo were validated by repo.Open at serviceImpl entry,
		// so keys.NewRepo cannot fail here; drop the error.
		puriKey, _ := keys.NewRepo(req.Tenant, req.Repo)
		puriRS, puriErr := refstore.New(req.Ctx, req.Store, puriKey, body)
		if puriErr != nil {
			return fmt.Errorf("fetch: refstore for packfile-uri gate: %w", puriErr)
		}
		puriRefs, puriErr := puriRS.List(req.Ctx)
		if puriErr != nil {
			return fmt.Errorf("fetch: list refs for packfile-uri gate: %w", puriErr)
		}
		refTips := make([]string, 0, len(puriRefs))
		for _, oid := range puriRefs {
			refTips = append(refTips, oid)
		}
		full := v2proto.EvaluateFullPackRequested(v2proto.FullPackRequestedInputs{
			Wants:   fetchReq.Wants,
			Haves:   fetchReq.Haves,
			Packs:   body.Packs,
			RefTips: refTips,
		})
		if full {
			// EvaluateFullPackRequested already ensured len(body.Packs)==1 && PackChecksum!="".
			pe := body.Packs[0]
			res, gerr := v2proto.EvaluatePackURIAdvertise(req.Ctx, v2proto.PackURIInputs{
				ClientOptedIn:     true,
				FullPackRequested: true,
				PackChecksum:      pe.PackChecksum,
				PackKey:           pe.PackKey,
				PackID:            pe.PackID,
				BuildURL:          req.PackURIBuildURL,
			})
			if gerr == nil && res.Stanza != "" {
				packURIStanza = res.Stanza
				// Pack files in the bare mirror are named
				// `pack-<PackID>.pack` (see exporter.downloadAndIndexPack).
				// Pass that basename to pack-objects --keep-pack so the
				// inline pack excludes its objects. A non-empty
				// res.Stanza implies EvaluatePackURIAdvertise has
				// already validated pe.PackID is 40 lowercase hex
				// (matching gitcli.validPackBasename's shape), so
				// advertise emission and keep-pack elision cannot
				// desynchronize mid-response.
				excludeFromInlinePackID = pe.PackID
				emitMetric(req.Ctx, req.Logger, "pack_uri_advertised_total", 1,
					"repo_id", req.Tenant+"/"+req.Repo,
					"via", classifyVia(res.URL),
				)
			}
		}
	}

	// Open the pack stream BEFORE writing the packfile section header.
	// If pack-objects fails to start (missing binary, bad dir, invalid
	// args), surfacing that as a clean error is only possible while
	// the response body is still empty (i.e. before the acknowledgments
	// section was emitted in the no-haves path, or — when haves were
	// present and we already wrote the acks section — at least before the
	// packfile header). Stream-time errors after start are reported via
	// side-band fatal below.
	//
	// Only commons are forwarded to pack-objects as ^<oid> exclusions;
	// unknown haves would become invalid negative revisions and cause
	// `git pack-objects --revs` to abort, breaking otherwise valid fetches
	// from clients that send haves from unrelated local refs.
	pack, err := gitcli.PackObjectsForFetch(req.Ctx, m.BareDir(), gitcli.PackForFetchOptions{
		Wants:      fetchReq.Wants,
		Haves:      commons,
		ThinPack:   fetchReq.ThinPack,
		IncludeTag: fetchReq.IncludeTag,
		OfsDelta:   fetchReq.OfsDelta,
		NoProgress: fetchReq.NoProgress,
		KeepPacks:  keepPackBasenames(excludeFromInlinePackID),
	})
	if err != nil {
		// If we already wrote acknowledgments, we have to surface this on
		// the side-band — but the side-band lives inside the packfile
		// section, so we must first emit the protocol-required delim and
		// "packfile\n" header to keep framing valid. Otherwise the client
		// sees side-band frames where it expects a section header and
		// reports a malformed response. Pre-ack failures take the clean
		// error return path because the body is still empty.
		if ackEmitted {
			_ = pw.WriteDelim()
			// Skip the packfile-uris section on pack-objects start failure
			// — we have nothing meaningful to advertise and the side-band
			// fatal carries the error to the client.
			_ = pw.WriteString("packfile\n")
			sb := pktline.NewSidebandWriter(pw)
			_, _ = sb.WriteFatal([]byte("pack-objects: " + err.Error()))
			return nil
		}
		return fmt.Errorf("pack-objects: %w", err)
	}

	if ackEmitted {
		// Per protocol-v2 §fetch, when both an acknowledgments section and
		// a packfile section are present, they are separated by a delim-pkt.
		_ = pw.WriteDelim()
	}
	if packURIStanza != "" {
		// Emit the packfile-uris section. Per Git protocol-v2 packfile-uris,
		// the section header is "packfile-uris\n" followed by one
		// "<sha1> <uri>\n" line per advertised pack, terminated by a
		// delim-pkt before the next ("packfile") section. The inline pack
		// still flows in the packfile section that follows for protocol
		// framing, but it excludes objects already covered by the URI
		// pack via `--keep-pack=pack-<PackID>.pack` (forwarded through
		// PackForFetchOptions.KeepPacks above). Without this elision a
		// single-canonical-pack clone delivers the same objects on both
		// surfaces and the client's http-fetch -> index-pack pipeline
		// errors with "expected keep then TAB at start of http-fetch
		// output" (a known git fetch-pack bug; see git's b664e9ffa1).
		if err := pw.WriteString("packfile-uris\n"); err != nil {
			_ = pack.Close()
			return nil
		}
		if err := pw.WriteString(packURIStanza); err != nil {
			_ = pack.Close()
			return nil
		}
		if err := pw.WriteDelim(); err != nil {
			_ = pack.Close()
			return nil
		}
	}
	if err := pw.WriteString("packfile\n"); err != nil {
		_ = pack.Close()
		return nil
	}

	sb := pktline.NewSidebandWriter(pw)
	buf := make([]byte, 65000)
	streamErr := false
	for {
		n, rerr := pack.Read(buf)
		if n > 0 {
			if _, werr := sb.WriteData(buf[:n]); werr != nil {
				streamErr = true
				break
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			_, _ = sb.WriteFatal([]byte("pack stream: " + rerr.Error()))
			streamErr = true
			break
		}
	}
	// Explicitly close the pack reader so a non-zero pack-objects exit code
	// is observed BEFORE we send the trailing flush. A defer'd Close would
	// run after we've already promised the client a clean response.
	if cerr := pack.Close(); cerr != nil && !streamErr {
		_, _ = sb.WriteFatal([]byte("pack-objects: " + cerr.Error()))
		return nil
	}
	if streamErr {
		return nil
	}
	_ = pw.WriteFlush()
	return nil
}

// serveBundleURI handles command=bundle-uri. The dispatch arm is always
// present so that even when BundleURIEnabled=false, a misbehaving client
// that sends the command gets a well-formed empty response rather than a
// 400 bad-request that might abort the session. When disabled, or when
// BundleURIBuildURL is nil, or when no warm bundle exists in the manifest,
// HandleBundleURI writes an empty response (flush-pkt only) and the client
// falls through to a standard fetch.
func serveBundleURI(req *EngineRequest, body *manifest.Body) error {
	outcome, encodeErr := doServeBundleURI(req, body)
	emitBundleURIObservability(req, outcome)
	return encodeErr
}

// doServeBundleURI contains the existing serveBundleURI logic and returns a
// BundleURIOutcome so emitBundleURIObservability can emit metrics + audit
// without re-running the freshness state machine.
//
// Reason-vocabulary sync: the short-circuits below set the same Reason strings
// (no_bundle, no_ref) that v2proto.HandleBundleURI's equivalent branches set
// at internal/v2proto/bundleuri.go:HandleBundleURI. They're duplicated here as
// an optimization (avoids the reachability.Load storage read for known-empty
// cases). If either set diverges, the gateway's freshness label will desync
// from a direct HandleBundleURI caller's. Keep them in sync.
func doServeBundleURI(req *EngineRequest, body *manifest.Body) (v2proto.BundleURIOutcome, error) {
	// Short-circuit when the feature is disabled or unconfigured. The
	// dispatch arm is always reachable (a misbehaving client can send
	// command=bundle-uri even when the cap wasn't advertised) so we
	// answer with a well-formed empty response rather than 400-ing,
	// AND we skip the reachability.Load storage read that would
	// otherwise amplify bogus requests into backend traffic.
	if !req.BundleURIEnabled || req.BundleURIBuildURL == nil {
		return v2proto.BundleURIOutcome{State: v2proto.FreshnessRetired, Reason: "disabled"},
			v2proto.EncodeBundleURIResponse(req.Stdout, nil)
	}

	// Also short-circuit when the manifest has no full_default bundle to
	// advertise — saves the reachability.Load roundtrip in the common
	// "repo has no bundle yet" case.
	var entry *manifest.BundleEntry
	for i := range body.Bundles {
		if body.Bundles[i].Kind == "full_default" {
			entry = &body.Bundles[i]
			break
		}
	}
	if entry == nil {
		return v2proto.BundleURIOutcome{State: v2proto.FreshnessRetired, Reason: "no_bundle"},
			v2proto.EncodeBundleURIResponse(req.Stdout, nil)
	}

	// Look up the current tip once. If the ref is missing (deleted) or
	// empty, HandleBundleURI will return empty too — but bailing here
	// avoids the reachability.Load storage read for a known-dead ref.
	// tenant/repo were validated by repo.Open at serviceImpl entry, so
	// keys.NewRepo cannot fail here; drop the error.
	bundleKey, _ := keys.NewRepo(req.Tenant, req.Repo)
	bundleRS, bundleRSErr := refstore.New(req.Ctx, req.Store, bundleKey, body)
	if bundleRSErr != nil {
		// Distinguish a refstore initialisation failure (e.g. a shard object
		// missing from the bucket) from the ordinary "ref doesn't exist" case.
		// Using "no_ref" for both silently suppresses bundle advertisement and
		// produces misleading metric/audit labels for real backend errors.
		slog.WarnContext(req.Ctx, "upload-pack: bundle-uri refstore.New failed",
			slog.String("ref", entry.Ref),
			slog.String("err", bundleRSErr.Error()))
		return v2proto.BundleURIOutcome{State: v2proto.FreshnessRetired, Reason: "refstore_error"},
			v2proto.EncodeBundleURIResponse(req.Stdout, nil)
	}
	currentTip, refPresent, bundleRSLookupErr := bundleRS.Lookup(req.Ctx, entry.Ref)
	if bundleRSLookupErr != nil {
		// A Lookup I/O error is distinct from "ref not present".
		slog.WarnContext(req.Ctx, "upload-pack: bundle-uri refstore.Lookup failed",
			slog.String("ref", entry.Ref),
			slog.String("err", bundleRSLookupErr.Error()))
		return v2proto.BundleURIOutcome{State: v2proto.FreshnessRetired, Reason: "refstore_error"},
			v2proto.EncodeBundleURIResponse(req.Stdout, nil)
	}
	if !refPresent || currentTip == "" {
		return v2proto.BundleURIOutcome{State: v2proto.FreshnessRetired, Reason: "no_ref"},
			v2proto.EncodeBundleURIResponse(req.Stdout, nil)
	}

	// Hot path: when the bundle's tip already matches the current ref tip,
	// EvaluateFreshness will return "current" without ever consulting
	// IsAncestor/WalkBack. Skip the reachability.Load storage read in
	// that case — every fetch immediately after a maintenance cycle hits
	// this branch. The stubs panic on call so a future refactor that
	// removes EvaluateFreshness's "current" early-return is caught
	// immediately in tests instead of silently downgrading current
	// bundles to stale.
	var isAncestor func(a, d string, max int) bool
	var walkBack func(from, target string, max int) (int, error)
	if currentTip == entry.TipOID {
		isAncestor = func(a, d string, max int) bool {
			panic("uploadpack: hot-path IsAncestor stub called; EvaluateFreshness should have returned current before reaching it")
		}
		walkBack = func(from, target string, max int) (int, error) {
			panic("uploadpack: hot-path WalkBack stub called; EvaluateFreshness should have returned current before reaching it")
		}
	} else {
		// Build reachability closures. When the index is unavailable, use
		// fallback closures that report "not ancestor" so HandleBundleURI
		// omits the advertisement safely.
		k, err := keys.NewRepo(req.Tenant, req.Repo)
		if err != nil {
			// keys.NewRepo only errors on invalid names; tenant/repo were already
			// validated by repo.Open above, so this is a defensive fallback.
			isAncestor = func(a, d string, max int) bool { return false }
			walkBack = func(from, target string, max int) (int, error) { return -1, nil }
		} else {
			set, rerr := reachability.Load(req.Ctx, req.Store, k, *body)
			if rerr == nil {
				isAncestor = set.IsAncestor
				walkBack = set.WalkBackOID
			} else {
				// No index (ErrNoIndex) or decode failure → safe fallback: omit
				// advertisement. The freshness state machine will see IsAncestor
				// returning false and classify the bundle as stale. Surface a
				// warn log for decode/storage failures so operators with a
				// broken .bvrd see the silent degradation; ErrNoIndex is the
				// expected pre-maintenance state and stays silent.
				if !errors.Is(rerr, reachability.ErrNoIndex) {
					slog.WarnContext(req.Ctx, "upload-pack: bundle-uri reachability load failed",
						"tenant", req.Tenant, "repo", req.Repo, "err", rerr)
				}
				isAncestor = func(a, d string, max int) bool { return false }
				walkBack = func(from, target string, max int) (int, error) { return -1, nil }
			}
		}
	}

	// Wrap BuildURL to surface configuration errors (signed-URL backend
	// unavailable, proxied URL mint failure, empty URL with nil error)
	// in operator logs. HandleBundleURI treats both error and "" as
	// soft failures and degrades to empty response; without this log
	// the operator would see "bundle-uri advertised but every response
	// is empty" with no signal pointing at the URL-build root cause.
	logBuildURL := req.BundleURIBuildURL
	wrappedBuildURL := func(ctx context.Context, hash, key, expected string) (string, error) {
		url, err := logBuildURL(ctx, hash, key, expected)
		if err != nil {
			slog.WarnContext(ctx, "upload-pack: bundle-uri BuildURL error",
				"tenant", req.Tenant, "repo", req.Repo, "err", err)
		} else if url == "" {
			slog.WarnContext(ctx, "upload-pack: bundle-uri BuildURL returned empty URL with nil error (misconfigured backend?)",
				"tenant", req.Tenant, "repo", req.Repo)
		}
		return url, err
	}

	deps := v2proto.BundleURIDeps{
		Body:        *body,
		Now:         time.Now(),
		WarmCommits: req.BundleWarmCommits,
		WarmAge:     req.BundleWarmAge,
		IsAncestor:  isAncestor,
		WalkBack:    walkBack,
		BuildURL:    wrappedBuildURL,
	}
	return v2proto.HandleBundleURI(req.Ctx, req.Stdout, deps)
}

// emitBundleURIObservability emits bundle_advertised_total + (when an
// advertisement was emitted) bundle_uri_advertised_total + bundle.uri.advertised
// audit. Fires once per command=bundle-uri dispatch, including the
// encode-error path — the metric counts dispatch attempts, not successful
// response writes. Operators wiring success-rate dashboards should pair this
// with the gateway's request-success / response-error metrics rather than
// treating it as a "succeeded" signal.
//
// Rate-amplification note: bundle_advertised_total fires once per
// command=bundle-uri dispatch, INCLUDING short-circuits where the feature
// is disabled (freshness=disabled) or where a misbehaving client sends the
// command even though the cap wasn't advertised. Operators tracking this
// metric should expect rogue clients can pump it at unbounded rate; alert
// on freshness=current/warm/stale rates instead of the overall total when
// they need rate that reflects "real" advertise traffic.
func emitBundleURIObservability(req *EngineRequest, outcome v2proto.BundleURIOutcome) {
	repoID := req.Tenant + "/" + req.Repo
	freshnessLabel := outcome.Reason
	if freshnessLabel == "" {
		freshnessLabel = outcome.State.String() // defensive fallback
	}
	emitMetric(req.Ctx, req.Logger, "bundle_advertised_total", 1,
		"repo_id", repoID,
		"freshness", freshnessLabel,
	)
	if outcome.URI != "" {
		via := classifyVia(outcome.URI)
		emitMetric(req.Ctx, req.Logger, "bundle_uri_advertised_total", 1,
			"repo_id", repoID,
			"via", via,
		)
		// TODO(M12): when multiple bundle kinds (incremental, delta, rolling_base)
		// are advertised, compute bundleCount from the full set of emitted bundles
		// rather than the hardcoded 1. M11 emits exactly one full_default bundle
		// per advertise, so bundleCount == 1 by construction.
		emitBundleURIAdvertised(req.Ctx, req.Logger, repoID,
			freshnessLabel, via, 1, outcome.FirstTipOID)
	}
}

// dedupOIDs returns oids with duplicates removed in first-seen order.
// Both wants and haves are validated as 40-char hex by ParseFetchArgs,
// so dedup-by-string is safe. We dedupe before any per-OID work to
// bound the cost when a misbehaving client sends the same OID many
// times.
func dedupOIDs(oids []string) []string {
	if len(oids) <= 1 {
		return oids
	}
	seen := make(map[string]struct{}, len(oids))
	out := make([]string, 0, len(oids))
	for _, o := range oids {
		if _, ok := seen[o]; ok {
			continue
		}
		seen[o] = struct{}{}
		out = append(out, o)
	}
	return out
}

// drainPktLine reads all tokens from r until EOF or a Flush token. Each Data
// token's Payload is COPIED because pktline.Reader reuses its internal buffer
// across reads. If r yields more than maxPktLineTokens frames, drainPktLine
// aborts with an error — this bounds slice growth even if the body limit is
// generous.
//
// Stopping at Flush (rather than EOF) is required for SSH transport where the
// channel stays open after the client sends its command: the client writes the
// command pkt-lines followed by a flush-pkt and then blocks waiting for the
// server response. Waiting for EOF would deadlock. For HTTP, this is also
// correct because the request body ends with a flush-pkt followed by EOF, so
// drainPktLine terminates at the flush and the next Read returns EOF anyway.
func drainPktLine(r io.Reader) ([]pktline.Token, error) {
	pr := pktline.NewReader(r)
	var out []pktline.Token
	for {
		tok, err := pr.Read()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("pktline: %w", err)
		}
		if len(out) >= maxPktLineTokens {
			return nil, fmt.Errorf("pktline: too many frames (>%d)", maxPktLineTokens)
		}
		if tok.Type == pktline.Data {
			cp := append([]byte{}, tok.Payload...)
			out = append(out, pktline.Token{Type: tok.Type, Payload: cp})
		} else {
			out = append(out, tok)
		}
		// A Flush token signals end-of-message. Stop reading so the server
		// can process the command and write its response — necessary for SSH
		// where the channel remains open after the flush.
		if tok.Type == pktline.Flush {
			return out, nil
		}
	}
}

// keepPackBasenames returns the pack basenames to pass to
// `git pack-objects --keep-pack=<name>` so the inline pack excludes
// objects already covered by an advertised pack URL. Returns nil when
// packID is empty (no URI advertised) — `nil` rather than an empty
// slice so the caller's PackForFetchOptions.KeepPacks remains zero
// and pack-objects sees no extra args.
//
// The pack file inside the bare mirror is named `pack-<PackID>.pack`
// (see exporter.downloadAndIndexPack), so the basename is derived
// directly from the canonical pack entry's PackID without any storage
// lookup.
func keepPackBasenames(packID string) []string {
	if packID == "" {
		return nil
	}
	return []string{"pack-" + packID + ".pack"}
}

// serveFetchLazyPath attempts pure-Go negotiation via the reachability index
// BEFORE opening the mirror. It returns (true, err) when the request is fully
// handled (either a no-op ACK because the plan is empty, or an error that
// aborted the session). It returns (false, nil) when the caller should fall
// through to the normal mirror-first path.
//
// The two cases that result in falling through:
//  1. The reachability index is unavailable (ErrNoIndex, decode failure,
//     etc.) — logged as a structured warning with a fallback_reason label.
//  2. The client has wants that ARE present in the index and there ARE commits
//     to ship — the caller must materialise the mirror and run pack-objects.
//
// On ErrUnknownWant, falls through to the mirror path (returns false, nil).
// We cannot distinguish "client wants an annotated tag / tree / blob the
// Set doesn't index" from "client wants a missing commit", and the mirror
// has full object knowledge. Falling through is the safe choice.
func serveFetchLazyPath(req *EngineRequest, fetchReq v2proto.FetchRequest, body *manifest.Body) (done bool, _ error) {
	// Build keys.Repo to satisfy reachability.Load's signature. If the
	// tenant/repo names are invalid at the keys layer we skip the lazy path
	// (they will be caught downstream by the existing mirror path or have
	// already been caught by the repo.Open call above in serviceImpl).
	k, err := keys.NewRepo(req.Tenant, req.Repo)
	if err != nil {
		return false, nil
	}

	set, err := reachability.Load(req.Ctx, req.Store, k, *body)
	if err != nil {
		// Log a structured fallback warning so operators can track the
		// fallback rate on dashboards. ClassifyFallback provides a bounded
		// label set (no_index, delta_decode, unknown) for easy alert pivots.
		slog.WarnContext(req.Ctx, "upload-pack: reachability index unavailable; falling back to mirror path",
			"tenant", req.Tenant,
			"repo", req.Repo,
			"fallback_reason", reachability.ClassifyFallback(err),
			"error", err.Error(),
		)
		return false, nil
	}

	// Convert hex-string OIDs from ParseFetchArgs to pack.OID.
	wants := make([]pack.OID, 0, len(fetchReq.Wants))
	for _, h := range fetchReq.Wants {
		o, err := pack.ParseOID(h)
		if err != nil {
			// ParseFetchArgs already validated hex format; this branch is
			// defensive. Log the unexpected input and fall through to mirror.
			slog.ErrorContext(req.Ctx, "upload-pack: lazy path: ParseOID failed for want (should not happen after ParseFetchArgs)",
				"input", h, "error", err.Error())
			return false, nil
		}
		wants = append(wants, o)
	}
	haves := make([]pack.OID, 0, len(fetchReq.Haves))
	for _, h := range fetchReq.Haves {
		o, err := pack.ParseOID(h)
		if err != nil {
			// Same defensive fallback as wants above.
			slog.ErrorContext(req.Ctx, "upload-pack: lazy path: ParseOID failed for have (should not happen after ParseFetchArgs)",
				"input", h, "error", err.Error())
			return false, nil
		}
		haves = append(haves, o)
	}

	plan, err := Negotiate(req.Ctx, set, NegotiateInput{
		Wants: wants,
		Haves: haves,
		Done:  fetchReq.Done,
	})
	if err != nil {
		if errors.Is(err, ErrUnknownWant) {
			// The want OID is not in the reachability index. Two possible causes:
			//   (a) The client asked for something that genuinely doesn't exist.
			//   (b) The OID is a tag, tree, or blob — the commit-graph index
			//       covers commits only, so annotated tags, tree wants, etc.
			//       won't be found here. Fall through to the mirror path which
			//       can validate and serve them correctly.
			// We cannot distinguish (a) from (b) without examining the object
			// type, so the safe choice is always to fall through.
			return false, nil
		}
		// Other negotiate errors (e.g. context cancellation, storage read
		// failures): log at Warn so operators can track them, then fall through
		// to the mirror path.
		slog.WarnContext(req.Ctx, "reachability.fallback",
			"fallback_reason", "negotiate_error",
			"err", err,
			"repo", k.Prefix(),
		)
		return false, nil
	}

	// No commits to ship. Two cases:
	//
	//   done=false (interim negotiation round): the client is still sending
	//   haves and waiting for more acks. A bare "acknowledgments + flush"
	//   response is correct here — it tells the client "here's what we agree
	//   on so far; keep going". No packfile section is expected by the client.
	//
	//   done=true (final round): protocol v2 §fetch requires the server to
	//   terminate with a final-response that includes a packfile section (even
	//   if the pack would be empty). Emitting only "acknowledgments + flush"
	//   violates the grammar and strict clients may hang waiting for the
	//   packfile section. The simplest correct response is to fall through to
	//   the mirror path and let git upload-pack produce the proper
	//   "acknowledgments delim-pkt packfile flush" shape, including an empty
	//   pack when no objects need to be sent.
	//
	// TODO(M11 Phase 8.x): when the request matches FullPackRequested AND the
	// lazy path would otherwise fall through to mirror just to produce an
	// empty pack, we could short-circuit by emitting only the
	// packfile-uris section + an empty packfile section, skipping the
	// gitcli.PackObjectsForFetch invocation entirely. Out of scope for
	// Task 8.2.
	if len(plan.Commits) == 0 {
		if fetchReq.Done {
			// Fall through to mirror — it will emit the proper empty-pack
			// final response that satisfies the protocol v2 grammar.
			return false, nil
		}
		// Interim continuation with haves. Only fall through to the mirror
		// when at least one have is known to the Set — if ALL haves are
		// unknown, the lazy NAK+flush path is the correct response and the
		// mirror would produce the same result, so we stay on the lazy path
		// rather than opening the mirror unnecessarily.
		if len(haves) > 0 {
			var anyKnown bool
			for _, h := range haves {
				if set.Has(h) {
					anyKnown = true
					break
				}
			}
			if anyKnown {
				// At least one known have: the lazy path can ACK it via
				// set.Has(o) even when the commit is not reachable from an
				// advertised ref (e.g. an abandoned force-pushed commit still
				// present in an old .bvrd delta). The eager mirror path
				// filters to ref-reachable commits, so the two paths would
				// disagree on haves disclosure. Punt to mirror for consistency.
				return false, nil
			}
			// All haves unknown to the Set: lazy NAK+flush is correct.
			// Fall through to the NAK+flush emission below.
		}
		// No haves AND no commits to ship: emit an empty acknowledgments
		// section + flush so the client advances its negotiation round.
		// Protocol v2 §fetch allows either an empty acknowledgments section
		// or a bare flush as the interim continuation when there are no known
		// commons to report; WriteAcknowledgments with nil commons and nil
		// unknowns emits only the section header + flush, which clients treat
		// as "continue". This matches what the mirror path would emit.
		if err := v2proto.WriteAcknowledgments(req.Stdout, nil, nil, false); err != nil {
			return true, fmt.Errorf("lazy fetch: write acks: %w", err)
		}
		pw := pktline.NewWriter(req.Stdout)
		if err := pw.WriteFlush(); err != nil {
			return true, fmt.Errorf("lazy fetch: flush: %w", err)
		}
		return true, nil
	}

	// There are commits to ship — fall through to the mirror path which
	// knows how to materialise the pack via git pack-objects.
	return false, nil
}
