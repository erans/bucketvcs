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

	// Defence-in-depth: verify the blob exists before issuing the conditional
	// DELETE. Azurite (and some Azure endpoints) return 412 PreconditionFailed
	// rather than 404 NotFound when IfMatch is applied to an absent blob, which
	// would incorrectly surface as ErrVersionMismatch. By pre-checking with
	// GetProperties we surface ErrNotFound correctly for callers.
	props, err := bb.GetProperties(ctx, nil)
	if err != nil {
		return classify(opHead, err)
	}
	currentEtag := versionFromETag(props.ETag)
	if currentEtag.Token != expected.Token {
		return wrap(bvstorage.ErrVersionMismatch, nil)
	}

	etag := parseETag(expected)
	_, err = bb.Delete(ctx, &blob.DeleteOptions{
		AccessConditions: &blob.AccessConditions{
			ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfMatch: &etag},
		},
	})
	if err != nil {
		return classify(opDeleteIfMatch, err)
	}
	return nil
}
