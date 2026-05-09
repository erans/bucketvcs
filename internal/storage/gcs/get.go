package gcs

import (
	"context"
	"fmt"
	"io"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func (g *GCS) Get(ctx context.Context, key string, opts *bvstorage.GetOptions) (*bvstorage.Object, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	obj := g.bucket.Object(applyPrefix(g.cfg.Prefix, key))
	if opts != nil && opts.IfVersionMatches != nil {
		gen, err := parseGen(*opts.IfVersionMatches)
		if err != nil {
			return nil, err
		}
		obj = obj.Generation(gen)
	}
	rdr, err := obj.NewReader(ctx)
	if err != nil {
		return nil, classify(opGet, err)
	}
	attrs, err := obj.Attrs(ctx)
	if err != nil {
		_ = rdr.Close()
		return nil, classify(opHead, err)
	}
	return &bvstorage.Object{
		Body: rdr,
		Metadata: bvstorage.ObjectMetadata{
			Key:         key,
			Version:     versionFromGen(attrs.Generation),
			Size:        attrs.Size,
			ContentType: attrs.ContentType,
			ModifiedAt:  attrs.Updated,
		},
	}, nil
}

func (g *GCS) Head(ctx context.Context, key string) (*bvstorage.ObjectMetadata, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	obj := g.bucket.Object(applyPrefix(g.cfg.Prefix, key))
	attrs, err := obj.Attrs(ctx)
	if err != nil {
		return nil, classify(opHead, err)
	}
	return &bvstorage.ObjectMetadata{
		Key:         key,
		Version:     versionFromGen(attrs.Generation),
		Size:        attrs.Size,
		ContentType: attrs.ContentType,
		ModifiedAt:  attrs.Updated,
	}, nil
}

func (g *GCS) GetRange(ctx context.Context, key string, start, endInclusive int64) (io.ReadCloser, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	if start < 0 || endInclusive < start {
		return nil, fmt.Errorf("%w: invalid range [%d,%d]", bvstorage.ErrInvalidArgument, start, endInclusive)
	}
	obj := g.bucket.Object(applyPrefix(g.cfg.Prefix, key))
	rdr, err := obj.NewRangeReader(ctx, start, endInclusive-start+1)
	if err != nil {
		return nil, classify(opGetRange, err)
	}
	return rdr, nil
}

// versionFromGen / parseGen serialize the GCS generation number as a
// decimal string so ObjectVersion stays opaque to callers.
func versionFromGen(gen int64) bvstorage.ObjectVersion {
	return bvstorage.ObjectVersion{
		Provider: "gcs",
		Token:    fmt.Sprintf("%d", gen),
		Kind:     bvstorage.VersionEtag, // generation plays the etag role
	}
}

func parseGen(v bvstorage.ObjectVersion) (int64, error) {
	if v.Provider != "" && v.Provider != "gcs" {
		return 0, fmt.Errorf("%w: ObjectVersion.Provider=%q (gcs requires \"gcs\")", bvstorage.ErrVersionMismatch, v.Provider)
	}
	var gen int64
	_, err := fmt.Sscanf(v.Token, "%d", &gen)
	if err != nil {
		return 0, fmt.Errorf("%w: ObjectVersion.Token must be a decimal generation: %v", bvstorage.ErrInvalidArgument, err)
	}
	return gen, nil
}
