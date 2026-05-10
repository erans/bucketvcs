package maintenance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

// RepackOutput describes the local artifacts produced by Repack.
type RepackOutput struct {
	PackID    string // git's trailing SHA-1 over the pack bytes
	PackPath  string // <bareDir>/out/pack-<PackID>.pack
	IdxPath   string // <bareDir>/out/pack-<PackID>.idx
	SizeBytes int64  // size of the .pack file
}

// Repack invokes gitcli.PackObjectsAll against <bareDir>/bare.git and
// writes the consolidated pack pair into <bareDir>/out/. Returns the
// pack ID, paths, and pack file size.
//
// Caller is responsible for cleaning up <bareDir> when done.
func Repack(ctx context.Context, bareDir string) (*RepackOutput, error) {
	outDir := filepath.Join(bareDir, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("repack: mkdir: %w", err)
	}
	prefix := filepath.Join(outDir, "pack")

	bare := filepath.Join(bareDir, "bare.git")
	packID, err := gitcli.PackObjectsAll(ctx, bare, prefix)
	if err != nil {
		return nil, fmt.Errorf("repack: pack-objects: %w", err)
	}
	packPath := prefix + "-" + packID + ".pack"
	idxPath := prefix + "-" + packID + ".idx"
	st, err := os.Stat(packPath)
	if err != nil {
		return nil, fmt.Errorf("repack: stat pack: %w", err)
	}
	return &RepackOutput{
		PackID:    packID,
		PackPath:  packPath,
		IdxPath:   idxPath,
		SizeBytes: st.Size(),
	}, nil
}
