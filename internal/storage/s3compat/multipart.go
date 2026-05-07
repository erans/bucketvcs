package s3compat

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// upload is the S3-backed MultipartUpload. It holds a back-pointer to
// the parent S3Compat so UploadPart and Abort can issue requests.
type upload struct {
	parent   *S3Compat
	uploadID string
	key      string // logical (caller-visible) key
	storeKey string // adapter-prefixed key on the wire
}

var _ storage.MultipartUpload = (*upload)(nil)

func (u *upload) UploadID() string { return u.uploadID }
func (u *upload) Key() string      { return u.key }

func (u *upload) UploadPart(ctx context.Context, partNumber int, body io.Reader) (storage.MultipartPart, error) {
	seekable, err := materializeForRetry(body, u.parent.cfg.UploadPartSize)
	if err != nil {
		return storage.MultipartPart{}, err
	}
	out, err := u.parent.client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:     aws.String(u.parent.cfg.Bucket),
		Key:        aws.String(u.storeKey),
		UploadId:   aws.String(u.uploadID),
		PartNumber: aws.Int32(int32(partNumber)),
		Body:       seekable,
	})
	if err != nil {
		return storage.MultipartPart{}, classify(opUploadPart, err)
	}
	size := bodyKnownSize(seekable)
	return storage.MultipartPart{
		PartNumber: partNumber,
		Token:      aws.ToString(out.ETag),
		Size:       size,
	}, nil
}

func (u *upload) Abort(ctx context.Context) error {
	_, err := u.parent.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(u.parent.cfg.Bucket),
		Key:      aws.String(u.storeKey),
		UploadId: aws.String(u.uploadID),
	})
	if err != nil {
		return classify(opAbortMultipart, err)
	}
	return nil
}

func (s *S3Compat) CreateMultipart(ctx context.Context, key string, opts *storage.MultipartOptions) (storage.MultipartUpload, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	in := &s3.CreateMultipartUploadInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(applyPrefix(s.cfg.Prefix, key)),
	}
	if opts != nil && opts.ContentType != "" {
		in.ContentType = aws.String(opts.ContentType)
	}
	out, err := s.client.CreateMultipartUpload(ctx, in)
	if err != nil {
		return nil, classify(opCreateMultipart, err)
	}
	return &upload{
		parent:   s,
		uploadID: aws.ToString(out.UploadId),
		key:      key,
		storeKey: applyPrefix(s.cfg.Prefix, key),
	}, nil
}

func (s *S3Compat) CompleteMultipartIfAbsent(ctx context.Context, mu storage.MultipartUpload, parts []storage.MultipartPart) (storage.ObjectVersion, error) {
	u, ok := mu.(*upload)
	if !ok {
		return storage.ObjectVersion{}, fmt.Errorf("%w: CompleteMultipartIfAbsent: upload is %T, want *s3compat.upload", storage.ErrInvalidArgument, mu)
	}
	completed := make([]types.CompletedPart, 0, len(parts))
	for _, p := range parts {
		completed = append(completed, types.CompletedPart{
			ETag:       aws.String(p.Token),
			PartNumber: aws.Int32(int32(p.PartNumber)),
		})
	}
	out, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(u.parent.cfg.Bucket),
		Key:             aws.String(u.storeKey),
		UploadId:        aws.String(u.uploadID),
		IfNoneMatch:     aws.String("*"),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	})
	if err != nil {
		return storage.ObjectVersion{}, classify(opCompleteIfAbsent, err)
	}
	return storage.ObjectVersion{
		Provider: "s3compat",
		Token:    aws.ToString(out.ETag),
		Kind:     storage.VersionEtag,
	}, nil
}

// bodyKnownSize attempts to determine the byte length of an
// already-seekable body. Returns 0 if unknown.
func bodyKnownSize(r io.Reader) int64 {
	switch v := r.(type) {
	case *bytes.Reader:
		return int64(v.Len())
	case *bytes.Buffer:
		return int64(v.Len())
	}
	return 0
}
