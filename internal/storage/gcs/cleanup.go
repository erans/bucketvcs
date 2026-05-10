package gcs

import "context"

// AbortMultipartsUnderPrefix is a conformance-suite test helper. Unlike
// S3, GCS has no server-side multipart sessions to abort: our resumable
// uploads are tracked only in adapter memory. This function exists for
// symmetry with the s3compat test cleanup hook and is a no-op.
func (g *GCS) AbortMultipartsUnderPrefix(ctx context.Context) error {
	return nil
}
