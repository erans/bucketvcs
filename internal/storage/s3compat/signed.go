package s3compat

import (
	"context"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// SignedGetURL returns a presigned URL granting time-limited GET
// access to key. opts.Expires is clamped to PresignDefaultTTL when
// zero. opts.Method is informational; the SDK only supports GET
// presigning via the GetObject route, so non-"GET" methods produce
// the same URL.
//
// When opts.ExpectedHash has the "sha256:" prefix the presigned URL
// requests x-amz-checksum-mode=ENABLED so S3 returns the stored
// object checksum on GET. A downstream verifier compares the returned
// checksum against the supplied ExpectedHash; the URL itself remains
// valid for ordinary GET clients. The prefix check is permissive — the
// adapter does not validate the suffix length or hex content — because
// the binding is advisory and a malformed hash produces no useful
// guarantee regardless of validation.
func (s *S3Compat) SignedGetURL(ctx context.Context, key string, opts storage.SignedURLOptions) (string, error) {
	if err := validateKey(key); err != nil {
		return "", err
	}
	ttl := opts.Expires
	if ttl <= 0 {
		ttl = s.cfg.PresignDefaultTTL
	}
	in := &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(applyPrefix(s.cfg.Prefix, key)),
	}
	if strings.HasPrefix(opts.ExpectedHash, "sha256:") {
		in.ChecksumMode = types.ChecksumModeEnabled
	}
	out, err := s.presign.PresignGetObject(ctx, in, func(po *s3.PresignOptions) {
		po.Expires = ttl
	})
	if err != nil {
		return "", classify(opGet, err)
	}
	return out.URL, nil
}
