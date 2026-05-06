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
type FetchRequest struct {
	Wants       []string // raw 40-char SHA-1 hex
	WantRefs    []string // ref names (server-side resolution)
	Haves       []string // raw 40-char SHA-1 hex
	Done        bool
	ThinPack    bool
	NoProgress  bool
	IncludeTag  bool
	OfsDelta    bool
	Depth       int // 0 = no shallow
	DeepenSince string
	DeepenNot   []string
	Shallow     []string // client-side shallow boundary OIDs
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
		case line == "done":
			req.Done = true
		case strings.HasPrefix(line, "want "):
			oid := strings.TrimPrefix(line, "want ")
			if !validOID(oid) {
				return fmt.Errorf("fetch: invalid want OID %q", oid)
			}
			req.Wants = append(req.Wants, oid)
		case strings.HasPrefix(line, "want-ref "):
			ref := strings.TrimPrefix(line, "want-ref ")
			if ref == "" || strings.ContainsAny(ref, " \t") {
				return fmt.Errorf("fetch: invalid want-ref %q", ref)
			}
			req.WantRefs = append(req.WantRefs, ref)
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
			req.Depth = n
		case strings.HasPrefix(line, "deepen-since "):
			req.DeepenSince = strings.TrimPrefix(line, "deepen-since ")
		case strings.HasPrefix(line, "deepen-not "):
			req.DeepenNot = append(req.DeepenNot, strings.TrimPrefix(line, "deepen-not "))
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
	if len(req.Wants) == 0 && len(req.WantRefs) == 0 {
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
// the server does not have.
//
// If len(commons)==0 we emit "NAK"; otherwise we emit ACKs for each common
// plus "ready". A trailing flush is the caller's responsibility.
func WriteAcknowledgments(w io.Writer, commons, unknown []string) error {
	pw := pktline.NewWriter(w)
	if err := pw.WriteString("acknowledgments\n"); err != nil {
		return err
	}
	if len(commons) == 0 {
		return pw.WriteString("NAK\n")
	}
	for _, oid := range commons {
		if err := pw.WriteString("ACK " + oid + " common\n"); err != nil {
			return err
		}
	}
	return pw.WriteString("ready\n")
}
