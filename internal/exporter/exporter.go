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

// Export materializes a normal bare git repo at DestDir from
// bucketvcs storage. See spec §6.3.
//
// Atomicity & failure semantics:
//
// Successful Export produces a fsck-clean bare git repo at DestDir.
// Failure paths:
//   - Step 1 (Open/ReadRoot/Unmarshal): no DestDir created.
//   - Step 2-onwards (init-bare, downloads, refs, fsck): DestDir
//     contains a partial bare repo. Retrying Export with the same
//     DestDir gets ErrDestNotEmpty; the operator must rm -rf and
//     retry. This is a known M2 limitation parallel to Import's
//     stranded-repo behavior. A future iteration may stage into a
//     temp dir and rename atomically on success.
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
		if !validRefName(ref) {
			return nil, fmt.Errorf("exporter: ref name %q not in refs/* namespace or malformed", ref)
		}
		if !validOID(oid) {
			return nil, fmt.Errorf("exporter: ref %s has invalid OID %q", ref, oid)
		}
		if err := gitcli.CheckRefFormat(ctx, ref); err != nil {
			return nil, fmt.Errorf("exporter: ref %s rejected by git check-ref-format: %w", ref, err)
		}
		if err := gitcli.UpdateRef(ctx, opts.DestDir, ref, oid); err != nil {
			return nil, fmt.Errorf("exporter: update-ref %s: %w", ref, err)
		}
	}
	if body.DefaultBranch != "" {
		if !validRefName(body.DefaultBranch) {
			return nil, fmt.Errorf("exporter: default_branch %q not a valid refs/* name", body.DefaultBranch)
		}
		if err := gitcli.CheckRefFormat(ctx, body.DefaultBranch); err != nil {
			return nil, fmt.Errorf("exporter: default_branch %q rejected by git check-ref-format: %w", body.DefaultBranch, err)
		}
		// If refs are non-empty, default_branch must point to one of them.
		if len(body.Refs) > 0 {
			if _, ok := body.Refs[body.DefaultBranch]; !ok {
				return nil, fmt.Errorf("exporter: default_branch %q not present in refs", body.DefaultBranch)
			}
		}
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
// and runs git index-pack to (re)build the .idx. When the manifest carries
// a non-empty BitmapKey for this pack (M9.5+), the .bitmap sidecar is also
// downloaded into objects/pack/ so real git upload-pack picks it up on
// clone. git index-pack does NOT regenerate a bitmap, so the .bitmap must
// come from storage — there is no local fallback.
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
	absPack, err := filepath.Abs(dstPack)
	if err != nil {
		return 0, fmt.Errorf("exporter: abs path: %w", err)
	}
	if err := gitcli.IndexPack(ctx, destDir, absPack); err != nil {
		return 0, fmt.Errorf("exporter: index-pack: %w", err)
	}

	// M9.5: download the .bitmap sidecar when the manifest carries one.
	// Bitmaps are a clone accelerator, not a correctness primitive,
	// so failures here are non-fatal — but they ARE distinguishable:
	// ErrNotFound (bitmap raced GC; pack still valid) is silently
	// skipped, while any other error (transient backend hiccup, auth
	// failure, corrupted blob) is propagated. The pre-M9.5 case
	// (empty BitmapKey) is simply skipped.
	//
	// TODO(observability): the ErrNotFound branch has no log signal
	// today (no logger plumbed into downloadAndIndexPack). An
	// operator investigating "why don't clones use bitmaps?" sees no
	// signal that the download was attempted and skipped. Plumb a
	// logger so this matches Report.BitmapUploadError on the upload
	// side.
	if p.BitmapKey != "" {
		if err := downloadBitmapSidecar(ctx, store, p.BitmapKey, packDir, p.PackID); err != nil {
			if !errors.Is(err, storage.ErrNotFound) {
				return 0, fmt.Errorf("exporter: bitmap download: %w", err)
			}
		}
	}
	return p.ObjectCount, nil
}

// downloadBitmapSidecar fetches the .bitmap blob from storage and writes
// it as pack-<id>.bitmap next to the corresponding .pack/.idx. Path
// containment is verified the same way the pack path is.
func downloadBitmapSidecar(ctx context.Context, store storage.ObjectStore, bitmapKey, packDir, packID string) error {
	dstBitmap := filepath.Join(packDir, "pack-"+packID+".bitmap")
	cleaned := filepath.Clean(dstBitmap)
	if !strings.HasPrefix(cleaned, filepath.Clean(packDir)+string(filepath.Separator)) {
		return fmt.Errorf("exporter: bitmap path escapes packDir: %s", cleaned)
	}
	obj, err := store.Get(ctx, bitmapKey, nil)
	if err != nil {
		return err
	}
	defer obj.Body.Close()
	out, err := os.Create(dstBitmap)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, obj.Body); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

const nullOID = "0000000000000000000000000000000000000000"

// validRefName reports whether s is in the refs/* namespace and has a
// reasonable shape. Pseudo-refs (HEAD, ORIG_HEAD, FETCH_HEAD) are rejected
// here because UpdateRef on them would mutate symbolic state, not create
// a regular ref. A separate gitcli.CheckRefFormat call provides the
// authoritative validation.
func validRefName(s string) bool {
	if !strings.HasPrefix(s, "refs/") {
		return false
	}
	if strings.Contains(s, "//") || strings.Contains(s, "/.") || strings.HasSuffix(s, "/") {
		return false
	}
	if strings.Contains(s, "..") || strings.Contains(s, "@{") {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= ' ' || c == '~' || c == '^' || c == ':' || c == '?' || c == '*' || c == '[' || c == 0x7f {
			return false
		}
	}
	return true
}

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
