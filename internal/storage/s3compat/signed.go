package s3compat

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// SignedGetURL returns a presigned URL for time-limited object access.
// opts.Expires is clamped to PresignDefaultTTL when zero.
//
// opts.Method selects the operation:
//   - "" or "GET" (case-insensitive): presigns a GetObject URL. When
//     opts.ExpectedHash has the "sha256:" prefix the URL requests
//     x-amz-checksum-mode=ENABLED so S3 returns the stored object
//     checksum on GET; a downstream verifier compares it against the
//     supplied ExpectedHash. The prefix check is permissive — the
//     adapter does not validate the suffix length or hex content —
//     because the binding is advisory and a malformed hash produces no
//     useful guarantee regardless of validation.
//   - "PUT" (case-insensitive): presigns a PutObject URL for direct
//     object upload. ExpectedHash and ChecksumMode are ignored on PUT;
//     end-to-end integrity is enforced by a post-upload verify step
//     (see internal/lfs in M13).
//   - any other value: returns storage.ErrInvalidArgument.
//
// The returned header set is nil — S3 v4 presigned URLs bind all
// required state into the URL itself. Clients may add Content-Type
// or other headers on PUT, but none are required for the upload to
// succeed.
func (s *S3Compat) SignedGetURL(ctx context.Context, key string, opts storage.SignedURLOptions) (string, http.Header, error) {
	if err := validateKey(key); err != nil {
		return "", nil, err
	}
	ttl := opts.Expires
	if ttl <= 0 {
		ttl = s.cfg.PresignDefaultTTL
	}
	wireKey := applyPrefix(s.cfg.Prefix, key)
	switch strings.ToUpper(strings.TrimSpace(opts.Method)) {
	case "", "GET":
		in := &s3.GetObjectInput{
			Bucket: aws.String(s.cfg.Bucket),
			Key:    aws.String(wireKey),
		}
		if strings.HasPrefix(opts.ExpectedHash, "sha256:") {
			in.ChecksumMode = types.ChecksumModeEnabled
		}
		out, err := s.presign.PresignGetObject(ctx, in, func(po *s3.PresignOptions) {
			po.Expires = ttl
		})
		if err != nil {
			return "", nil, classify(opGet, err)
		}
		return out.URL, nil, nil
	case "PUT":
		in := &s3.PutObjectInput{
			Bucket: aws.String(s.cfg.Bucket),
			Key:    aws.String(wireKey),
		}
		out, err := s.presign.PresignPutObject(ctx, in, func(po *s3.PresignOptions) {
			po.Expires = ttl
		})
		if err != nil {
			return "", nil, classify(opPresignPut, err)
		}
		return out.URL, nil, nil
	default:
		return "", nil, fmt.Errorf("s3compat: signed-URL method %q: %w", opts.Method, storage.ErrInvalidArgument)
	}
}
