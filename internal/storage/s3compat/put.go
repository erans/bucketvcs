package s3compat

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

func (s *S3Compat) PutIfAbsent(ctx context.Context, key string, body io.Reader, opts *storage.PutOptions) (storage.ObjectVersion, error) {
	if err := validateKey(key); err != nil {
		return storage.ObjectVersion{}, err
	}
	seekable, err := materializeForRetry(body, s.cfg.UploadPartSize)
	if err != nil {
		return storage.ObjectVersion{}, err
	}
	in := &s3.PutObjectInput{
		Bucket:      aws.String(s.cfg.Bucket),
		Key:         aws.String(applyPrefix(s.cfg.Prefix, key)),
		Body:        seekable,
		IfNoneMatch: aws.String("*"),
	}
	if opts != nil && opts.ContentType != "" {
		in.ContentType = aws.String(opts.ContentType)
	}
	out, err := s.client.PutObject(ctx, in)
	if err != nil {
		return storage.ObjectVersion{}, classify(opPutIfAbsent, err)
	}
	return storage.ObjectVersion{
		Provider: "s3compat",
		Token:    aws.ToString(out.ETag),
		Kind:     storage.VersionEtag,
	}, nil
}

func (s *S3Compat) PutIfVersionMatches(ctx context.Context, key string, expected storage.ObjectVersion, body io.Reader, opts *storage.PutOptions) (storage.ObjectVersion, error) {
	if err := validateKey(key); err != nil {
		return storage.ObjectVersion{}, err
	}
	seekable, err := materializeForRetry(body, s.cfg.UploadPartSize)
	if err != nil {
		return storage.ObjectVersion{}, err
	}
	in := &s3.PutObjectInput{
		Bucket:  aws.String(s.cfg.Bucket),
		Key:     aws.String(applyPrefix(s.cfg.Prefix, key)),
		Body:    seekable,
		IfMatch: aws.String(expected.Token),
	}
	if opts != nil && opts.ContentType != "" {
		in.ContentType = aws.String(opts.ContentType)
	}
	out, err := s.client.PutObject(ctx, in)
	if err != nil {
		return storage.ObjectVersion{}, classify(opPutIfMatch, err)
	}
	return storage.ObjectVersion{
		Provider: "s3compat",
		Token:    aws.ToString(out.ETag),
		Kind:     storage.VersionEtag,
	}, nil
}

// materializeForRetry returns a Reader that the SDK can rewind for
// retries. Bodies <= maxBuffer are buffered into memory; larger bodies
// must already be seekable (io.ReadSeeker) — we surface a clear error
// otherwise.
func materializeForRetry(body io.Reader, maxBuffer int64) (io.Reader, error) {
	if rs, ok := body.(io.ReadSeeker); ok {
		return rs, nil
	}
	limited := io.LimitReader(body, maxBuffer+1)
	buf, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("s3compat: read body: %w", err)
	}
	if int64(len(buf)) > maxBuffer {
		return nil, fmt.Errorf("s3compat: non-seekable body exceeds %d-byte buffer; use multipart for larger uploads", maxBuffer)
	}
	return bytes.NewReader(buf), nil
}
