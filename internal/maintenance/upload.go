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
	ObjectMapHash    string
	ObjectMapBytes   []byte
	CommitGraphHash  string
	CommitGraphBytes []byte
}

type uploadResult struct {
	PackKey        string
	IdxKey         string
	ObjectMapKey   string
	CommitGraphKey string
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
