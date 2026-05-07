package s3compat

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// DeleteIfVersionMatches removes the object only if the current
// on-store version matches expected.
//
// S3-compatible backends do not reliably enforce If-Match on DELETE
// (AWS S3 treats DELETE as idempotent and may return 204 even when the
// object is absent, regardless of If-Match). To make this method
// honest, we Head the object first to verify the current ETag matches
// expected.Token, then issue DeleteObject with If-Match for race
// safety. The double round-trip is acceptable because DeleteIfVersionMatches
// is GC-path code, not a hot operation.
func (s *S3Compat) DeleteIfVersionMatches(ctx context.Context, key string, expected storage.ObjectVersion) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if err := matchesAdapterShape(expected); err != nil {
		return err
	}

	wireKey := applyPrefix(s.cfg.Prefix, key)

	// Verify the current version matches expected before deleting.
	head, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(wireKey),
	})
	if err != nil {
		return classify(opHead, err) // 404 -> ErrNotFound
	}
	if aws.ToString(head.ETag) != expected.Token {
		return fmt.Errorf("%w: have %q want %q",
			storage.ErrVersionMismatch, aws.ToString(head.ETag), expected.Token)
	}

	// Race-safe delete: if a concurrent writer has updated the object
	// between Head and Delete, the If-Match will fail with 412, which
	// classifies to ErrVersionMismatch.
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket:  aws.String(s.cfg.Bucket),
		Key:     aws.String(wireKey),
		IfMatch: aws.String(expected.Token),
	}); err != nil {
		return classify(opDeleteIfMatch, err)
	}
	return nil
}
