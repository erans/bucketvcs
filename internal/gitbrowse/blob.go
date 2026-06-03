package gitbrowse

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/browsemodel"
	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

const (
	// maxBlobBytes is the hard cap on blob bytes read for the blob/raw views.
	maxBlobBytes = 10 << 20 // 10 MiB
	// binarySniffWindow is how many leading bytes are scanned for a NUL.
	binarySniffWindow = 8 << 10 // 8 KiB
)

// ReadBlob returns the blob at (oid, path). It confirms the path is a blob,
// records the size, applies the hard size cap, and detects binary content.
// Binary blobs still carry their bytes (Bytes is nil only when TooLarge).
func (s *Service) ReadBlob(ctx context.Context, tenant, repoID, oid, path string) (browsemodel.Blob, error) {
	clean := strings.Trim(path, "/")
	if clean == "" {
		return browsemodel.Blob{}, fmt.Errorf("read blob: empty path: %w", browsemodel.ErrNotFound)
	}
	m, release, err := s.openMirror(ctx, tenant, repoID)
	if err != nil {
		return browsemodel.Blob{}, err
	}
	defer release()

	rev := oid + ":" + clean
	typ, err := gitcli.CatFileType(ctx, m.BareDir(), rev)
	if err != nil || typ != "blob" {
		return browsemodel.Blob{}, fmt.Errorf("read blob %q: %w", rev, browsemodel.ErrNotFound)
	}
	size, err := gitcli.CatFileSize(ctx, m.BareDir(), rev)
	if err != nil {
		return browsemodel.Blob{}, err
	}
	out := browsemodel.Blob{Path: clean, Size: size}
	if size > maxBlobBytes {
		out.TooLarge = true
		return out, nil
	}
	data, err := gitcli.CatBlob(ctx, m.BareDir(), rev)
	if err != nil {
		return browsemodel.Blob{}, err
	}
	out.Bytes = data
	window := data
	if len(window) > binarySniffWindow {
		window = window[:binarySniffWindow]
	}
	if bytes.IndexByte(window, 0x00) >= 0 {
		out.Binary = true
	}
	return out, nil
}
