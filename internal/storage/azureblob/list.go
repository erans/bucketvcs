package azureblob

import (
	"context"
	"log"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

// blobItemToMetadata converts a BlobItem to ObjectMetadata. If the blob's
// ContentLength is nil (can occur with certain chunked-transfer list
// responses), the item is skipped and a warning is logged rather than
// silently emitting a 0-size entry. Issuing a per-item GetProperties
// fallback would be correct but risks O(N) extra round-trips per page;
// skipping is the safer default for a listing operation.
func (a *AzureBlob) blobItemToMetadata(item *container.BlobItem) (bvstorage.ObjectMetadata, bool) {
	if item.Properties.ContentLength == nil {
		log.Printf("azureblob: list: skipping blob %q: ContentLength is nil", deref(item.Name))
		return bvstorage.ObjectMetadata{}, false
	}
	return bvstorage.ObjectMetadata{
		Key:         stripPrefix(a.cfg.Prefix, deref(item.Name)),
		Version:     versionFromETag(item.Properties.ETag),
		Size:        *item.Properties.ContentLength,
		ContentType: derefStr(item.Properties.ContentType),
		ModifiedAt:  derefTime(item.Properties.LastModified),
	}, true
}

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
			if meta, ok := a.blobItemToMetadata(item); ok {
				page.Objects = append(page.Objects, meta)
			}
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
		if meta, ok := a.blobItemToMetadata(item); ok {
			page.Objects = append(page.Objects, meta)
		}
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
