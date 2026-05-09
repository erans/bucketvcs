package gcs

import (
	"context"
	"errors"

	gstorage "cloud.google.com/go/storage"
	"google.golang.org/api/iterator"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func (g *GCS) List(ctx context.Context, prefix string, opts *bvstorage.ListOptions) (*bvstorage.ListPage, error) {
	q := &gstorage.Query{
		Prefix: applyPrefix(g.cfg.Prefix, prefix),
	}
	maxKeys := 1000
	var token string
	if opts != nil {
		if opts.Delimiter != "" {
			q.Delimiter = opts.Delimiter
		}
		if opts.MaxKeys > 0 {
			maxKeys = opts.MaxKeys
		}
		token = opts.ContinuationToken
	}
	it := g.bucket.Objects(ctx, q)
	pager := iterator.NewPager(it, maxKeys, token)

	var batch []*gstorage.ObjectAttrs
	nextToken, err := pager.NextPage(&batch)
	if err != nil && !errors.Is(err, iterator.Done) {
		return nil, classify(opList, err)
	}

	page := &bvstorage.ListPage{NextToken: nextToken}
	for _, attrs := range batch {
		// CommonPrefixes come back as ObjectAttrs with empty Name and
		// non-empty Prefix.
		if attrs.Prefix != "" {
			page.CommonPrefixes = append(page.CommonPrefixes, stripPrefix(g.cfg.Prefix, attrs.Prefix))
			continue
		}
		page.Objects = append(page.Objects, bvstorage.ObjectMetadata{
			Key:         stripPrefix(g.cfg.Prefix, attrs.Name),
			Version:     versionFromGen(attrs.Generation),
			Size:        attrs.Size,
			ContentType: attrs.ContentType,
			ModifiedAt:  attrs.Updated,
		})
	}
	return page, nil
}
