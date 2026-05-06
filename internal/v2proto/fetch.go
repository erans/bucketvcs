package v2proto

import (
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/pktline"
)

// FetchRequest is the parsed form of a protocol-v2 "fetch" command body.
//
// Note: want-ref is intentionally not represented; the M3 capability
// advertisement does not expose ref-in-want, and ParseFetchArgs rejects
// want-ref lines outright.
type FetchRequest struct {
	Wants          []string // raw 40-char SHA-1 hex
	Haves          []string // raw 40-char SHA-1 hex
	Done           bool
	ThinPack       bool
	NoProgress     bool
	IncludeTag     bool
	OfsDelta       bool
	Depth          int // 0 = no shallow
	DeepenSince    string
	DeepenNot      []string
	DeepenRelative bool     // "deepen-relative": Depth is measured from current shallow boundary
	Shallow        []string // client-side shallow boundary OIDs
}

// ParseFetchArgs decodes the pkt-line stream of a "fetch" command body.
//
// Layout:
//
//	"command=fetch\n"
//	delim
//	"<arg>\n" repeated
//	flush
func ParseFetchArgs(args []pktline.Token) (FetchRequest, error) {
	var req FetchRequest
	// Track which shallow specifier the client picked. Per protocol-v2,
	// "deepen", "deepen-since", and "deepen-not" are mutually exclusive
	// shallow modes; "deepen-not" may repeat within its own mode.
	const (
		shallowNone = iota
		shallowDepth
		shallowSince
		shallowNot
	)
	shallowMode := shallowNone
	setShallow := func(mode int, name string) error {
		if shallowMode != shallowNone && shallowMode != mode {
			return fmt.Errorf("fetch: %s conflicts with another shallow option", name)
		}
		shallowMode = mode
		return nil
	}
	if err := iterateArgs(args, "fetch", func(line string) error {
		switch {
		case line == "thin-pack":
			req.ThinPack = true
		case line == "no-progress":
			req.NoProgress = true
		case line == "include-tag":
			req.IncludeTag = true
		case line == "ofs-delta":
			req.OfsDelta = true
		case line == "deepen-relative":
			// "deepen-relative" is a flag (no value) modifying the meaning
			// of "deepen <depth>" — depth is counted from the current
			// shallow boundary instead of the tip. It is meaningful only
			// in combination with "deepen", but since the spec allows it
			// to appear before or after deepen we don't enforce ordering
			// here; downstream pack generation interprets it.
			req.DeepenRelative = true
		case line == "done":
			req.Done = true
		case strings.HasPrefix(line, "want "):
			oid := strings.TrimPrefix(line, "want ")
			if !validOID(oid) {
				return fmt.Errorf("fetch: invalid want OID %q", oid)
			}
			req.Wants = append(req.Wants, oid)
		case strings.HasPrefix(line, "want-ref "):
			// "want-ref" requires the "ref-in-want" fetch capability, which
			// the M3 advertisement (see V2Capabilities) does not expose.
			// Reject it so a manually crafted client cannot use an
			// unadvertised extension to make the server resolve refs.
			return fmt.Errorf("fetch: want-ref not supported (ref-in-want not advertised)")
		case strings.HasPrefix(line, "have "):
			oid := strings.TrimPrefix(line, "have ")
			if !validOID(oid) {
				return fmt.Errorf("fetch: invalid have OID %q", oid)
			}
			req.Haves = append(req.Haves, oid)
		case strings.HasPrefix(line, "deepen "):
			n, err := strconv.Atoi(strings.TrimPrefix(line, "deepen "))
			if err != nil || n <= 0 {
				return fmt.Errorf("fetch: invalid deepen %q", line)
			}
			if err := setShallow(shallowDepth, "deepen"); err != nil {
				return err
			}
			req.Depth = n
		case strings.HasPrefix(line, "deepen-since "):
			v := strings.TrimPrefix(line, "deepen-since ")
			ts, err := strconv.ParseInt(v, 10, 64)
			if err != nil || ts <= 0 {
				return fmt.Errorf("fetch: invalid deepen-since %q", v)
			}
			if err := setShallow(shallowSince, "deepen-since"); err != nil {
				return err
			}
			req.DeepenSince = v
		case strings.HasPrefix(line, "deepen-not "):
			ref := strings.TrimPrefix(line, "deepen-not ")
			if ref == "" || strings.ContainsAny(ref, " \t") {
				return fmt.Errorf("fetch: invalid deepen-not %q", ref)
			}
			if err := setShallow(shallowNot, "deepen-not"); err != nil {
				return err
			}
			req.DeepenNot = append(req.DeepenNot, ref)
		case strings.HasPrefix(line, "shallow "):
			oid := strings.TrimPrefix(line, "shallow ")
			if !validOID(oid) {
				return fmt.Errorf("fetch: invalid shallow OID %q", oid)
			}
			req.Shallow = append(req.Shallow, oid)
		case strings.HasPrefix(line, "filter "):
			return fmt.Errorf("fetch: filter not supported in M3")
		default:
			return fmt.Errorf("fetch: unknown argument %q", line)
		}
		return nil
	}); err != nil {
		return FetchRequest{}, err
	}
	if len(req.Wants) == 0 {
		return FetchRequest{}, fmt.Errorf("fetch: no want present")
	}
	return req, nil
}

func validOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	if _, err := hex.DecodeString(s); err != nil {
		return false
	}
	return true
}

// WriteAcknowledgments writes the protocol-v2 "acknowledgments" section that
// precedes the packfile. commons is the (possibly empty) set of haves the
// server is acknowledging as common ancestors. unknown is the set of haves
// the server does not have. ready signals that the server has decided to
// proceed with packfile generation; when false, only ACK lines (or NAK)
// are emitted, leaving the negotiation open for another round.
//
// Per protocol-v2, ACK lines carry just the OID ("ACK <oid>\n") — the
// trailing " common" suffix is the v0/v1 multi_ack_detailed form and is
// not used in v2. If commons is empty we emit "NAK"; otherwise we emit
// one "ACK <oid>" line per common and, when ready is true, a trailing
// "ready" line. A trailing flush is the caller's responsibility.
func WriteAcknowledgments(w io.Writer, commons, unknown []string, ready bool) error {
	pw := pktline.NewWriter(w)
	if err := pw.WriteString("acknowledgments\n"); err != nil {
		return err
	}
	if len(commons) == 0 {
		return pw.WriteString("NAK\n")
	}
	for _, oid := range commons {
		if err := pw.WriteString("ACK " + oid + "\n"); err != nil {
			return err
		}
	}
	if ready {
		return pw.WriteString("ready\n")
	}
	return nil
}
