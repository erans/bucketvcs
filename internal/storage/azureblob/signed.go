package azureblob

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

// SignedGetURL returns a SAS-based URL for time-limited object access.
// opts.Expires is clamped to PresignDefaultTTL when zero. If neither
// AccountKey nor ConnectionString is configured the adapter cannot sign
// and returns ErrNotSupported.
//
// opts.Method selects the operation:
//   - "" or "GET" (case-insensitive): SAS with Read permission for
//     time-limited GET access. opts.ExpectedHash is silently ignored:
//     Azure SAS does not natively bind to SHA-256. Integrity for the
//     M11 bundle/pack-uri use case is provided by the M8 retention-window
//     dominance contract (signed-URL TTL << retention window).
//   - "PUT" (case-insensitive): SAS with Write+Create permissions for
//     direct object upload. ExpectedHash is ignored on PUT; end-to-end
//     integrity is enforced by a post-upload verify step (see
//     internal/lfs in M13).
//   - any other value: returns ErrInvalidArgument.
func (a *AzureBlob) SignedGetURL(ctx context.Context, key string, opts bvstorage.SignedURLOptions) (string, error) {
	if err := validateKey(key); err != nil {
		return "", err
	}
	method := strings.ToUpper(strings.TrimSpace(opts.Method))
	if method == "" {
		method = "GET"
	}
	if method != "GET" && method != "PUT" {
		return "", fmt.Errorf("azureblob: signed-URL method %q: %w", opts.Method, bvstorage.ErrInvalidArgument)
	}
	if a.cfg.AccountKey == "" && a.cfg.ConnectionString == "" {
		return "", wrap(bvstorage.ErrNotSupported, nil)
	}
	ttl := opts.Expires
	if ttl <= 0 {
		ttl = a.cfg.PresignDefaultTTL
	}
	var perms sas.BlobPermissions
	switch method {
	case "GET":
		perms.Read = true
	case "PUT":
		perms.Write = true
		perms.Create = true // required for creating a new blob
	}
	bb := a.container.NewBlockBlobClient(applyPrefix(a.cfg.Prefix, key))
	url, err := bb.GetSASURL(
		perms,
		time.Now().Add(ttl),
		nil,
	)
	if err != nil {
		return "", wrap(bvstorage.ErrNotSupported, err)
	}
	return url, nil
}
