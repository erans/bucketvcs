package s3compat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

func (s *S3Compat) Get(ctx context.Context, key string, opts *storage.GetOptions) (*storage.Object, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
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
	if err := validateKey(key); err != nil {
		return nil, err
	}
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
	if err := validateKey(key); err != nil {
		return nil, err
	}
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
		// 416 Range Not Satisfiable means the requested range starts at
		// or beyond EOF. Per the storage contract (matching localfs),
		// return an empty reader rather than a transient error.
		var httpErr *awshttp.ResponseError
		if errors.As(err, &httpErr) {
			if httpErr.Response != nil && httpErr.Response.Response != nil &&
				httpErr.Response.Response.StatusCode == 416 {
				return io.NopCloser(bytes.NewReader(nil)), nil
			}
		}
		return nil, classify(opGetRange, err)
	}
	return out.Body, nil
}
