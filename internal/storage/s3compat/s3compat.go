package s3compat

import (
	"context"
	"errors"
	"io"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// S3Compat is the S3-compatible storage.ObjectStore implementation.
type S3Compat struct {
	cfg     Config
	client  *s3.Client
	presign *s3.PresignClient
}

var _ storage.ObjectStore = (*S3Compat)(nil)

// Capabilities reports the S3-compatible adapter capabilities. Values
// match real provider limits (5 MiB / 10000 parts / 5 TiB; strong list;
// signed URLs).
func (s *S3Compat) Capabilities() storage.Capabilities {
	return storage.Capabilities{
		SignedURLs:           true,
		StrongList:           true,
		MultipartMinPartSize: 5 << 20,
		MultipartMaxParts:    10000,
		MaxObjectSize:        5 << 40,
	}
}

// All other ObjectStore methods return errNotImpl until their
// dedicated tasks land. This keeps the package buildable while
// individual methods land one at a time.

var errNotImpl = errors.New("s3compat: not yet implemented (skeleton)")

func (s *S3Compat) Get(ctx context.Context, key string, opts *storage.GetOptions) (*storage.Object, error) {
	return nil, errNotImpl
}

func (s *S3Compat) Head(ctx context.Context, key string) (*storage.ObjectMetadata, error) {
	return nil, errNotImpl
}

func (s *S3Compat) GetRange(ctx context.Context, key string, start, endInclusive int64) (io.ReadCloser, error) {
	return nil, errNotImpl
}

func (s *S3Compat) PutIfAbsent(ctx context.Context, key string, body io.Reader, opts *storage.PutOptions) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, errNotImpl
}

func (s *S3Compat) PutIfVersionMatches(ctx context.Context, key string, expected storage.ObjectVersion, body io.Reader, opts *storage.PutOptions) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, errNotImpl
}

func (s *S3Compat) DeleteIfVersionMatches(ctx context.Context, key string, expected storage.ObjectVersion) error {
	return errNotImpl
}

func (s *S3Compat) List(ctx context.Context, prefix string, opts *storage.ListOptions) (*storage.ListPage, error) {
	return nil, errNotImpl
}

func (s *S3Compat) CreateMultipart(ctx context.Context, key string, opts *storage.MultipartOptions) (storage.MultipartUpload, error) {
	return nil, errNotImpl
}

func (s *S3Compat) CompleteMultipartIfAbsent(ctx context.Context, upload storage.MultipartUpload, parts []storage.MultipartPart) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, errNotImpl
}

func (s *S3Compat) SignedGetURL(ctx context.Context, key string, opts storage.SignedURLOptions) (string, error) {
	return "", errNotImpl
}
