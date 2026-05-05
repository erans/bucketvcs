// Package importer round-trips a bare git repo from local disk into
// bucketvcs storage. See spec §3.6 for the import flow. The exported
// surface is Import(ctx, store, opts); the unexported helpers in this
// file are split out for testability.
package importer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

// Options configures one import.
type Options struct {
	SourceDir     string
	Tenant, Repo  string
	Actor         string
	DefaultBranch string // optional; if empty, taken from source HEAD
}

// Result describes a successful import's resulting state.
type Result struct {
	PackID          string
	ObjectMapHash   string
	CommitGraphHash string
	ManifestVersion uint64
	RefCount        int
	ObjectCount     int
}

// preparedPack is the local-disk artifact set produced before any upload.
// Caller owns WorkDir and must clean it up.
type preparedPack struct {
	WorkDir       string
	PackID        string
	PackPath      string
	IdxPath       string
	Refs          map[string]string
	DefaultBranch string
}

// prepareLocalPack runs steps 1-3 + 5 of §3.6: clone, fsck, pack-objects,
// collect refs + default branch.
func prepareLocalPack(ctx context.Context, sourceDir string) (_ *preparedPack, retErr error) {
	work, err := os.MkdirTemp("", "bucketvcs-import-")
	if err != nil {
		return nil, fmt.Errorf("importer: tmpdir: %w", err)
	}
	defer func() {
		// On error, drop the workdir; on success, the caller takes ownership.
		if retErr != nil {
			_ = os.RemoveAll(work)
		}
	}()

	bare := filepath.Join(work, "src.git")
	if err := gitcli.CloneBareMirror(ctx, sourceDir, bare); err != nil {
		return nil, fmt.Errorf("importer: clone: %w", err)
	}
	if err := gitcli.Fsck(ctx, bare, true); err != nil {
		return nil, fmt.Errorf("importer: source fsck: %w", err)
	}
	prefix := filepath.Join(work, "out", "pack")
	if err := os.MkdirAll(filepath.Dir(prefix), 0o755); err != nil {
		return nil, fmt.Errorf("importer: mkdir pack out: %w", err)
	}
	packID, err := gitcli.PackObjectsAll(ctx, bare, prefix)
	if err != nil {
		return nil, fmt.Errorf("importer: pack-objects: %w", err)
	}
	refs, err := gitcli.ShowRef(ctx, bare)
	if err != nil {
		return nil, fmt.Errorf("importer: show-ref: %w", err)
	}
	headTarget, err := gitcli.SymbolicRef(ctx, bare, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("importer: symbolic-ref HEAD: %w", err)
	}
	return &preparedPack{
		WorkDir:       work,
		PackID:        packID,
		PackPath:      prefix + "-" + packID + ".pack",
		IdxPath:       prefix + "-" + packID + ".idx",
		Refs:          refs,
		DefaultBranch: headTarget,
	}, nil
}
