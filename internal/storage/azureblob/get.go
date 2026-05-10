package azureblob

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

func (a *AzureBlob) Get(ctx context.Context, key string, opts *bvstorage.GetOptions) (*bvstorage.Object, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	bb := a.container.NewBlockBlobClient(applyPrefix(a.cfg.Prefix, key))
	dlOpts := &blob.DownloadStreamOptions{}
	if opts != nil && opts.IfVersionMatches != nil {
		etag := parseETag(*opts.IfVersionMatches)
		dlOpts.AccessConditions = &blob.AccessConditions{
			ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfMatch: &etag},
		}
	}
	resp, err := bb.DownloadStream(ctx, dlOpts)
	if err != nil {
		return nil, classify(opGet, err)
	}

	// Azure may omit ContentLength for chunked transfer responses. Fall back
	// to a GetProperties call to obtain the real size rather than silently
	// returning 0 in ObjectMetadata.Size.
	size := deref(resp.ContentLength)
	etag := resp.ETag
	contentType := resp.ContentType
	modifiedAt := resp.LastModified
	if resp.ContentLength == nil {
		props, propErr := bb.GetProperties(ctx, nil)
		if propErr == nil {
			size = deref(props.ContentLength)
			if etag == nil {
				etag = props.ETag
			}
			if contentType == nil {
				contentType = props.ContentType
			}
			if modifiedAt == nil {
				modifiedAt = props.LastModified
			}
		}
		// If GetProperties also fails we proceed with size=0 rather than
		// discarding a successfully opened body; the caller can detect this
		// via a 0-size metadata field.
	}

	return &bvstorage.Object{
		Body: resp.Body,
		Metadata: bvstorage.ObjectMetadata{
			Key:         key,
			Version:     versionFromETag(etag),
			Size:        size,
			ContentType: derefStr(contentType),
			ModifiedAt:  derefTime(modifiedAt),
		},
	}, nil
}

func (a *AzureBlob) Head(ctx context.Context, key string) (*bvstorage.ObjectMetadata, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	bb := a.container.NewBlockBlobClient(applyPrefix(a.cfg.Prefix, key))
	resp, err := bb.GetProperties(ctx, nil)
	if err != nil {
		return nil, classify(opHead, err)
	}
	// GetProperties returning a nil ContentLength is an Azure protocol error
	// (the field is always present in a well-formed 200 response). Treat it
	// as a transient failure so callers can retry rather than receiving a
	// silently corrupt 0-size result.
	if resp.ContentLength == nil {
		return nil, fmt.Errorf("azureblob: %w: HEAD %s returned nil ContentLength", bvstorage.ErrTransient, key)
	}
	return &bvstorage.ObjectMetadata{
		Key:         key,
		Version:     versionFromETag(resp.ETag),
		Size:        *resp.ContentLength,
		ContentType: derefStr(resp.ContentType),
		ModifiedAt:  derefTime(resp.LastModified),
	}, nil
}

func (a *AzureBlob) GetRange(ctx context.Context, key string, start, endInclusive int64) (io.ReadCloser, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	if start < 0 || endInclusive < start {
		return nil, fmt.Errorf("%w: invalid range [%d,%d]", bvstorage.ErrInvalidArgument, start, endInclusive)
	}
	bb := a.container.NewBlockBlobClient(applyPrefix(a.cfg.Prefix, key))
	resp, err := bb.DownloadStream(ctx, &blob.DownloadStreamOptions{
		Range: blob.HTTPRange{Offset: start, Count: endInclusive - start + 1},
	})
	if err != nil {
		return nil, classify(opGetRange, err)
	}
	return resp.Body, nil
}

// versionFromETag / parseETag round-trip Azure ETags (raw, quotes
// stripped) through ObjectVersion.Token.
func versionFromETag(etagPtr *azcore.ETag) bvstorage.ObjectVersion {
	if etagPtr == nil {
		return bvstorage.ObjectVersion{Provider: "azureblob", Kind: bvstorage.VersionEtag}
	}
	return bvstorage.ObjectVersion{
		Provider: "azureblob",
		Token:    strings.Trim(string(*etagPtr), `"`),
		Kind:     bvstorage.VersionEtag,
	}
}

func parseETag(v bvstorage.ObjectVersion) azcore.ETag {
	if v.Provider != "" && v.Provider != "azureblob" {
		return azcore.ETag("")
	}
	return azcore.ETag(`"` + v.Token + `"`)
}

// pointer-deref helpers. Azure SDK returns lots of *T fields.
func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
func derefStr(p *string) string    { return deref(p) }
func derefTime(p *time.Time) time.Time { return deref(p) }
