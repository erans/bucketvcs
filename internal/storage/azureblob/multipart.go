package azureblob

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

// stagedPart holds the metadata for a staged Azure block.
type stagedPart struct {
	blockID string
	size    int64
}

// upload models a block-blob multipart upload. Each StageBlock gets a
// fixed-length block ID that combines a per-upload GUID with a
// zero-padded part number; the GUID prevents cross-upload collisions
// to the same target key, the padding satisfies Azure's "all block
// IDs must be the same length within a single CommitBlockList" rule.
type upload struct {
	parent *AzureBlob
	id     string // per-upload GUID
	key    string

	mu         sync.Mutex
	parts      map[int]stagedPart // partNumber -> staged part info
	terminated bool
}

var _ bvstorage.MultipartUpload = (*upload)(nil)

func (u *upload) UploadID() string { return u.id }
func (u *upload) Key() string      { return u.key }

func (a *AzureBlob) CreateMultipart(ctx context.Context, key string, _ *bvstorage.MultipartOptions) (bvstorage.MultipartUpload, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	id, err := newUploadID()
	if err != nil {
		return nil, fmt.Errorf("azureblob: upload id: %w", err)
	}
	return &upload{
		parent: a,
		id:     id,
		key:    key,
		parts:  make(map[int]stagedPart),
	}, nil
}

func (u *upload) UploadPart(ctx context.Context, partNumber int, body io.Reader) (bvstorage.MultipartPart, error) {
	if partNumber < 1 || partNumber > 50000 {
		return bvstorage.MultipartPart{}, fmt.Errorf("%w: partNumber must be in [1,50000] (got %d)", bvstorage.ErrInvalidArgument, partNumber)
	}

	buf, err := io.ReadAll(body)
	if err != nil {
		return bvstorage.MultipartPart{}, fmt.Errorf("azureblob: read part: %w", err)
	}
	blockID := makeBlockID(u.id, partNumber)
	bb := u.parent.container.NewBlockBlobClient(applyPrefix(u.parent.cfg.Prefix, u.key))

	// Hold the mutex across the StageBlock network call. This serialises
	// concurrent UploadPart calls but prevents the Abort race: if Abort runs
	// while StageBlock is in flight the block lands on Azure with no local
	// record, leaking until Azure's 7-day GC. Holding the lock means Abort
	// blocks until StageBlock completes (or fails) before flipping terminated.
	// This matches the gcs/multipart.go pattern; conformance stress tests for
	// concurrent Complete still pass because Complete holds its own lock and
	// operates only on the already-staged parts map.
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.terminated {
		return bvstorage.MultipartPart{}, fmt.Errorf("%w: upload %s already terminated", bvstorage.ErrInvalidArgument, u.id)
	}
	_, err = bb.StageBlock(ctx, blockID, &readSeekCloser{Reader: bytes.NewReader(buf)}, nil)
	if err != nil {
		return bvstorage.MultipartPart{}, classify(opStageBlock, err)
	}
	if u.terminated {
		// Abort arrived during StageBlock; block is staged but we have no
		// record — Azure will GC it after 7 days. Do not record the part.
		return bvstorage.MultipartPart{}, fmt.Errorf("%w: upload %s terminated during stage", bvstorage.ErrInvalidArgument, u.id)
	}
	partSize := int64(len(buf))
	u.parts[partNumber] = stagedPart{blockID: blockID, size: partSize}
	return bvstorage.MultipartPart{
		PartNumber: partNumber,
		Token:      blockID,
		Size:       partSize,
	}, nil
}

func (a *AzureBlob) CompleteMultipartIfAbsent(ctx context.Context, mu bvstorage.MultipartUpload, parts []bvstorage.MultipartPart) (bvstorage.ObjectVersion, error) {
	u, ok := mu.(*upload)
	if !ok {
		return bvstorage.ObjectVersion{}, fmt.Errorf("%w: upload not produced by this adapter", bvstorage.ErrInvalidArgument)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.terminated {
		return bvstorage.ObjectVersion{}, fmt.Errorf("%w: upload %s already terminated", bvstorage.ErrInvalidArgument, u.id)
	}
	if len(parts) == 0 {
		return bvstorage.ObjectVersion{}, fmt.Errorf("%w: complete called with empty parts list", bvstorage.ErrInvalidArgument)
	}
	sorted := append([]bvstorage.MultipartPart(nil), parts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].PartNumber < sorted[j].PartNumber })

	// Validate: parts must be contiguous starting from 1.
	for i, p := range sorted {
		if p.PartNumber != i+1 {
			return bvstorage.ObjectVersion{}, fmt.Errorf("%w: parts must be contiguous starting at 1 (got part %d at position %d)", bvstorage.ErrInvalidArgument, p.PartNumber, i+1)
		}
	}

	blockIDs := make([]string, 0, len(sorted))
	seen := make(map[int]bool)
	for _, p := range sorted {
		if seen[p.PartNumber] {
			return bvstorage.ObjectVersion{}, fmt.Errorf("%w: part %d listed twice", bvstorage.ErrInvalidArgument, p.PartNumber)
		}
		seen[p.PartNumber] = true
		staged, ok := u.parts[p.PartNumber]
		if !ok {
			return bvstorage.ObjectVersion{}, fmt.Errorf("%w: part %d was never staged", bvstorage.ErrInvalidArgument, p.PartNumber)
		}
		// Validate that the caller's reported size matches what was staged.
		if p.Size != staged.size {
			return bvstorage.ObjectVersion{}, fmt.Errorf("%w: part %d size mismatch: staged %d bytes, caller reports %d bytes", bvstorage.ErrInvalidArgument, p.PartNumber, staged.size, p.Size)
		}
		blockIDs = append(blockIDs, staged.blockID)
	}

	bb := u.parent.container.NewBlockBlobClient(applyPrefix(u.parent.cfg.Prefix, u.key))
	resp, err := bb.CommitBlockList(ctx, blockIDs, &blockblob.CommitBlockListOptions{
		AccessConditions: &blob.AccessConditions{
			ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: to.Ptr(eTagAny)},
		},
	})
	if err != nil {
		return bvstorage.ObjectVersion{}, classify(opCommitIfAbsent, err)
	}
	u.terminated = true
	return versionFromETag(resp.ETag), nil
}

func (u *upload) Abort(ctx context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.terminated {
		return nil
	}
	u.terminated = true
	u.parts = nil
	// Uncommitted blocks are GC'd by Azure after 7 days; no API call
	// is needed in the abort path. If a partial commit happened (it
	// should not — we only commit once), the caller can issue a
	// conditional delete separately.
	return nil
}

// makeBlockID returns base64(guid:zeroPad(partNumber)) — fixed length
// for any single CommitBlockList call.
func makeBlockID(uploadID string, partNumber int) string {
	raw := fmt.Sprintf("%s:%010d", uploadID, partNumber)
	return base64.StdEncoding.EncodeToString([]byte(raw))
}

func newUploadID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
