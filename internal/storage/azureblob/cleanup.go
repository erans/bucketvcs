package azureblob

import "context"

// AbortMultipartsUnderPrefix is a conformance-suite helper. Azure
// Blob has no enumerable multipart-session abstraction — uncommitted
// blocks expire automatically after 7 days. This is a no-op kept for
// symmetry with s3compat.
func (a *AzureBlob) AbortMultipartsUnderPrefix(ctx context.Context) error {
	return nil
}
