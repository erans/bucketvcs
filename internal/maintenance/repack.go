package maintenance

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

// RepackOutput describes the local artifacts produced by Repack.
type RepackOutput struct {
	PackID       string // git's trailing SHA-1 over the pack bytes
	PackPath     string // <bareDir>/out/pack-<PackID>.pack
	IdxPath      string // <bareDir>/out/pack-<PackID>.idx
	BitmapPath   string // <bareDir>/out/pack-<PackID>.bitmap (M9.5+; non-empty when present on disk)
	SizeBytes    int64  // size of the .pack file
	PackChecksum string // 40-hex SHA-1 read from the pack's last 20 bytes
}

// Repack invokes gitcli.PackObjectsAllWithBitmap against
// <bareDir>/bare.git and writes the consolidated pack pair plus
// .bitmap sidecar into <bareDir>/out/. Returns the pack ID, paths, and
// pack file size.
//
// BitmapPath is populated only when the .bitmap file actually appeared
// on disk. pack-objects can decline to write a bitmap in degenerate
// cases (empty repo, --all that resolves to no refs); the caller MUST
// treat BitmapPath == "" as "no bitmap this run" and continue without
// recording one on the manifest.
//
// Caller is responsible for cleaning up <bareDir> when done.
func Repack(ctx context.Context, bareDir string) (*RepackOutput, error) {
	outDir := filepath.Join(bareDir, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("repack: mkdir: %w", err)
	}
	prefix := filepath.Join(outDir, "pack")

	bare := filepath.Join(bareDir, "bare.git")
	packID, err := gitcli.PackObjectsAllWithBitmap(ctx, bare, prefix)
	if err != nil {
		return nil, fmt.Errorf("repack: pack-objects: %w", err)
	}
	packPath := prefix + "-" + packID + ".pack"
	idxPath := prefix + "-" + packID + ".idx"
	bitmapPath := prefix + "-" + packID + ".bitmap"
	st, err := os.Stat(packPath)
	if err != nil {
		return nil, fmt.Errorf("repack: stat pack: %w", err)
	}
	trailer, err := readPackTrailer(packPath)
	if err != nil {
		return nil, fmt.Errorf("repack: read trailer: %w", err)
	}
	// Bitmap is optional — pack-objects may skip it (empty pack, no refs).
	if _, err := os.Stat(bitmapPath); err != nil {
		bitmapPath = ""
	}
	return &RepackOutput{
		PackID:       packID,
		PackPath:     packPath,
		IdxPath:      idxPath,
		BitmapPath:   bitmapPath,
		SizeBytes:    st.Size(),
		PackChecksum: trailer,
	}, nil
}

// readPackTrailer returns the 40-hex SHA-1 stored in the final 20 bytes of
// a Git pack file (Git's pack-checksum trailer). This is distinct from the
// SHA-256 storage hash recorded elsewhere; the trailer is what §16.4
// packfile-uri advertisement embeds in the protocol stanza.
func readPackTrailer(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Seek(-20, io.SeekEnd); err != nil {
		return "", err
	}
	var buf [20]byte
	if _, err := io.ReadFull(f, buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}
