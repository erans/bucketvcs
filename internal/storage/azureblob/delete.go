package azureblob

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func (a *AzureBlob) DeleteIfVersionMatches(ctx context.Context, key string, expected bvstorage.ObjectVersion) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if expected.Provider != "" && expected.Provider != "azureblob" {
		return wrap(bvstorage.ErrVersionMismatch, nil)
	}
	bb := a.container.NewBlockBlobClient(applyPrefix(a.cfg.Prefix, key))
	etag := parseETag(expected)
	_, err := bb.Delete(ctx, &blob.DeleteOptions{
		AccessConditions: &blob.AccessConditions{
			ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfMatch: &etag},
		},
	})
	if err != nil {
		return classify(opDeleteIfMatch, err)
	}
	return nil
}
