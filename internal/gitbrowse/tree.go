package gitbrowse

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/browsemodel"
	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

// ReadTree lists one directory level at (oid, path). path "" is the root tree.
func (s *Service) ReadTree(ctx context.Context, tenant, repoID, oid, path string) ([]browsemodel.TreeEntry, error) {
	m, release, err := s.openMirror(ctx, tenant, repoID)
	if err != nil {
		return nil, err
	}
	defer release()

	treeish := oid
	clean := strings.Trim(path, "/")
	if clean != "" {
		treeish = oid + ":" + clean
	}
	out, err := gitcli.LsTree(ctx, m.BareDir(), treeish)
	if err != nil {
		// git exits non-zero for a missing path/oid; treat as not found.
		return nil, fmt.Errorf("ls-tree %q: %w", treeish, browsemodel.ErrNotFound)
	}
	return parseLsTree(out, clean)
}

// parseLsTree parses `git ls-tree --long -z` output. parentPath is the
// directory the listing is relative to ("" for root) and is prefixed onto each
// entry's Path. Entries are sorted directories-first then by name.
func parseLsTree(raw []byte, parentPath string) ([]browsemodel.TreeEntry, error) {
	var entries []browsemodel.TreeEntry
	records := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	for _, rec := range records {
		if rec == "" {
			continue
		}
		// "<mode> <type> <oid> <size|->\t<name>"
		tab := strings.IndexByte(rec, '\t')
		if tab < 0 {
			return nil, fmt.Errorf("gitbrowse: malformed ls-tree record %q", rec)
		}
		meta := strings.Fields(rec[:tab])
		name := rec[tab+1:]
		if len(meta) != 4 {
			return nil, fmt.Errorf("gitbrowse: malformed ls-tree meta %q", rec[:tab])
		}
		var size int64
		if meta[3] != "-" {
			n, err := strconv.ParseInt(meta[3], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("gitbrowse: ls-tree size %q: %w", meta[3], err)
			}
			size = n
		}
		full := name
		if parentPath != "" {
			full = parentPath + "/" + name
		}
		entries = append(entries, browsemodel.TreeEntry{
			Name: name, Path: full, Mode: meta[0], Type: meta[1], OID: meta[2], Size: size,
		})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		di, dj := entries[i].Type == "tree", entries[j].Type == "tree"
		if di != dj {
			return di // trees first
		}
		return entries[i].Name < entries[j].Name
	})
	return entries, nil
}
