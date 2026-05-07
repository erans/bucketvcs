package s3compat

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

func (s *S3Compat) List(ctx context.Context, prefix string, opts *storage.ListOptions) (*storage.ListPage, error) {
	in := &s3.ListObjectsV2Input{
		Bucket: aws.String(s.cfg.Bucket),
		Prefix: aws.String(applyPrefix(s.cfg.Prefix, prefix)),
	}
	if opts != nil {
		if opts.MaxKeys > 0 {
			in.MaxKeys = aws.Int32(int32(opts.MaxKeys))
		}
		if opts.ContinuationToken != "" {
			in.ContinuationToken = aws.String(opts.ContinuationToken)
		}
		if opts.Delimiter != "" {
			in.Delimiter = aws.String(opts.Delimiter)
		}
	}
	out, err := s.client.ListObjectsV2(ctx, in)
	if err != nil {
		return nil, classify(opList, err)
	}
	page := &storage.ListPage{}
	for _, obj := range out.Contents {
		stored := aws.ToString(obj.Key)
		k, err := stripPrefix(s.cfg.Prefix, stored)
		if err != nil {
			return nil, err
		}
		page.Objects = append(page.Objects, storage.ObjectMetadata{
			Key: k,
			Version: storage.ObjectVersion{
				Provider: "s3compat",
				Token:    aws.ToString(obj.ETag),
				Kind:     storage.VersionEtag,
			},
			Size: aws.ToInt64(obj.Size),
		})
	}
	for _, cp := range out.CommonPrefixes {
		stored := aws.ToString(cp.Prefix)
		p, err := stripPrefix(s.cfg.Prefix, stored)
		if err != nil {
			return nil, err
		}
		page.CommonPrefixes = append(page.CommonPrefixes, p)
	}
	if out.IsTruncated != nil && *out.IsTruncated {
		page.NextToken = aws.ToString(out.NextContinuationToken)
	}
	return page, nil
}
