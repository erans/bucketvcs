package gitbrowse

import (
	"context"
	"strconv"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/browsemodel"
	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

// MaxLogLimit caps a single log page.
const MaxLogLimit = 100

// Log returns one page of commits reachable from oid. It requests limit+1 rows
// to compute `more` without a second query. limit<=0 defaults to 50; limit is
// capped at MaxLogLimit; offset<0 is clamped to 0.
func (s *Service) Log(ctx context.Context, tenant, repoID, oid string, offset, limit int) ([]browsemodel.CommitMeta, bool, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > MaxLogLimit {
		limit = MaxLogLimit
	}
	if offset < 0 {
		offset = 0
	}
	m, release, err := s.openMirror(ctx, tenant, repoID)
	if err != nil {
		return nil, false, err
	}
	defer release()

	raw, err := gitcli.LogRaw(ctx, m.BareDir(), oid, offset, limit+1)
	if err != nil {
		return nil, false, err
	}
	metas, err := parseLog(raw)
	if err != nil {
		return nil, false, err
	}
	more := false
	if len(metas) > limit {
		more = true
		metas = metas[:limit]
	}
	return metas, more, nil
}

// LogPath returns one page of commits touching path, reachable from oid. It
// mirrors Log's pagination. Rename-following (git --follow) is enabled only
// when path resolves to a single blob; for directories or an undeterminable
// kind it is omitted (git --follow is single-file only).
func (s *Service) LogPath(ctx context.Context, tenant, repoID, oid, path string, offset, limit int) ([]browsemodel.CommitMeta, bool, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > MaxLogLimit {
		limit = MaxLogLimit
	}
	if offset < 0 {
		offset = 0
	}
	m, release, err := s.openMirror(ctx, tenant, repoID)
	if err != nil {
		return nil, false, err
	}
	defer release()

	follow := false
	if kind, kerr := gitcli.PathKind(ctx, m.BareDir(), oid, path); kerr == nil && kind == "blob" {
		follow = true
	}
	raw, err := gitcli.LogRawPath(ctx, m.BareDir(), oid, path, follow, offset, limit+1)
	if err != nil {
		return nil, false, err
	}
	metas, err := parseLog(raw)
	if err != nil {
		return nil, false, err
	}
	more := false
	if len(metas) > limit {
		more = true
		metas = metas[:limit]
	}
	return metas, more, nil
}

// parseLog parses the 0x1e-record / 0x1f-field format emitted by gitcli.LogRaw.
func parseLog(raw []byte) ([]browsemodel.CommitMeta, error) {
	var out []browsemodel.CommitMeta
	for _, rec := range strings.Split(string(raw), "\x1e") {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.Split(rec, "\x1f")
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
		out = append(out, browsemodel.CommitMeta{
			OID: oid, ShortOID: short, AuthorName: f[1], AuthorEmail: f[2],
			AuthorTime: at, Summary: f[4],
		})
	}
	return out, nil
}
