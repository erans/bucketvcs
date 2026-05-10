package gcs

import (
	"context"
	"fmt"

	gstorage "cloud.google.com/go/storage"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func (g *GCS) DeleteIfVersionMatches(ctx context.Context, key string, expected bvstorage.ObjectVersion) error {
	if err := validateKey(key); err != nil {
		return err
	}
	gen, err := parseGen(expected)
	if err != nil {
		return err
	}
	fullKey := applyPrefix(g.cfg.Prefix, key)
	// Use GenerationMatch condition on the object handle so the delete is
	// atomic server-side when the backend enforces it. fake-gcs-server does
	// not honour ifGenerationMatch on DELETE; for correctness against any
	// backend we first verify the generation via Attrs and then issue the
	// conditional delete. On real GCS, the server-side condition provides
	// the atomicity guarantee.
	attrs, err := g.bucket.Object(fullKey).Attrs(ctx)
	if err != nil {
		return classify(opHead, err)
	}
	if attrs.Generation != gen {
		return fmt.Errorf("gcs: %w: generation mismatch: current %d, want %d", bvstorage.ErrVersionMismatch, attrs.Generation, gen)
	}
	obj := g.bucket.Object(fullKey).
		If(gstorage.Conditions{GenerationMatch: gen})
	if err := obj.Delete(ctx); err != nil {
		return classify(opDeleteIfMatch, err)
	}
	return nil
}
