package mirror

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

// RefUpdate describes one ref change to apply to the mirror.
type RefUpdate struct {
	Refname string
	OldOID  string // 40-char hex; "" or 40 zeros for create
	NewOID  string // 40-char hex; 40 zeros for delete
}

// nullOID is the sentinel for create/delete commands.
const nullOID = "0000000000000000000000000000000000000000"

// IngestPack copies packPath (with companion .idx) into the mirror's
// objects/pack/ dir, applies ref updates via git, then writes a fresh
// sentinel from (newManifestVersion, newLatestTx). Caller MUST hold
// m.Lock().
//
// If packPath == "", the pack copy is skipped (delete-only push).
//
// Failure modes (per M3 design spec §7.5–§7.6):
//   - Pack copy fails: nothing applied; return error.
//   - Ref update fails: pack remains as dangling objects (harmless until
//     GC). Sentinel NOT bumped. Next request detects mismatch and rebuilds.
//   - Sentinel write fails: refs are already updated. Sentinel NOT bumped.
//     Next request rebuilds.
//
// The sentinel is bumped only after all earlier steps succeed.
func (m *Mirror) IngestPack(ctx context.Context, packPath string, updates []RefUpdate, newManifestVersion uint64, newLatestTx string) error {
	if packPath != "" {
		if err := copyPackPair(packPath, filepath.Join(m.BareDir(), "objects", "pack")); err != nil {
			return fmt.Errorf("mirror: copy pack: %w", err)
		}
	}
	for _, u := range updates {
		if err := applyRefUpdate(ctx, m.BareDir(), u); err != nil {
			return fmt.Errorf("mirror: ref update %q: %w", u.Refname, err)
		}
	}
	if err := writeSentinel(m.VersionFile(), sentinel{
		ManifestVersion: newManifestVersion,
		LatestTx:        newLatestTx,
	}); err != nil {
		return fmt.Errorf("mirror: write sentinel: %w", err)
	}
	return nil
}

// copyPackPair copies a .pack file and its companion .idx into destDir.
// Both files must already exist on disk before the copy. Each copy uses
// O_CREATE|O_EXCL so a name collision is treated as an idempotent retry
// (the existing file on disk is accepted as-is and no error is returned).
func copyPackPair(packPath, destDir string) error {
	if !strings.HasSuffix(packPath, ".pack") {
		return fmt.Errorf("mirror: pack path must end in .pack, got %q", packPath)
	}
	idxPath := strings.TrimSuffix(packPath, ".pack") + ".idx"
	if _, err := os.Stat(idxPath); err != nil {
		return fmt.Errorf("mirror: missing companion .idx for %q: %w", packPath, err)
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	for _, src := range []string{packPath, idxPath} {
		base := filepath.Base(src)
		if err := copyFile(src, filepath.Join(destDir, base)); err != nil {
			return err
		}
	}
	return nil
}

// copyFile copies src to dst. If dst already exists, the copy is skipped
// silently to support idempotent retry — the caller is responsible for
// ensuring the on-disk content matches before re-invoking IngestPack with
// the same pack basename.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return nil
		}
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return nil
}

// applyRefUpdate dispatches u to the right gitcli call: delete when
// NewOID is the null OID, create when OldOID is empty or null, CAS update
// otherwise.
func applyRefUpdate(ctx context.Context, bareDir string, u RefUpdate) error {
	switch {
	case u.NewOID == nullOID:
		return gitcli.UpdateRefDelete(ctx, bareDir, u.Refname, u.OldOID)
	case u.OldOID == "" || u.OldOID == nullOID:
		return gitcli.UpdateRef(ctx, bareDir, u.Refname, u.NewOID)
	default:
		return gitcli.UpdateRefCAS(ctx, bareDir, u.Refname, u.NewOID, u.OldOID)
	}
}
