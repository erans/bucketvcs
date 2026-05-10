package maintenance

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// downloadPack streams (packKey, idxKey) into <bareDir>/pack-<basename>.{pack,idx}.
// The basename is derived from packKey's content hash where possible
// (last /-separated segment, sans extension); if that fails we fall
// back to a SHA-1 of packKey to keep names unique within bareDir.
//
// Returns the local paths of the written files.
func downloadPack(ctx context.Context, s storage.ObjectStore, packKey, idxKey, bareDir string) (string, string, error) {
	base := basenameFromKey(packKey)
	if base == "" {
		sum := sha1.Sum([]byte(packKey))
		base = "synth-" + hex.EncodeToString(sum[:8])
	}
	packPath := filepath.Join(bareDir, "pack-"+base+".pack")
	idxPath := filepath.Join(bareDir, "pack-"+base+".idx")

	if err := streamToFile(ctx, s, packKey, packPath); err != nil {
		return "", "", fmt.Errorf("downloadPack: pack: %w", err)
	}
	if err := streamToFile(ctx, s, idxKey, idxPath); err != nil {
		return "", "", fmt.Errorf("downloadPack: idx: %w", err)
	}
	return packPath, idxPath, nil
}

// streamToFile streams an ObjectStore key to a local file, creating
// parent directories as needed.
func streamToFile(ctx context.Context, s storage.ObjectStore, key, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	obj, err := s.Get(ctx, key, nil)
	if err != nil {
		return err
	}
	defer obj.Body.Close()
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, obj.Body); err != nil {
		return err
	}
	return nil
}

// basenameFromKey extracts the last /-separated segment of key, then
// strips any trailing .pack / .idx. Returns "" if the result is empty.
func basenameFromKey(key string) string {
	last := key
	for i := len(key) - 1; i >= 0; i-- {
		if key[i] == '/' {
			last = key[i+1:]
			break
		}
	}
	for _, ext := range []string{".pack", ".idx"} {
		if len(last) > len(ext) && last[len(last)-len(ext):] == ext {
			last = last[:len(last)-len(ext)]
		}
	}
	return last
}

// PackRef identifies one canonical pack to materialize. Mirrors the
// minimum subset of manifest.PackEntry that materialize needs.
type PackRef struct {
	PackKey string
	IdxKey  string
}

// MaterializeInput drives one Materialize call.
type MaterializeInput struct {
	BareDir       string            // parent dir; "bare.git/" is created inside
	Packs         []PackRef         // every canonical pack that must end up locally
	Refs          map[string]string // ref → commit oid
	DefaultBranch string            // for HEAD; must be non-empty when Refs is non-empty
}

// Materialize creates <BareDir>/bare.git/objects/pack/, downloads every
// pack pair, writes packed-refs, HEAD, and a minimal config, and runs
// `git fsck --full` to validate the result.
//
// Cleanup: the caller owns BareDir and is responsible for cleaning it
// up, including on error. On a partial failure (e.g. one pack download
// succeeds and the next fails), Materialize returns without removing
// the partial state under BareDir.
func Materialize(ctx context.Context, s storage.ObjectStore, in MaterializeInput) error {
	bare := filepath.Join(in.BareDir, "bare.git")
	packDir := filepath.Join(bare, "objects", "pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		return fmt.Errorf("materialize: mkdir bare: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(bare, "refs"), 0o755); err != nil {
		return fmt.Errorf("materialize: mkdir refs: %w", err)
	}
	if err := writeMinimalConfig(bare); err != nil {
		return fmt.Errorf("materialize: write config: %w", err)
	}
	if err := writeHEAD(bare, in.DefaultBranch); err != nil {
		return fmt.Errorf("materialize: write HEAD: %w", err)
	}
	if err := writePackedRefs(bare, in.Refs); err != nil {
		return fmt.Errorf("materialize: write packed-refs: %w", err)
	}
	for _, p := range in.Packs {
		if _, _, err := downloadPack(ctx, s, p.PackKey, p.IdxKey, packDir); err != nil {
			return fmt.Errorf("materialize: download pack: %w", err)
		}
	}
	if err := gitcli.Fsck(ctx, bare, true); err != nil {
		return fmt.Errorf("%w: %v", ErrCorruptInput, err)
	}
	return nil
}
