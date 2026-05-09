package gcs

import (
	"cloud.google.com/go/storage"
	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

// Config holds GCS adapter configuration. Fully defined in Task 1.2 (config.go).
type Config struct{}

// GCS is the Google Cloud Storage storage.ObjectStore implementation.
type GCS struct {
	cfg    Config
	client *storage.Client
	bucket *storage.BucketHandle
}

// TODO(M7 task 1.16): re-enable interface assertion once all methods exist
// var _ bvstorage.ObjectStore = (*GCS)(nil)

// Capabilities reports the GCS adapter capabilities. MultipartMinPartSize
// is GCS's 256 KiB resumable-chunk minimum. MultipartMaxParts is 0
// because we model MultipartUpload as a single resumable-upload session,
// so there is no per-part cap to enforce. MaxObjectSize is GCS's
// documented 5 TiB blob limit. SignedURLs is reported true; emulators
// that do not implement them return ErrNotSupported at call time and
// the suite skips §29 #10 accordingly.
func (g *GCS) Capabilities() bvstorage.Capabilities {
	return bvstorage.Capabilities{
		SignedURLs:           true,
		StrongList:           true,
		MultipartMinPartSize: 256 << 10, // GCS resumable chunk minimum
		MultipartMaxParts:    0,         // no adapter-imposed cap
		MaxObjectSize:        5 << 40,
	}
}
