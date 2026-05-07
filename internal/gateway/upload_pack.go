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
//  3. Validate that every want OID exists in the mirror — protecting against
//     clients that guess unadvertised OIDs (the gitcli helper documents that
//     reachability checks are the caller's responsibility).
//  4. If the client sent haves, emit an "acknowledgments" section listing the
//     ones we have. With "done" the client expects the packfile in the same
//     response; without "done" we close the round and let it negotiate again.
//  5. Stream the pack via side-band-64k on band 1.
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

	// Validate every want is reachable in the mirror. We use cat-file -t via
	// RevParseObjectKind rather than show-ref because clients legitimately
	// "want" non-tip commits (e.g. fetching a specific OID by sha). If the
	// object is not present in the mirror we refuse — without this check, a
	// client that guesses an OID could exfiltrate hidden objects.
	for _, oid := range req.Wants {
		if _, err := gitcli.RevParseObjectKind(r.Context(), m.BareDir(), oid); err != nil {
			http.Error(w, "fetch: not our ref "+oid, http.StatusBadRequest)
			return
		}
	}

	pw := pktline.NewWriter(w)

	// Acknowledgments section — only emitted when the client sent haves.
	if len(req.Haves) > 0 {
		var commons []string
		for _, h := range req.Haves {
			if _, err := gitcli.RevParseObjectKind(r.Context(), m.BareDir(), h); err == nil {
				commons = append(commons, h)
			}
		}
		// "ready" tells the client we have enough to proceed to the packfile
		// section in this response. We can only set it when at least one
		// have is common AND the client signaled "done"; otherwise the
		// client would interpret "ready" as our intent to send a packfile
		// in this round, but it still expects another negotiation round.
		ready := len(commons) > 0 && req.Done
		if err := v2proto.WriteAcknowledgments(w, commons, nil, ready); err != nil {
			return
		}
		if !req.Done {
			// Multi-round negotiation — flush and let the client send
			// another round.
			_ = pw.WriteFlush()
			return
		}
		// Per protocol-v2 §fetch, when both an acknowledgments section and
		// a packfile section are present, they are separated by a delim-pkt.
		_ = pw.WriteDelim()
	}

	if err := pw.WriteString("packfile\n"); err != nil {
		return
	}

	pack, err := gitcli.PackObjectsForFetch(r.Context(), m.BareDir(), gitcli.PackForFetchOptions{
		Wants:      req.Wants,
		Haves:      req.Haves,
		ThinPack:   req.ThinPack,
		IncludeTag: req.IncludeTag,
		OfsDelta:   req.OfsDelta,
		NoProgress: req.NoProgress,
		Depth:      req.Depth,
	})
	if err != nil {
		http.Error(w, "pack-objects: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer pack.Close()

	sb := pktline.NewSidebandWriter(pw)
	buf := make([]byte, 65000)
	for {
		n, rerr := pack.Read(buf)
		if n > 0 {
			if _, werr := sb.WriteData(buf[:n]); werr != nil {
				return
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			_, _ = sb.WriteFatal([]byte("pack stream: " + rerr.Error()))
			return
		}
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
