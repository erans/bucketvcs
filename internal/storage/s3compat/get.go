package s3compat

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

func (s *S3Compat) Get(ctx context.Context, key string, opts *storage.GetOptions) (*storage.Object, error) {
	in := &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(applyPrefix(s.cfg.Prefix, key)),
	}
	if opts != nil && opts.IfVersionMatches != nil {
		in.IfMatch = aws.String(opts.IfVersionMatches.Token)
	}
	out, err := s.client.GetObject(ctx, in)
	if err != nil {
		return nil, classify(opGet, err)
	}
	md := storage.ObjectMetadata{
		Key: key,
		Version: storage.ObjectVersion{
			Provider: "s3compat",
			Token:    aws.ToString(out.ETag),
			Kind:     storage.VersionEtag,
		},
		Size: aws.ToInt64(out.ContentLength),
	}
	if out.ContentType != nil {
		md.ContentType = *out.ContentType
	}
	if out.LastModified != nil {
		md.ModifiedAt = *out.LastModified
	} else {
		md.ModifiedAt = time.Time{}
	}
	return &storage.Object{Body: out.Body, Metadata: md}, nil
}

func (s *S3Compat) Head(ctx context.Context, key string) (*storage.ObjectMetadata, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(applyPrefix(s.cfg.Prefix, key)),
	})
	if err != nil {
		return nil, classify(opHead, err)
	}
	md := &storage.ObjectMetadata{
		Key: key,
		Version: storage.ObjectVersion{
			Provider: "s3compat",
			Token:    aws.ToString(out.ETag),
			Kind:     storage.VersionEtag,
		},
		Size: aws.ToInt64(out.ContentLength),
	}
	if out.ContentType != nil {
		md.ContentType = *out.ContentType
	}
	if out.LastModified != nil {
		md.ModifiedAt = *out.LastModified
	}
	return md, nil
}

func (s *S3Compat) GetRange(ctx context.Context, key string, start, endInclusive int64) (io.ReadCloser, error) {
	if start < 0 || endInclusive < 0 || endInclusive < start {
		return nil, fmt.Errorf("%w: invalid range [%d, %d]", storage.ErrInvalidArgument, start, endInclusive)
	}
	rangeHeader := fmt.Sprintf("bytes=%d-%d", start, endInclusive)
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(applyPrefix(s.cfg.Prefix, key)),
		Range:  aws.String(rangeHeader),
	})
	if err != nil {
		return nil, classify(opGetRange, err)
	}
	return out.Body, nil
}
