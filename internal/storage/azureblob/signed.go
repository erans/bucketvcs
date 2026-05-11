package azureblob

import (
	"context"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

// SignedGetURL returns a SAS-based URL granting time-limited GET
// access to key. opts.Expires is clamped to PresignDefaultTTL when
// zero. If neither AccountKey nor ConnectionString is configured the
// adapter cannot sign and returns ErrNotSupported.
//
// opts.ExpectedHash is silently ignored: Azure SAS does not natively
// bind to SHA-256. Integrity for the M11 bundle/pack-uri use case is
// provided by the M8 retention-window dominance contract (signed-URL
// TTL << retention window).
func (a *AzureBlob) SignedGetURL(ctx context.Context, key string, opts bvstorage.SignedURLOptions) (string, error) {
	if err := validateKey(key); err != nil {
		return "", err
	}
	if a.cfg.AccountKey == "" && a.cfg.ConnectionString == "" {
		return "", wrap(bvstorage.ErrNotSupported, nil)
	}
	ttl := opts.Expires
	if ttl <= 0 {
		ttl = a.cfg.PresignDefaultTTL
	}
	bb := a.container.NewBlockBlobClient(applyPrefix(a.cfg.Prefix, key))
	url, err := bb.GetSASURL(
		sas.BlobPermissions{Read: true},
		time.Now().Add(ttl),
		nil,
	)
	if err != nil {
		return "", wrap(bvstorage.ErrNotSupported, err)
	}
	return url, nil
}
