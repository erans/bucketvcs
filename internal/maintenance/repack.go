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
	SizeBytes    int64  // size of the .pack file
	PackChecksum string // 40-hex SHA-1 read from the pack's last 20 bytes
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
	trailer, err := readPackTrailer(packPath)
	if err != nil {
		return nil, fmt.Errorf("repack: read trailer: %w", err)
	}
	return &RepackOutput{
		PackID:       packID,
		PackPath:     packPath,
		IdxPath:      idxPath,
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
