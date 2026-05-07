package s3compat

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// AbortMultipartsUnderPrefix lists every in-progress multipart upload
// under the configured key prefix and aborts each one. It is a
// best-effort cleanup helper intended for test harnesses and orphan
// reclamation tools (M16 repair). Errors during individual aborts are
// silently swallowed; the function continues past failures.
func (s *S3Compat) AbortMultipartsUnderPrefix(ctx context.Context) error {
	prefix := s.cfg.Prefix
	in := &s3.ListMultipartUploadsInput{
		Bucket: aws.String(s.cfg.Bucket),
		Prefix: aws.String(prefix),
	}
	for {
		out, err := s.client.ListMultipartUploads(ctx, in)
		if err != nil {
			return classify(opList, err)
		}
		for _, up := range out.Uploads {
			_, abortErr := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
				Bucket:   aws.String(s.cfg.Bucket),
				Key:      up.Key,
				UploadId: up.UploadId,
			})
			_ = abortErr // best-effort; caller logs via the outer return if needed
		}
		if out.IsTruncated == nil || !*out.IsTruncated {
			break
		}
		in.KeyMarker = out.NextKeyMarker
		in.UploadIdMarker = out.NextUploadIdMarker
	}
	return nil
}
