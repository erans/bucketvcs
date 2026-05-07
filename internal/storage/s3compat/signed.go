package s3compat

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// SignedGetURL returns a presigned URL granting time-limited GET
// access to key. opts.Expires is clamped to PresignDefaultTTL when
// zero. opts.Method is informational; the SDK only supports GET
// presigning via the GetObject route, so non-"GET" methods produce
// the same URL.
func (s *S3Compat) SignedGetURL(ctx context.Context, key string, opts storage.SignedURLOptions) (string, error) {
	if err := validateKey(key); err != nil {
		return "", err
	}
	ttl := opts.Expires
	if ttl <= 0 {
		ttl = s.cfg.PresignDefaultTTL
	}
	out, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(applyPrefix(s.cfg.Prefix, key)),
	}, func(po *s3.PresignOptions) {
		po.Expires = ttl
	})
	if err != nil {
		return "", classify(opGet, err)
	}
	return out.URL, nil
}
