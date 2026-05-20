package receivepack

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/pktline"
	"github.com/bucketvcs/bucketvcs/internal/repo/oidconst"
)

// ErrFlushOnlyProbe signals that the request body contained only a flush
// packet (no update commands and no pack data). git-remote-curl's
// "stateless RPC" code (remote-curl.c::probe_rpc) issues such a request as
// an authentication/connectivity probe BEFORE sending a large push body in
// chunked encoding. The server must respond 200 OK with an empty body so
// the client proceeds with the real POST. Treating it as a parse error
// (HTTP 400) breaks every push above http.postBuffer (default 1 MiB).
var ErrFlushOnlyProbe = errors.New("receivepack: flush-only probe body")

// ErrBadRequest is returned for malformed pkt-line input or unsupported
// commands. The HTTP adapter maps this to 400.
var ErrBadRequest = errors.New("receivepack: bad request")

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

// parseReceivePackRequest drains the v0 receive-pack request body. It reads
// pkt-line tokens until flush, accumulating <old> <new> <refname> commands;
// the FIRST command line carries a NUL-suffixed capability list. After the
// flush, if any command is a non-delete (NewOID != oidconst.NullOIDHex), the remaining
// body bytes are streamed verbatim into <incoming>/rcv-<rand>.pack.
// On error the returned *receivePackRequest may carry a non-empty PackPath
// the caller must clean up.
func parseReceivePackRequest(ctx context.Context, body io.Reader, incoming string) (*receivePackRequest, error) {
	pr := pktline.NewReader(body)
	req := &receivePackRequest{Caps: map[string]bool{}}
	first := true
	for {
		tok, err := pr.Read()
		if err == io.EOF {
			// A body that ends without any pkt-lines at all is the
			// large-request probe described in ErrFlushOnlyProbe — but
			// real git-remote-curl probes carry a single flush packet
			// (handled in the Flush branch below). A bare EOF without any
			// bytes read is an invalid client request.
			return nil, errors.New("unexpected EOF before flush")
		}
		if err != nil {
			return nil, fmt.Errorf("read commands: %w", err)
		}
		if tok.Type == pktline.Flush {
			// Flush as the very first (and so far only) token with no
			// commands accumulated is the large-request probe; signal the
			// caller to short-circuit with a 200 instead of HTTP 400.
			if first && len(req.Updates) == 0 {
				return req, ErrFlushOnlyProbe
			}
			break
		}
		if tok.Type != pktline.Data {
			return nil, fmt.Errorf("unexpected token %v", tok.Type)
		}
		// Copy payload because pktline reuses its internal buffer.
		line := string(append([]byte{}, tok.Payload...))
		// pack-protocol(5) describes each receive-pack command as
		// "<old> <new> <name>" — the trailing LF is permitted but not
		// required, and real `git push` clients (observed: git 2.54)
		// omit it on the wire. Strip at most one LF if present; reject
		// any further trailing newlines so a malformed payload (e.g.
		// `...main\n\n`) doesn't sneak past the OID/refname checks
		// below.
		if strings.HasSuffix(line, "\n") {
			line = line[:len(line)-1]
			if strings.HasSuffix(line, "\n") {
				return nil, fmt.Errorf("extra LF in command")
			}
		}
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
		if neu == oidconst.NullOIDHex && old == oidconst.NullOIDHex {
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

	// Reject duplicate refnames in a single request. pack-protocol(5)
	// doesn't explicitly forbid duplicates, but our pipeline collapses
	// refUpdates into a map keyed by refname (only the LAST entry wins
	// at BuildAndCommit time) while the per-command status loop still
	// reports BOTH commands as ok. A crafted request with two creates
	// for the same ref could therefore see "ok refs/heads/x" twice with
	// only the second value committed, masking that the client's first
	// command was silently dropped. Rejecting at parse time keeps the
	// invariant "every accepted command corresponds to one applied
	// update" — which the per-ref ok/ng reporting depends on.
	seen := make(map[string]struct{}, len(req.Updates))
	for _, u := range req.Updates {
		if _, dup := seen[u.Refname]; dup {
			return nil, fmt.Errorf("duplicate refname in request: %q", u.Refname)
		}
		seen[u.Refname] = struct{}{}
	}

	allDelete := true
	for _, u := range req.Updates {
		if u.NewOID != oidconst.NullOIDHex {
			allDelete = false
			break
		}
	}
	if allDelete {
		// pack-protocol(5) forbids a packfile after a delete-only command
		// list. Trailing bytes indicate a malformed or attacker-crafted
		// request; reject so we don't silently accept arbitrary garbage.
		// We probe with a 1-byte read against the MaxBytesReader, which
		// returns EOF on a clean end and a non-EOF error if the body
		// exceeded the limit.
		var probe [1]byte
		n, err := body.Read(probe[:])
		if n > 0 {
			return req, errors.New("trailing bytes after delete-only command list")
		}
		if err != nil && err != io.EOF {
			return req, fmt.Errorf("body trailer: %w", err)
		}
		return req, nil
	}

	// Non-delete push: read remaining body into a temp pack file under
	// <mirror>/incoming/. IncomingDir is mkdir'd at mirror.Manager.Open
	// time; the MkdirAll here is defensive in case the directory was
	// removed out-of-band.
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
	written, err := io.Copy(f, body)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(packPath)
		return req, fmt.Errorf("write incoming: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(packPath)
		return req, fmt.Errorf("close incoming: %w", err)
	}
	// Zero-byte body is legal for a non-delete push: the client may be
	// updating a ref to an OID the server already has (e.g. fast-forward
	// to a tip already known via another ref, branch rename via push of
	// the existing tip under a new name). pack-protocol(5) says the
	// packfile is OPTIONAL after the command list, and real `git push`
	// elides it entirely in this case rather than sending a zero-object
	// pack header. Indexing a 0-byte file would fail with "invalid-pack",
	// so leave PackPath empty and let the connectivity check verify the
	// new OID is already present in the bare.
	if written == 0 {
		_ = os.Remove(packPath)
		return req, nil
	}
	req.PackPath = packPath
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
