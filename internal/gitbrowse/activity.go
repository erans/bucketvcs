package gitbrowse

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/browsemodel"
	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

// treeActivityWindow bounds the attribution walk: entries not touched within
// the most recent N commits render without a last-commit annotation.
const treeActivityWindow = 200

type walkRecord struct {
	meta  browsemodel.CommitMeta
	paths []string
}

// TreeActivity returns, for each direct child of the listed directory, the most
// recent commit touching it — attributed from a single bounded history walk.
// Keys are entry paths as ReadTree produces them (e.g. "sub" or "sub/b.txt"
// when listing "sub"). Entries untouched within the window are absent.
func (s *Service) TreeActivity(ctx context.Context, tenant, repoID, oid, path string) (map[string]browsemodel.CommitMeta, error) {
	clean := strings.Trim(path, "/")
	m, release, err := s.openMirror(ctx, tenant, repoID)
	if err != nil {
		return nil, err
	}
	defer release()

	raw, err := gitcli.LogNameStatus(ctx, m.BareDir(), oid, treeActivityWindow, clean)
	if err != nil && !errors.Is(err, gitcli.ErrOutputCapped) {
		return nil, err
	}
	out := map[string]browsemodel.CommitMeta{}
	for _, rec := range parseNameStatusWalk(raw) {
		for _, p := range rec.paths {
			key, ok := childKey(clean, p)
			if !ok {
				continue
			}
			if _, seen := out[key]; !seen {
				out[key] = rec.meta // records are newest-first; first wins
			}
		}
	}
	return out, nil
}

// childKey maps a touched repo path to the listing entry it belongs to: the
// direct child of dir ("" = root). For dir="": "a/b/c" -> "a", "x.txt" -> "x.txt".
// For dir="sub": "sub/b.txt" -> "sub/b.txt", "sub/d/e" -> "sub/d"; paths outside
// dir return ok=false.
func childKey(dir, p string) (string, bool) {
	rel := p
	if dir != "" {
		if !strings.HasPrefix(p, dir+"/") {
			return "", false
		}
		rel = p[len(dir)+1:]
	}
	if rel == "" {
		return "", false
	}
	if i := strings.IndexByte(rel, '/'); i >= 0 {
		rel = rel[:i]
	}
	if dir != "" {
		return dir + "/" + rel, true
	}
	return rel, true
}

// parseNameStatusWalk parses gitcli.LogNameStatus output (0x1e records: header
// line with 0x1f fields, then STATUS\tpath lines).
func parseNameStatusWalk(raw []byte) []walkRecord {
	var out []walkRecord
	for _, rec := range strings.Split(string(raw), "\x1e") {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		lines := strings.Split(rec, "\n")
		f := strings.Split(lines[0], "\x1f")
		if len(f) != 5 {
			continue
		}
		var at int64
		if n, err := strconv.ParseInt(strings.TrimSpace(f[3]), 10, 64); err == nil {
			at = n
		}
		oid := f[0]
		short := oid
		if len(short) > 12 {
			short = short[:12]
		}
		w := walkRecord{meta: browsemodel.CommitMeta{
			OID: oid, ShortOID: short, AuthorName: f[1], AuthorEmail: f[2],
			AuthorTime: at, Summary: f[4],
		}}
		for _, ln := range lines[1:] {
			tab := strings.IndexByte(ln, '\t')
			if tab <= 0 {
				continue
			}
			w.paths = append(w.paths, ln[tab+1:])
		}
		out = append(out, w)
	}
	return out
}
