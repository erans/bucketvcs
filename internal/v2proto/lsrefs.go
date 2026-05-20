package v2proto

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/pktline"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/refstore"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// HandleLsRefs is the legacy entry point for callers that have a body known
// to be inline-mode (no shards). Wraps HandleLsRefsWithStore with a nil
// ObjectStore — works for v1 bodies because the refstore factory routes to
// InlineRefStore in that case.
//
// Sharded bodies will return an error here (refstore.New returns an error when
// asked to construct a ShardedRefStore over a nil store). Callers that may
// handle v2 manifests MUST use HandleLsRefsWithStore.
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
func HandleLsRefs(ctx context.Context, args []pktline.Token, body *manifest.Body, w io.Writer) error {
	return HandleLsRefsWithStore(ctx, args, body, nil, nil, w)
}

// HandleLsRefsWithStore is the M12+ ls-refs handler. It opens a RefStore over
// body (inline or sharded), enumerates refs through it, and emits the
// wire-format output. The store + keys are only consulted for sharded bodies;
// inline bodies route to the in-memory InlineRefStore.
//
// For inline-mode bodies, store and k may be nil.
func HandleLsRefsWithStore(ctx context.Context, args []pktline.Token, body *manifest.Body, store storage.ObjectStore, k *keys.Repo, w io.Writer) error {
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

	rs, err := refstore.New(ctx, store, k, body)
	if err != nil {
		return fmt.Errorf("ls-refs: refstore: %w", err)
	}
	refs, err := rs.List(ctx)
	if err != nil {
		return fmt.Errorf("ls-refs: list: %w", err)
	}

	pw := pktline.NewWriter(w)

	// HEAD line: in v2, HEAD is emitted only if the default branch is in the
	// advertised set, OR (with "unborn") even when missing.
	// body.DefaultBranch is already a fully-qualified ref (e.g. refs/heads/main);
	// see internal/repo/manifest/body.go and the importer/exporter contract.
	headTarget := body.DefaultBranch
	headOID, headExists := refs[headTarget]
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
	names := make([]string, 0, len(refs))
	for name := range refs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !prefixOK(name, prefixes) {
			continue
		}
		oid := refs[name]
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
//	"<capability>\n" ...     (zero or more; e.g. agent=..., object-format=...)
//	delim
//	"<arg-line>\n" ...
//	flush
//
// invoking fn for each <arg-line> with the trailing newline stripped.
// Per gitprotocol-v2, command requests may include capability lines between
// the command line and the delim; those lines are not ls-refs/fetch arguments
// and are tolerated and ignored here. If no delim is present the request has
// no command-specific args and fn is never invoked.
func iterateArgs(args []pktline.Token, expectCmd string, fn func(line string) error) error {
	if len(args) == 0 {
		return fmt.Errorf("v2proto: empty arg stream")
	}
	if args[0].Type != pktline.Data || strings.TrimRight(string(args[0].Payload), "\n") != "command="+expectCmd {
		return fmt.Errorf("v2proto: expected command=%s, got %q", expectCmd, args[0].Payload)
	}

	// Phase 1: skip the command line plus any pre-delim capability lines.
	i := 1
	sawDelim := false
	for ; i < len(args); i++ {
		t := args[i]
		switch t.Type {
		case pktline.Delim:
			sawDelim = true
		case pktline.Data:
			// Capability line; ignore.
			continue
		case pktline.Flush:
			// Request terminated before any args.
			return nil
		default:
			return fmt.Errorf("v2proto: unexpected token %v", t.Type)
		}
		if sawDelim {
			i++
			break
		}
	}
	if !sawDelim {
		// Reached end of stream without a delim — no args.
		return nil
	}

	// Phase 2: emit each arg line until flush or end of stream.
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
