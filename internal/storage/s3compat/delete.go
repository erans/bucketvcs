package s3compat

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

func (s *S3Compat) DeleteIfVersionMatches(ctx context.Context, key string, expected storage.ObjectVersion) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if err := matchesAdapterShape(expected); err != nil {
		return err
	}
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket:  aws.String(s.cfg.Bucket),
		Key:     aws.String(applyPrefix(s.cfg.Prefix, key)),
		IfMatch: aws.String(expected.Token),
	})
	if err != nil {
		return classify(opDeleteIfMatch, err)
	}
	return nil
}
