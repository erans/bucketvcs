package gcs

import (
	"context"

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
	obj := g.bucket.Object(applyPrefix(g.cfg.Prefix, key)).
		If(gstorage.Conditions{GenerationMatch: gen})
	if err := obj.Delete(ctx); err != nil {
		return classify(opDeleteIfMatch, err)
	}
	return nil
}
