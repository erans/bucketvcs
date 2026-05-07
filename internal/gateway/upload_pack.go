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
	body := http.MaxBytesReader(w, r.Body, s.opts.MaxBodyBytes)

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

	// Reject shallow/deepen arguments until M3+ adds proper shallow-info
	// handling. We currently have no shallow-boundary plumbing (no
	// shallow-info section in the response, no shallow file written for
	// pack-objects), and pack-objects' Depth knob is informational-only
	// per the gitcli contract. Rather than silently serve a full pack to
	// a shallow request — which would corrupt the client's view of
	// history — we 400 with a precise reason. A future task will
	// implement the shallow path end-to-end and (only then) honor the
	// fetch=shallow capability we advertise.
	if req.Depth > 0 || req.DeepenSince != "" || len(req.DeepenNot) > 0 || req.DeepenRelative || len(req.Shallow) > 0 {
		http.Error(w, "fetch: shallow/deepen arguments not yet supported by gateway", http.StatusBadRequest)
		return
	}

	pw := pktline.NewWriter(w)

	// Acknowledgments section — emitted whenever the client sent haves.
	// Per protocol-v2 §fetch the section is REQUIRED in that case (with
	// "NAK" when nothing is common); omitting it makes follow-on framing
	// ambiguous. We split the haves into commons (we have them) and
	// unknowns (we don't), and feed both into WriteAcknowledgments so
	// it can NAK when commons is empty.
	//
	// We track ackEmitted so the delim before "packfile\n" is only
	// written when the acknowledgments section actually ran.
	var (
		commons    []string
		unknown    []string
		ackEmitted bool
	)
	if len(req.Haves) > 0 {
		for _, h := range req.Haves {
			if _, err := gitcli.RevParseObjectKind(r.Context(), m.BareDir(), h); err == nil {
				commons = append(commons, h)
			} else {
				unknown = append(unknown, h)
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
		// the side-band as the response body has begun. Otherwise we can
		// emit a clean HTTP error.
		if ackEmitted {
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
		if tok.Type == pktline.Data {
			cp := append([]byte{}, tok.Payload...)
			out = append(out, pktline.Token{Type: tok.Type, Payload: cp})
		} else {
			out = append(out, tok)
		}
	}
}
