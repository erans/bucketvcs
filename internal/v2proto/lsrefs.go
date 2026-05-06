package v2proto

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/pktline"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

// HandleLsRefs implements the protocol-v2 "ls-refs" command. Args is the full
// pkt-line token stream that was the request body (including the
// "command=ls-refs" line, the delim, the args, and the trailing flush). The
// response is written to w as pkt-line frames followed by a flush.
//
// Supported argument keywords (each on its own pkt-line frame):
//
//	peel              — emit "peeled:<oid>" annotations for tag refs (no-op
//	                    in M3: peel info would require object-store reads
//	                    that we'd rather defer; see "limitations" below).
//	symrefs           — emit "symref-target:<target>" annotations for the
//	                    HEAD symref.
//	unborn            — include an "unborn" line for HEAD when the default
//	                    branch ref does not yet exist.
//	ref-prefix <pfx>  — restrict output to refs whose name starts with <pfx>.
//	                    Multiple ref-prefix args union (any-of).
//
// Limitations: M3 advertises ls-refs=unborn only. peel is parsed but does
// nothing because tag-peeling requires object-store reads (it can be added
// later by walking commitgraph). Tests for peel are not part of the M3
// matrix.
func HandleLsRefs(args []pktline.Token, body *manifest.Body, w io.Writer) error {
	var (
		wantSymrefs bool
		wantUnborn  bool
		prefixes    []string
	)
	if err := iterateArgs(args, "ls-refs", func(line string) error {
		switch {
		case line == "peel":
			// Parsed but not implemented in M3.
		case line == "symrefs":
			wantSymrefs = true
		case line == "unborn":
			wantUnborn = true
		case strings.HasPrefix(line, "ref-prefix "):
			prefix := strings.TrimPrefix(line, "ref-prefix ")
			if prefix == "" || strings.ContainsAny(prefix, " \t") {
				return fmt.Errorf("ls-refs: invalid ref-prefix %q", prefix)
			}
			prefixes = append(prefixes, prefix)
		default:
			return fmt.Errorf("ls-refs: unknown argument %q", line)
		}
		return nil
	}); err != nil {
		return err
	}

	pw := pktline.NewWriter(w)

	// HEAD line: in v2, HEAD is emitted only if the default branch is in the
	// advertised set, OR (with "unborn") even when missing.
	headTarget := "refs/heads/" + body.DefaultBranch
	headOID, headExists := body.Refs[headTarget]
	if (headExists || wantUnborn) && prefixOK("HEAD", prefixes) {
		var line string
		switch {
		case headExists:
			line = headOID + " HEAD"
		default:
			line = "unborn HEAD"
		}
		if wantSymrefs && body.DefaultBranch != "" {
			line += " symref-target:" + headTarget
		}
		if err := pw.WriteString(line + "\n"); err != nil {
			return err
		}
	}

	// Other refs.
	names := make([]string, 0, len(body.Refs))
	for name := range body.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !prefixOK(name, prefixes) {
			continue
		}
		oid := body.Refs[name]
		if err := pw.WriteString(oid + " " + name + "\n"); err != nil {
			return err
		}
	}
	return pw.WriteFlush()
}

func prefixOK(name string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// iterateArgs walks a pkt-line token stream of the shape
//
//	"command=<cmd>\n"
//	delim
//	"<arg-line>\n" ...
//	flush
//
// invoking fn for each <arg-line> with the trailing newline stripped.
func iterateArgs(args []pktline.Token, expectCmd string, fn func(line string) error) error {
	if len(args) == 0 {
		return fmt.Errorf("v2proto: empty arg stream")
	}
	if args[0].Type != pktline.Data || strings.TrimRight(string(args[0].Payload), "\n") != "command="+expectCmd {
		return fmt.Errorf("v2proto: expected command=%s, got %q", expectCmd, args[0].Payload)
	}
	i := 1
	if i < len(args) && args[i].Type == pktline.Delim {
		i++
	}
	for ; i < len(args); i++ {
		t := args[i]
		switch t.Type {
		case pktline.Flush:
			return nil
		case pktline.Data:
			line := strings.TrimRight(string(t.Payload), "\n")
			if err := fn(line); err != nil {
				return err
			}
		default:
			return fmt.Errorf("v2proto: unexpected token %v", t.Type)
		}
	}
	return nil // tolerate missing trailing flush
}
