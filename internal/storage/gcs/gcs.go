package gcs

import (
	"cloud.google.com/go/storage"
	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

// GCS is the Google Cloud Storage storage.ObjectStore implementation.
type GCS struct {
	cfg    Config
	client *storage.Client
	bucket *storage.BucketHandle

	// Pre-parsed signing credentials. Populated by Open when the
	// service-account JSON is supplied via cfg.CredentialsJSON or
	// cfg.CredentialsFile, so SignedURL can pass GoogleAccessID +
	// PrivateKey explicitly. This matters when the SDK's credential
	// auto-detect chain is short-circuited — most notably when
	// STORAGE_EMULATOR_HOST is set, which skips loading the JSON
	// entirely. Empty when running on workload identity / metadata
	// server (where the SDK signs via the IAM credentials API).
	signGoogleAccessID string
	signPrivateKey     []byte
}

var _ bvstorage.ObjectStore = (*GCS)(nil)

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
