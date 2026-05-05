// Package exporter materializes a normal bare git repo on local disk
// from bucketvcs storage. See spec §3.6 (export side) and §6.3.
package exporter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Options configures one export.
type Options struct {
	Tenant, Repo string
	DestDir      string
	SkipFsck     bool // by default, fsck runs after materialization
}

// Result describes a successful export.
type Result struct {
	ManifestVersion uint64
	ObjectCount     int
	FsckOK          bool // true if fsck ran and passed; false if SkipFsck or fsck failed
}

// ErrDestNotEmpty is returned when DestDir exists with content.
var ErrDestNotEmpty = errors.New("exporter: dest dir exists and is not empty")

// ErrMissingObject is returned when a referenced bucket key is absent.
var ErrMissingObject = errors.New("exporter: bucket missing referenced object")

// Export downloads packs/indexes from store, materializes a normal bare
// git repo at DestDir, and (unless SkipFsck is set) runs git fsck.
func Export(ctx context.Context, store storage.ObjectStore, opts Options) (*Result, error) {
	if opts.Tenant == "" || opts.Repo == "" || opts.DestDir == "" {
		return nil, fmt.Errorf("exporter: Tenant, Repo, DestDir required")
	}
	if err := requireEmptyDir(opts.DestDir); err != nil {
		return nil, err
	}

	r, err := repo.Open(ctx, store, opts.Tenant, opts.Repo)
	if err != nil {
		return nil, err
	}
	view, err := r.ReadRoot(ctx)
	if err != nil {
		return nil, err
	}
	var body manifest.Body
	if err := json.Unmarshal(view.Body, &body); err != nil {
		return nil, fmt.Errorf("exporter: unmarshal body: %w", err)
	}

	if err := os.MkdirAll(opts.DestDir, 0o755); err != nil {
		return nil, fmt.Errorf("exporter: mkdir: %w", err)
	}
	if err := gitcli.InitBare(ctx, opts.DestDir); err != nil {
		return nil, fmt.Errorf("exporter: InitBare: %w", err)
	}

	objectCount := 0
	for _, p := range body.Packs {
		count, err := downloadAndIndexPack(ctx, store, p, opts.DestDir)
		if err != nil {
			return nil, err
		}
		objectCount += count
	}

	for ref, oid := range body.Refs {
		if !validOID(oid) {
			return nil, fmt.Errorf("exporter: ref %s has invalid OID %q", ref, oid)
		}
		if err := gitcli.UpdateRef(ctx, opts.DestDir, ref, oid); err != nil {
			return nil, fmt.Errorf("exporter: update-ref %s: %w", ref, err)
		}
	}
	if body.DefaultBranch != "" {
		if err := gitcli.SymbolicRefSet(ctx, opts.DestDir, "HEAD", body.DefaultBranch); err != nil {
			return nil, fmt.Errorf("exporter: set HEAD: %w", err)
		}
	}

	res := &Result{ManifestVersion: view.Header.ManifestVersion, ObjectCount: objectCount}
	if !opts.SkipFsck {
		if err := gitcli.Fsck(ctx, opts.DestDir, true); err != nil {
			return res, fmt.Errorf("exporter: fsck: %w", err)
		}
		res.FsckOK = true
	}
	return res, nil
}

func requireEmptyDir(p string) error {
	st, err := os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("exporter: dest %s is not a directory", p)
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return ErrDestNotEmpty
	}
	return nil
}

// downloadAndIndexPack copies the .pack from store into dest's objects/pack/
// and runs git index-pack to (re)build the .idx.
func downloadAndIndexPack(ctx context.Context, store storage.ObjectStore, p manifest.PackEntry, destDir string) (int, error) {
	if !validOID(p.PackID) {
		return 0, fmt.Errorf("exporter: invalid PackID %q (want 40-char lowercase hex)", p.PackID)
	}
	packDir := filepath.Join(destDir, "objects", "pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		return 0, err
	}
	dstPack := filepath.Join(packDir, "pack-"+p.PackID+".pack")
	// Defense-in-depth: verify dstPack is still under packDir after cleaning.
	cleaned := filepath.Clean(dstPack)
	if !strings.HasPrefix(cleaned, filepath.Clean(packDir)+string(filepath.Separator)) {
		return 0, fmt.Errorf("exporter: pack path escapes packDir: %s", cleaned)
	}

	obj, err := store.Get(ctx, p.PackKey, nil)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return 0, fmt.Errorf("%w: %s", ErrMissingObject, p.PackKey)
		}
		return 0, err
	}
	defer obj.Body.Close()

	out, err := os.Create(dstPack)
	if err != nil {
		return 0, err
	}
	if _, err := io.Copy(out, obj.Body); err != nil {
		_ = out.Close()
		return 0, err
	}
	if err := out.Close(); err != nil {
		return 0, err
	}
	if err := gitcli.IndexPack(ctx, destDir, dstPack); err != nil {
		return 0, fmt.Errorf("exporter: index-pack: %w", err)
	}
	return p.ObjectCount, nil
}

const nullOID = "0000000000000000000000000000000000000000"

// validOID reports whether s is a 40-character lowercase hex Git OID
// other than the null OID. Used to validate manifest-supplied OIDs
// before passing to git CLI. The null OID is forbidden because passing
// it to `git update-ref` deletes the ref rather than setting it.
func validOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	if s == nullOID {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
