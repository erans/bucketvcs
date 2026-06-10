package gitbrowse

import (
	"context"
	"errors"

	"github.com/bucketvcs/bucketvcs/internal/browsemodel"
	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

// Compare returns the two-dot diff (baseOID..headOID) parsed into per-file
// diffs, reusing parseUnifiedDiff (same caps/truncation as Commit).
func (s *Service) Compare(ctx context.Context, tenant, repoID, baseOID, headOID string) (browsemodel.Comparison, error) {
	m, release, err := s.openMirror(ctx, tenant, repoID)
	if err != nil {
		return browsemodel.Comparison{}, err
	}
	defer release()

	rawDiff, err := gitcli.DiffRefsPatch(ctx, m.BareDir(), baseOID, headOID)
	capped := errors.Is(err, gitcli.ErrOutputCapped)
	if err != nil && !capped {
		return browsemodel.Comparison{}, err
	}
	files, truncated := parseUnifiedDiff(rawDiff)
	if capped {
		if len(files) > 0 {
			files = files[:len(files)-1]
		}
		truncated = true
	}
	add, del := 0, 0
	for i := range files {
		add += files[i].Additions
		del += files[i].Deletions
	}
	return browsemodel.Comparison{Files: files, Additions: add, Deletions: del, Truncated: truncated}, nil
}
