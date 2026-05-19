package maintenance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

type uploadInput struct {
	PackID           string
	PackBytes        []byte // ignored when PackPath is set
	PackPath         string // optional; when set, streamed from disk
	IdxBytes         []byte
	IdxPath          string
	BitmapBytes      []byte // optional (M9.5+); ignored when BitmapPath is set
	BitmapPath       string // optional; when set, streamed from disk; "" + nil bytes = no bitmap this run
	ObjectMapHash    string
	ObjectMapBytes   []byte
	CommitGraphHash  string
	CommitGraphBytes []byte
}

type uploadResult struct {
	PackKey        string
	IdxKey         string
	BitmapKey      string // populated when input carried a bitmap AND upload succeeded; empty otherwise
	BitmapErr      error  // non-nil when the bitmap upload was attempted and failed (non-ErrAlreadyExists). The pipeline surfaces this so an operator can distinguish "pack-objects produced no bitmap" (BitmapKey=="" && BitmapErr==nil) from "tried to upload, transient failure" (BitmapKey=="" && BitmapErr!=nil). The maintenance run itself is NOT aborted by a bitmap upload failure (bitmaps are a clone accelerator, not a correctness primitive).
	ObjectMapKey   string
	CommitGraphKey string
}

// uploadIndexesOnly uploads only the .bvcg artifact — no pack, idx, or
// .bvom. Used by the compact-only path where Packs and .bvom are unchanged.
// ObjectMapHash/Bytes are intentionally absent: compact-only must not upload
// a .bvom that references a locally-built pack that is never stored.
type uploadIndexesInput struct {
	CommitGraphHash  string
	CommitGraphBytes []byte
}

type uploadIndexesResult struct {
	CommitGraphKey string
}

// uploadIndexesOnlyArtifacts PutIfAbsent's only the .bvcg artifact. It is
// the compact-only counterpart to uploadArtifacts; .bvom is intentionally
// skipped because the compact-only path preserves prev.Indexes.ObjectMap.
func uploadIndexesOnlyArtifacts(ctx context.Context, s storage.ObjectStore, k *keys.Repo, in uploadIndexesInput) (uploadIndexesResult, error) {
	res := uploadIndexesResult{
		CommitGraphKey: k.CommitGraphKey(in.CommitGraphHash),
	}
	if err := putIfAbsentBytes(ctx, s, res.CommitGraphKey, in.CommitGraphBytes); err != nil && !errors.Is(err, storage.ErrAlreadyExists) {
		return res, fmt.Errorf("upload indexes: bvcg: %w", err)
	}
	return res, nil
}

// uploadArtifacts PutIfAbsent's all four canonical artifacts (pack, idx,
// .bvom, .bvcg). Each key is content-addressed (the pack/idx by git's
// trailing SHA-1 over the pack bytes; .bvom/.bvcg by SHA-256 of their
// bytes), so ErrAlreadyExists on any of them means "same bytes already
// at this key" and is treated as benign — we proceed to the next sidecar
// rather than aborting. This keeps the manifest's pack and index
// references consistent: every key the caller will commit into the
// manifest exists in the store on return.
//
// Non-collision errors (network, auth, etc.) are returned wrapped.
func uploadArtifacts(ctx context.Context, s storage.ObjectStore, k *keys.Repo, in uploadInput) (uploadResult, error) {
	res := uploadResult{
		PackKey:        k.CanonicalPackKey(in.PackID),
		IdxKey:         k.PackIdxKey(in.PackID, "canonical"),
		ObjectMapKey:   k.ObjectMapKey(in.ObjectMapHash),
		CommitGraphKey: k.CommitGraphKey(in.CommitGraphHash),
	}
	if err := putIfAbsentBytesOrFile(ctx, s, res.PackKey, in.PackBytes, in.PackPath); err != nil && !errors.Is(err, storage.ErrAlreadyExists) {
		return res, fmt.Errorf("upload: pack: %w", err)
	}
	if err := putIfAbsentBytesOrFile(ctx, s, res.IdxKey, in.IdxBytes, in.IdxPath); err != nil && !errors.Is(err, storage.ErrAlreadyExists) {
		return res, fmt.Errorf("upload: idx: %w", err)
	}
	// Bitmap (M9.5+) is optional. Skip the PUT entirely when neither
	// bytes nor path is set — the caller is signaling "no bitmap this
	// run". When a bitmap IS present, a non-ErrAlreadyExists PUT
	// failure is DOWN-GRADED to "no bitmap this run" (clear
	// res.BitmapKey, continue) rather than aborting the maintenance
	// pipeline: bitmaps are a clone accelerator, not a correctness
	// primitive. The pack and .idx have already succeeded by this
	// point. Dropping the bitmap means the lazy mirror's git upload-
	// pack falls back to per-object reachability walks until the next
	// maintenance run retries.
	//
	// TODO(observability): the silent downgrade has no log signal
	// today (upload.go is logger-free). Plumb a logger so operators
	// see the warning, mirroring the exporter.downloadBitmapSidecar
	// TODO. Until then, persistent bitmap-coverage trigger firings
	// across runs are the operator's signal that uploads are failing.
	if len(in.BitmapBytes) > 0 || in.BitmapPath != "" {
		bitmapKey := k.PackBitmapKey(in.PackID)
		err := putIfAbsentBytesOrFile(ctx, s, bitmapKey, in.BitmapBytes, in.BitmapPath)
		switch {
		case err == nil || errors.Is(err, storage.ErrAlreadyExists):
			res.BitmapKey = bitmapKey
		default:
			// Transient or otherwise — degrade to "no bitmap this run".
			// res.BitmapKey stays "" so the CAS-merge records empty;
			// res.BitmapErr carries the diagnostic so the pipeline can
			// surface it in the run summary even before a logger is
			// plumbed through.
			res.BitmapErr = err
		}
	}
	if err := putIfAbsentBytes(ctx, s, res.ObjectMapKey, in.ObjectMapBytes); err != nil && !errors.Is(err, storage.ErrAlreadyExists) {
		return res, fmt.Errorf("upload: bvom: %w", err)
	}
	if err := putIfAbsentBytes(ctx, s, res.CommitGraphKey, in.CommitGraphBytes); err != nil && !errors.Is(err, storage.ErrAlreadyExists) {
		return res, fmt.Errorf("upload: bvcg: %w", err)
	}
	return res, nil
}

func putIfAbsentBytes(ctx context.Context, s storage.ObjectStore, key string, body []byte) error {
	_, err := s.PutIfAbsent(ctx, key, bytes.NewReader(body), nil)
	return err
}

func putIfAbsentBytesOrFile(ctx context.Context, s storage.ObjectStore, key string, body []byte, path string) error {
	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = s.PutIfAbsent(ctx, key, f, nil)
		return err
	}
	return putIfAbsentBytes(ctx, s, key, body)
}
