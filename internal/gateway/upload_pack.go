package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/pktline"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/repoerrs"
	"github.com/bucketvcs/bucketvcs/internal/v2proto"
)

// uploadPackBodyLimit caps the upload-pack POST request body. A real fetch
// command body is dominated by want/have OID lines (~50 bytes each) plus a
// handful of capability lines. With our maxWants=4096 + maxHaves=8192 caps
// the absolute worst-case legitimate body is well under 1 MiB; 4 MiB gives
// plenty of headroom for client-side capability lines and future growth
// without letting a client exhaust gateway memory through unbounded
// pkt-line padding (capability lines, duplicated args, etc.) before we
// even reach the per-OID caps in handleFetch.
const uploadPackBodyLimit = 4 << 20 // 4 MiB

// maxPktLineTokens caps the number of pkt-line tokens drainPktLine will
// accumulate from one request. Even a maximal fetch body has only a few
// thousand tokens (one per want/have/capability line plus a handful of
// markers); 32k is above any legitimate request and below the point where
// the slice itself becomes a memory pressure source.
const maxPktLineTokens = 32 * 1024

// handleUploadPack serves POST /<tenant>/<repo>.git/git-upload-pack for
// protocol v2 clients. The handler requires Git-Protocol: version=2 (v0
// upload-pack POST is not supported in M3 — v0 fallback is read-only via
// info/refs only). It dispatches the first pkt-line "command=" line to either
// ls-refs or fetch.
func (s *Server) handleUploadPack(w http.ResponseWriter, r *http.Request, tenant, repoID string) {
	if !wantsV2(r.Header.Get("Git-Protocol")) {
		http.Error(w, "protocol v2 required (Git-Protocol: version=2)", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()
	// Use the SMALLER of (a) the operator-configured global cap and
	// (b) the upload-pack-specific cap. The global cap is sized for
	// receive-pack push uploads (which carry whole packfiles); upload-pack
	// requests are command frames, not bulk data, so a tighter cap closes
	// the pre-cap allocation window the reviewer flagged.
	limit := int64(uploadPackBodyLimit)
	if s.opts.MaxBodyBytes > 0 && s.opts.MaxBodyBytes < limit {
		limit = s.opts.MaxBodyBytes
	}
	body := http.MaxBytesReader(w, r.Body, limit)

	tokens, err := drainPktLine(body)
	if err != nil {
		http.Error(w, "bad request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(tokens) == 0 || tokens[0].Type != pktline.Data {
		http.Error(w, "empty command", http.StatusBadRequest)
		return
	}
	cmdLine := strings.TrimRight(string(tokens[0].Payload), "\n")

	r2, err := repo.Open(r.Context(), s.store, tenant, repoID)
	if err != nil {
		if errors.Is(err, repoerrs.ErrRepoNotFound) {
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
		if errors.Is(err, repoerrs.ErrInvalidTenantID) || errors.Is(err, repoerrs.ErrInvalidRepoID) {
			http.Error(w, "invalid tenant or repository name", http.StatusBadRequest)
			return
		}
		http.Error(w, "internal storage error", http.StatusInternalServerError)
		return
	}
	view, err := r2.ReadRoot(r.Context())
	if err != nil {
		http.Error(w, "internal storage error", http.StatusInternalServerError)
		return
	}
	var mbody manifest.Body
	if err := json.Unmarshal(view.Body, &mbody); err != nil {
		http.Error(w, "manifest decode error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")

	switch cmdLine {
	case "command=ls-refs":
		if err := v2proto.HandleLsRefs(tokens, &mbody, w); err != nil {
			http.Error(w, "ls-refs: "+err.Error(), http.StatusBadRequest)
		}
	case "command=fetch":
		s.handleFetch(w, r, tenant, repoID, tokens)
	default:
		http.Error(w, "unsupported command "+cmdLine, http.StatusBadRequest)
	}
}

// maxWants is the upper bound on want OIDs accepted in a single fetch.
// Real Git clients send one want per remote ref they are interested in;
// even a megarepo rarely exceeds a few thousand. We pick 4096 as a
// generous ceiling that still bounds CPU (per-want kind probe), argv
// size (rev-list <wants> --not --all), and memory.
const maxWants = 4096

// maxHaves is the upper bound on have OIDs accepted in a single fetch.
// Clients negotiate haves in rounds of 256-512 by default; a single
// request rarely exceeds 4-8K. Bounding both prevents an attacker from
// turning a malformed POST into per-request CPU/argv exhaustion.
const maxHaves = 8192

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

// handleFetch implements the protocol-v2 "fetch" command. The flow:
//
//  1. Parse fetch arguments via v2proto.ParseFetchArgs.
//  2. Open the per-repo mirror (which lazily syncs to the current manifest)
//     and acquire a read lock so concurrent ingest cannot rewrite refs while
//     we pack.
//  3. Validate that every want OID is REACHABLE from the manifest's
//     advertised refs — not merely present in the mirror. Mere existence
//     is insufficient: pack files may retain hidden, deleted, or otherwise
//     unadvertised objects, and a client that knows or guesses such an OID
//     could exfiltrate it. Reachability against `--not --all` enforces an
//     allow-reachable-sha1-in-want policy: the mirror's loose+pack refset
//     is the authoritative manifest snapshot under our RLock.
//  4. Reject shallow/deepen requests — the gateway has no shallow-info
//     plumbing yet, and silently serving a full pack to a depth-bounded
//     fetch would corrupt the client's history view.
//  5. If the client sent haves, emit an "acknowledgments" section listing
//     the ones we have (NAK when none common). With "done" the client
//     expects the packfile in the same response, separated from the acks
//     by a delim-pkt; without "done" we close the round and let it
//     negotiate again.
//  6. Stream the pack via side-band-64k on band 1, then explicitly Close
//     the pack reader so a non-zero pack-objects exit is reported as a
//     side-band fatal BEFORE the trailing flush.
func (s *Server) handleFetch(w http.ResponseWriter, r *http.Request, tenant, repoID string, tokens []pktline.Token) {
	req, err := v2proto.ParseFetchArgs(tokens)
	if err != nil {
		http.Error(w, "fetch: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Bound the per-OID work below: each want triggers a cat-file probe
	// and joins the rev-list argv; each have triggers the same plus a
	// reachability classification. Without these caps, a malformed
	// request can blow up CPU, argv (E2BIG), or memory.
	if len(req.Wants) > maxWants {
		http.Error(w, fmt.Sprintf("fetch: too many wants (%d > %d)", len(req.Wants), maxWants), http.StatusBadRequest)
		return
	}
	if len(req.Haves) > maxHaves {
		http.Error(w, fmt.Sprintf("fetch: too many haves (%d > %d)", len(req.Haves), maxHaves), http.StatusBadRequest)
		return
	}
	// Dedupe — clients sometimes send the same OID twice (e.g. when a
	// have appears in multiple local refs); without dedup we'd run the
	// same probe twice and pass duplicate args to rev-list.
	req.Wants = dedupOIDs(req.Wants)
	req.Haves = dedupOIDs(req.Haves)
	m, err := s.mgr.Open(r.Context(), tenant, repoID)
	if err != nil {
		http.Error(w, "mirror: "+err.Error(), http.StatusInternalServerError)
		return
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
	for _, oid := range req.Wants {
		kind, err := gitcli.RevParseObjectKind(r.Context(), m.BareDir(), oid)
		if err != nil {
			http.Error(w, "fetch: not our ref "+oid, http.StatusBadRequest)
			return
		}
		if kind != "commit" && kind != "tag" {
			http.Error(w, "fetch: want must be a commit or tag, got "+kind+" for "+oid, http.StatusBadRequest)
			return
		}
	}
	unreachable, err := gitcli.RevListNotAll(r.Context(), m.BareDir(), req.Wants)
	if err != nil {
		http.Error(w, "fetch: reachability check failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if len(unreachable) > 0 {
		// Don't echo the unreachable list verbatim; the first OID is
		// enough to diagnose and avoids leaking other hidden OIDs we may
		// have walked into.
		http.Error(w, "fetch: not our ref "+unreachable[0], http.StatusBadRequest)
		return
	}

	// Defensively reject shallow/deepen arguments. The v2 advertisement no
	// longer exposes the "shallow" feature qualifier on "fetch", so a
	// compliant client will not send these — but a misbehaving or
	// downgraded-cache client might, and silently serving a full pack to a
	// depth-bounded fetch would corrupt the client's history view. A future
	// task will add shallow-info plumbing and re-advertise the capability.
	if req.Depth > 0 || req.DeepenSince != "" || len(req.DeepenNot) > 0 || req.DeepenRelative || len(req.Shallow) > 0 {
		http.Error(w, "fetch: shallow/deepen arguments not yet supported by gateway", http.StatusBadRequest)
		return
	}

	pw := pktline.NewWriter(w)

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
	if len(req.Haves) > 0 {
		// First pass: keep only haves that are commit-or-tag AND present
		// in the mirror. Trees, blobs, and missing objects are treated as
		// "unknown" — they never get ACKed and never reach pack-objects.
		var candidates []string
		for _, h := range req.Haves {
			kind, err := gitcli.RevParseObjectKind(r.Context(), m.BareDir(), h)
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
			leaked, err := gitcli.RevListNotAll(r.Context(), m.BareDir(), candidates)
			if err != nil {
				http.Error(w, "fetch: have-reachability check failed: "+err.Error(), http.StatusInternalServerError)
				return
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
		ready := len(commons) > 0 && req.Done
		if err := v2proto.WriteAcknowledgments(w, commons, unknown, ready); err != nil {
			return
		}
		ackEmitted = true
		if !req.Done {
			// Multi-round negotiation — flush and let the client send
			// another round.
			_ = pw.WriteFlush()
			return
		}
	}

	// Open the pack stream BEFORE writing the packfile section header.
	// If pack-objects fails to start (missing binary, bad dir, invalid
	// args), surfacing that as a clean HTTP 500 is only possible while
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
	pack, err := gitcli.PackObjectsForFetch(r.Context(), m.BareDir(), gitcli.PackForFetchOptions{
		Wants:      req.Wants,
		Haves:      commons,
		ThinPack:   req.ThinPack,
		IncludeTag: req.IncludeTag,
		OfsDelta:   req.OfsDelta,
		NoProgress: req.NoProgress,
	})
	if err != nil {
		// If we already wrote acknowledgments, we have to surface this on
		// the side-band — but the side-band lives inside the packfile
		// section, so we must first emit the protocol-required delim and
		// "packfile\n" header to keep framing valid. Otherwise the client
		// sees side-band frames where it expects a section header and
		// reports a malformed response. Pre-ack failures take the clean
		// HTTP error path because the body is still empty.
		if ackEmitted {
			_ = pw.WriteDelim()
			_ = pw.WriteString("packfile\n")
			sb := pktline.NewSidebandWriter(pw)
			_, _ = sb.WriteFatal([]byte("pack-objects: " + err.Error()))
			return
		}
		http.Error(w, "pack-objects: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if ackEmitted {
		// Per protocol-v2 §fetch, when both an acknowledgments section and
		// a packfile section are present, they are separated by a delim-pkt.
		_ = pw.WriteDelim()
	}
	if err := pw.WriteString("packfile\n"); err != nil {
		_ = pack.Close()
		return
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
		return
	}
	if streamErr {
		return
	}
	_ = pw.WriteFlush()
}

// drainPktLine reads all tokens from r until EOF. Each Data token's Payload
// is COPIED because pktline.Reader reuses its internal buffer across reads.
// If r yields more than maxPktLineTokens frames, drainPktLine aborts with
// an error — this bounds slice growth even if the body limit is generous.
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
	}
}
