package s3compat

import (
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

func (s *S3Compat) Name() string { return "s3compat" }

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
