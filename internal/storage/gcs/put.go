package gcs

import (
	"context"
	"io"

	gstorage "cloud.google.com/go/storage"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func (g *GCS) PutIfAbsent(ctx context.Context, key string, body io.Reader, opts *bvstorage.PutOptions) (bvstorage.ObjectVersion, error) {
	if err := validateKey(key); err != nil {
		return bvstorage.ObjectVersion{}, err
	}
	obj := g.bucket.Object(applyPrefix(g.cfg.Prefix, key)).
		If(gstorage.Conditions{DoesNotExist: true})
	w := obj.NewWriter(ctx)
	w.ChunkSize = g.cfg.UploadChunkSize
	if opts != nil && opts.ContentType != "" {
		w.ContentType = opts.ContentType
	}
	if _, err := io.Copy(w, body); err != nil {
		_ = w.Close()
		return bvstorage.ObjectVersion{}, classify(opPutIfAbsent, err)
	}
	if err := w.Close(); err != nil {
		return bvstorage.ObjectVersion{}, classify(opPutIfAbsent, err)
	}
	return versionFromGen(w.Attrs().Generation), nil
}

func (g *GCS) PutIfVersionMatches(ctx context.Context, key string, expected bvstorage.ObjectVersion, body io.Reader, opts *bvstorage.PutOptions) (bvstorage.ObjectVersion, error) {
	if err := validateKey(key); err != nil {
		return bvstorage.ObjectVersion{}, err
	}
	gen, err := parseGen(expected)
	if err != nil {
		return bvstorage.ObjectVersion{}, err
	}
	obj := g.bucket.Object(applyPrefix(g.cfg.Prefix, key)).
		If(gstorage.Conditions{GenerationMatch: gen})
	w := obj.NewWriter(ctx)
	w.ChunkSize = g.cfg.UploadChunkSize
	if opts != nil && opts.ContentType != "" {
		w.ContentType = opts.ContentType
	}
	if _, err := io.Copy(w, body); err != nil {
		_ = w.Close()
		return bvstorage.ObjectVersion{}, classify(opPutIfMatch, err)
	}
	if err := w.Close(); err != nil {
		return bvstorage.ObjectVersion{}, classify(opPutIfMatch, err)
	}
	return versionFromGen(w.Attrs().Generation), nil
}
