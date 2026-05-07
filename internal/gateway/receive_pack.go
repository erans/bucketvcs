package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/pktline"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/repoerrs"
)

// gatewayNullOID is the 40-zero OID sentinel for create/delete commands.
// We define it locally to avoid an import cycle with internal/mirror.
const gatewayNullOID = "0000000000000000000000000000000000000000"

// maxUpdateCommands caps the number of update commands a single
// receive-pack request can carry. Each command spawns a
// `git check-ref-format` subprocess for refname validation, so an
// uncapped count is a CPU/process DoS even at delete-only sizes well
// below the request body limit. 4096 is well above any realistic push
// (the largest known repos advertise <1k branches/tags) and below the
// point where the slice itself is a memory pressure source.
const maxUpdateCommands = 4096

type updateCommand struct {
	OldOID  string
	NewOID  string
	Refname string
}

type receivePackRequest struct {
	Caps     map[string]bool
	Updates  []updateCommand
	PackPath string // empty for delete-only push
	IsAtomic bool
}

// handleReceivePack implements POST /<tenant>/<repo>.git/git-receive-pack
// for the v0 receive-pack protocol. Task 16's responsibility is parsing
// only: capture the update commands and capability set, stream any inbound
// pack to the mirror's incoming/ staging dir, then emit a placeholder
// "ok everything" report. Task 17 will replace the placeholder with full
// validation + commit logic (pack ingest via index-pack, ref-update CAS,
// connectivity checks, atomic-mode handling).
func (s *Server) handleReceivePack(w http.ResponseWriter, r *http.Request, tenant, repoID string) {
	defer r.Body.Close()
	body := http.MaxBytesReader(w, r.Body, s.opts.MaxBodyBytes)

	// Resolve the repo first so a missing repo returns a clean 404 instead
	// of a mirror-init 500. mirror.Manager.Open also calls repo.Open, but
	// we want to differentiate "repo not found" from "mirror init failed".
	if _, err := repo.Open(r.Context(), s.store, tenant, repoID); err != nil {
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
	m, err := s.mgr.Open(r.Context(), tenant, repoID)
	if err != nil {
		http.Error(w, "mirror: "+err.Error(), http.StatusInternalServerError)
		return
	}

	req, err := parseReceivePackRequest(r.Context(), body, m.IncomingDir())
	if err != nil {
		if req != nil && req.PackPath != "" {
			_ = os.Remove(req.PackPath)
		}
		http.Error(w, "receive-pack: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Acquire the per-repo write lock. Task 17's commit logic will run
	// while this lock is held; for Task 16 there is no mutation, but we
	// still acquire the lock to exercise the same lifecycle and surface
	// any future deadlock at parse time rather than first-commit time.
	m.Lock()
	defer m.Unlock()

	// Task 17 will replace this with full validation + commit. For Task 16
	// we MUST NOT report success — no pack ingest, no ref update, no CAS
	// has actually happened. We emit an honest report-status of "unpack ng
	// not-implemented" + per-ref "ng <ref> not-implemented" so a real Git
	// client surfaces the push as failed. The HTTP layer still returns 200
	// because report-status framing requires a 2xx response; the failure
	// is communicated via the report itself per pack-protocol(5).
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	if err := writeReportNotImplemented(w, req); err != nil {
		// best-effort — the response is already partially written and
		// surfacing an error here would corrupt the report.
		_ = err
	}

	// Always clean up the staged pack at the end of Task 16's path. Task 17
	// will rename/move it (e.g. via git index-pack) BEFORE this defer fires,
	// at which point os.Remove on the original name becomes a no-op.
	if req.PackPath != "" {
		_ = os.Remove(req.PackPath)
		_ = os.Remove(strings.TrimSuffix(req.PackPath, ".pack") + ".idx")
	}
}

// parseReceivePackRequest drains the v0 receive-pack request body. It reads
// pkt-line tokens until flush, accumulating <old> <new> <refname> commands;
// the FIRST command line carries a NUL-suffixed capability list. After the
// flush, if any command is a non-delete (NewOID != gatewayNullOID), the
// remaining body bytes are streamed verbatim into <incoming>/rcv-<rand>.pack.
// On error the returned *receivePackRequest may carry a non-empty PackPath
// the caller must clean up.
func parseReceivePackRequest(ctx context.Context, body io.Reader, incoming string) (*receivePackRequest, error) {
	pr := pktline.NewReader(body)
	req := &receivePackRequest{Caps: map[string]bool{}}
	first := true
	for {
		tok, err := pr.Read()
		if err == io.EOF {
			return nil, errors.New("unexpected EOF before flush")
		}
		if err != nil {
			return nil, fmt.Errorf("read commands: %w", err)
		}
		if tok.Type == pktline.Flush {
			break
		}
		if tok.Type != pktline.Data {
			return nil, fmt.Errorf("unexpected token %v", tok.Type)
		}
		// Copy payload because pktline reuses its internal buffer.
		line := strings.TrimRight(string(append([]byte{}, tok.Payload...)), "\n")
		if first {
			first = false
			if i := strings.IndexByte(line, '\x00'); i >= 0 {
				caps := strings.Fields(line[i+1:])
				for _, c := range caps {
					req.Caps[c] = true
				}
				line = line[:i]
			}
			if req.Caps["atomic"] {
				req.IsAtomic = true
			}
		}
		// "<old> <new> <refname>"
		parts := strings.SplitN(line, " ", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("malformed update command %q", line)
		}
		old, neu, ref := parts[0], parts[1], parts[2]
		if !validHexOID40(old) || !validHexOID40(neu) {
			return nil, fmt.Errorf("invalid OID in command %q", line)
		}
		if err := gitcli.CheckRefFormat(ctx, ref); err != nil {
			return nil, fmt.Errorf("invalid refname %q: %w", ref, err)
		}
		if strings.HasPrefix(ref, "refs/replace/") {
			return nil, fmt.Errorf("refs/replace/* writes are not allowed")
		}
		if neu == gatewayNullOID && old == gatewayNullOID {
			return nil, fmt.Errorf("noop command (both OIDs are zero)")
		}
		req.Updates = append(req.Updates, updateCommand{OldOID: old, NewOID: neu, Refname: ref})
		if len(req.Updates) > maxUpdateCommands {
			return nil, fmt.Errorf("too many update commands (>%d)", maxUpdateCommands)
		}
	}
	if len(req.Updates) == 0 {
		return nil, errors.New("no update commands")
	}

	allDelete := true
	for _, u := range req.Updates {
		if u.NewOID != gatewayNullOID {
			allDelete = false
			break
		}
	}
	if !allDelete {
		// Read remaining body into a temp pack file under <mirror>/incoming/.
		// IncomingDir is mkdir'd at mirror.Manager.Open time; the MkdirAll
		// here is defensive in case the directory was removed out-of-band.
		if err := os.MkdirAll(incoming, 0o755); err != nil {
			return req, fmt.Errorf("incoming mkdir: %w", err)
		}
		idBytes := make([]byte, 12)
		if _, err := rand.Read(idBytes); err != nil {
			return req, fmt.Errorf("incoming name: %w", err)
		}
		packPath := filepath.Join(incoming, "rcv-"+hex.EncodeToString(idBytes)+".pack")
		f, err := os.OpenFile(packPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
		if err != nil {
			return req, fmt.Errorf("create incoming: %w", err)
		}
		if _, err := io.Copy(f, body); err != nil {
			_ = f.Close()
			_ = os.Remove(packPath)
			return req, fmt.Errorf("write incoming: %w", err)
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(packPath)
			return req, fmt.Errorf("close incoming: %w", err)
		}
		req.PackPath = packPath
	}
	return req, nil
}

// validHexOID40 returns true iff s is exactly 40 lowercase hex chars. Git
// OIDs in wire protocols are always lowercase; mixed case would also be
// rejected by downstream tooling, so we don't normalize.
func validHexOID40(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !(('0' <= c && c <= '9') || ('a' <= c && c <= 'f')) {
			return false
		}
	}
	return true
}

// writeReportNotImplemented emits a report-status indicating that the
// receive-pack handler accepted and parsed the request but has not yet
// implemented commit logic (Task 17). This is honest signaling so a real
// Git client surfaces the push as failed rather than silently succeeding.
//
// Format per pack-protocol(5):
//
//	unpack <error-string-or-"ok">\n
//	(ok <ref>\n | ng <ref> <error>\n)+
//	flush
//
// We send "unpack <reason>" and a per-ref "ng <ref> <reason>" for every
// command, which clients render as a failed push.
//
// When the client negotiated side-band-64k (advertised in info/refs as a
// receive-pack capability) the entire report sequence is multiplexed on
// band 1, terminated by a band-level flush, then a final outer pkt-line
// flush. Otherwise the report is written as raw pkt-lines.
func writeReportNotImplemented(w io.Writer, req *receivePackRequest) error {
	const reason = "not-implemented-yet-task-17"
	pw := pktline.NewWriter(w)

	if req.Caps["side-band-64k"] {
		// Buffer the report-status pkt-line stream, then chunk it onto
		// band 1. We can't side-band each pkt-line individually because
		// the client's report-status parser expects a contiguous
		// pkt-line stream on band 1 (terminated by an inner flush, then
		// the side-band stream itself ends with an outer flush).
		var inner bytes.Buffer
		ipw := pktline.NewWriter(&inner)
		if err := ipw.WriteString("unpack " + reason + "\n"); err != nil {
			return err
		}
		for _, u := range req.Updates {
			if err := ipw.WriteString("ng " + u.Refname + " " + reason + "\n"); err != nil {
				return err
			}
		}
		if err := ipw.WriteFlush(); err != nil {
			return err
		}
		sb := pktline.NewSidebandWriter(pw)
		if _, err := sb.WriteData(inner.Bytes()); err != nil {
			return err
		}
		return pw.WriteFlush()
	}

	if err := pw.WriteString("unpack " + reason + "\n"); err != nil {
		return err
	}
	for _, u := range req.Updates {
		if err := pw.WriteString("ng " + u.Refname + " " + reason + "\n"); err != nil {
			return err
		}
	}
	return pw.WriteFlush()
}
