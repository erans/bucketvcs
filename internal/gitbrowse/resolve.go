package gitbrowse

import (
	"context"
	"fmt"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/browsemodel"
)

// isHex40 reports whether s is exactly 40 lowercase/uppercase hex chars.
func isHex40(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// Resolve splits a URL remainder ("ref/maybe/path" or "<40hex>/path") into a
// resolved {Ref, OID, Path}. It prefers a leading 40-hex OID; otherwise it picks
// the longest known branch/tag that is a slash-delimited prefix of rest.
func (s *Service) Resolve(ctx context.Context, tenant, repoID, rest string) (browsemodel.Resolved, error) {
	rest = strings.Trim(rest, "/")

	// Raw-OID form: <40hex> optionally followed by "/<path>".
	head := rest
	tail := ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		head, tail = rest[:i], rest[i+1:]
	}
	if isHex40(head) {
		return browsemodel.Resolved{Ref: "", OID: head, Path: tail}, nil
	}

	refs, err := s.ListRefs(ctx, tenant, repoID)
	if err != nil {
		return browsemodel.Resolved{}, err
	}
	type cand struct {
		name string
		oid  string
	}
	all := make([]cand, 0, len(refs.Branches)+len(refs.Tags))
	for _, b := range refs.Branches {
		all = append(all, cand{b.Name, b.OID})
	}
	for _, tg := range refs.Tags {
		all = append(all, cand{tg.Name, tg.OID})
	}

	best := cand{}
	for _, c := range all {
		if rest == c.name || strings.HasPrefix(rest, c.name+"/") {
			if len(c.name) > len(best.name) {
				best = c
			}
		}
	}
	if best.name == "" {
		return browsemodel.Resolved{}, fmt.Errorf("resolve %q: %w", rest, browsemodel.ErrNotFound)
	}
	path := strings.TrimPrefix(rest, best.name)
	path = strings.TrimPrefix(path, "/")
	return browsemodel.Resolved{Ref: best.name, OID: best.oid, Path: path}, nil
}
