package azureblob

import (
	"bytes"
	"context"
	"io"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

const eTagAny = azcore.ETag("*")

func (a *AzureBlob) PutIfAbsent(ctx context.Context, key string, body io.Reader, opts *bvstorage.PutOptions) (bvstorage.ObjectVersion, error) {
	if err := validateKey(key); err != nil {
		return bvstorage.ObjectVersion{}, err
	}
	buf, err := io.ReadAll(body)
	if err != nil {
		return bvstorage.ObjectVersion{}, err
	}
	bb := a.container.NewBlockBlobClient(applyPrefix(a.cfg.Prefix, key))
	etagAny := eTagAny
	upOpts := &blockblob.UploadOptions{
		AccessConditions: &blob.AccessConditions{
			ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: &etagAny},
		},
	}
	if opts != nil && opts.ContentType != "" {
		upOpts.HTTPHeaders = &blob.HTTPHeaders{BlobContentType: to.Ptr(opts.ContentType)}
	}
	resp, err := bb.Upload(ctx, &readSeekCloser{Reader: bytes.NewReader(buf)}, upOpts)
	if err != nil {
		return bvstorage.ObjectVersion{}, classify(opPutIfAbsent, err)
	}
	return versionFromETag(resp.ETag), nil
}

func (a *AzureBlob) PutIfVersionMatches(ctx context.Context, key string, expected bvstorage.ObjectVersion, body io.Reader, opts *bvstorage.PutOptions) (bvstorage.ObjectVersion, error) {
	if err := validateKey(key); err != nil {
		return bvstorage.ObjectVersion{}, err
	}
	if expected.Provider != "" && expected.Provider != "azureblob" {
		return bvstorage.ObjectVersion{}, wrap(bvstorage.ErrVersionMismatch, nil)
	}
	buf, err := io.ReadAll(body)
	if err != nil {
		return bvstorage.ObjectVersion{}, err
	}
	bb := a.container.NewBlockBlobClient(applyPrefix(a.cfg.Prefix, key))
	etag := parseETag(expected)
	upOpts := &blockblob.UploadOptions{
		AccessConditions: &blob.AccessConditions{
			ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfMatch: &etag},
		},
	}
	if opts != nil && opts.ContentType != "" {
		upOpts.HTTPHeaders = &blob.HTTPHeaders{BlobContentType: to.Ptr(opts.ContentType)}
	}
	resp, err := bb.Upload(ctx, &readSeekCloser{Reader: bytes.NewReader(buf)}, upOpts)
	if err != nil {
		return bvstorage.ObjectVersion{}, classify(opPutIfMatch, err)
	}
	return versionFromETag(resp.ETag), nil
}

// readSeekCloser wraps *bytes.Reader adding a no-op Close so it satisfies
// io.ReadSeekCloser as required by the Azure SDK Upload call.
// multipart.go (Task 2.12) must NOT redefine this type — use this one.
type readSeekCloser struct{ *bytes.Reader }

func (*readSeekCloser) Close() error { return nil }
