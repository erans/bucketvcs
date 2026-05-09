package azureblob

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func (a *AzureBlob) List(ctx context.Context, prefix string, opts *bvstorage.ListOptions) (*bvstorage.ListPage, error) {
	full := applyPrefix(a.cfg.Prefix, prefix)
	maxResults := int32(1000)
	var marker string
	var delimiter string
	if opts != nil {
		if opts.MaxKeys > 0 {
			maxResults = int32(opts.MaxKeys)
		}
		marker = opts.ContinuationToken
		delimiter = opts.Delimiter
	}

	page := &bvstorage.ListPage{}

	if delimiter == "" {
		pager := a.container.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{
			Prefix:     to.Ptr(full),
			MaxResults: &maxResults,
			Marker:     markerPtr(marker),
		})
		if !pager.More() {
			return page, nil
		}
		resp, err := pager.NextPage(ctx)
		if err != nil {
			return nil, classify(opList, err)
		}
		for _, item := range resp.Segment.BlobItems {
			page.Objects = append(page.Objects, bvstorage.ObjectMetadata{
				Key:         stripPrefix(a.cfg.Prefix, deref(item.Name)),
				Version:     versionFromETag(item.Properties.ETag),
				Size:        deref(item.Properties.ContentLength),
				ContentType: derefStr(item.Properties.ContentType),
				ModifiedAt:  derefTime(item.Properties.LastModified),
			})
		}
		page.NextToken = derefStr(resp.NextMarker)
		return page, nil
	}

	pager := a.container.NewListBlobsHierarchyPager(delimiter, &container.ListBlobsHierarchyOptions{
		Prefix:     to.Ptr(full),
		MaxResults: &maxResults,
		Marker:     markerPtr(marker),
	})
	if !pager.More() {
		return page, nil
	}
	resp, err := pager.NextPage(ctx)
	if err != nil {
		return nil, classify(opList, err)
	}
	for _, item := range resp.Segment.BlobItems {
		page.Objects = append(page.Objects, bvstorage.ObjectMetadata{
			Key:         stripPrefix(a.cfg.Prefix, deref(item.Name)),
			Version:     versionFromETag(item.Properties.ETag),
			Size:        deref(item.Properties.ContentLength),
			ContentType: derefStr(item.Properties.ContentType),
			ModifiedAt:  derefTime(item.Properties.LastModified),
		})
	}
	for _, p := range resp.Segment.BlobPrefixes {
		page.CommonPrefixes = append(page.CommonPrefixes, stripPrefix(a.cfg.Prefix, derefStr(p.Name)))
	}
	page.NextToken = derefStr(resp.NextMarker)
	return page, nil
}

// markerPtr returns nil for empty input. Azure treats nil and "" differently.
func markerPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
