package azureblob

import (
	"context"
	"fmt"
	"net/http"
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
//
// On PUT, the returned header set carries `x-ms-blob-type: BlockBlob`
// — Azure's Put Blob API rejects requests without it (HTTP 400),
// because the type is part of the blob's create-time metadata and
// cannot be inferred from the body. SAS parameters do not bind
// request headers, so the caller MUST forward this header on the PUT.
// internal/lfs.Store.PresignPut merges this with its own
// Content-Type: application/octet-stream.
func (a *AzureBlob) SignedGetURL(ctx context.Context, key string, opts bvstorage.SignedURLOptions) (string, http.Header, error) {
	if err := validateKey(key); err != nil {
		return "", nil, err
	}
	method := strings.ToUpper(strings.TrimSpace(opts.Method))
	if method == "" {
		method = "GET"
	}
	if method != "GET" && method != "PUT" {
		return "", nil, fmt.Errorf("azureblob: signed-URL method %q: %w", opts.Method, bvstorage.ErrInvalidArgument)
	}
	if a.cfg.AccountKey == "" && a.cfg.ConnectionString == "" {
		return "", nil, wrap(bvstorage.ErrNotSupported, nil)
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
		return "", nil, wrap(bvstorage.ErrNotSupported, err)
	}
	var hdr http.Header
	if method == "PUT" {
		hdr = http.Header{}
		hdr.Set("x-ms-blob-type", "BlockBlob")
	}
	return url, hdr, nil
}
