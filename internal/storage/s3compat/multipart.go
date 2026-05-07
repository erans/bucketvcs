package s3compat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

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

	mu         sync.Mutex
	parts      map[int]storage.MultipartPart // partNumber -> recorded metadata
	terminated bool
}

var _ storage.MultipartUpload = (*upload)(nil)

func (u *upload) UploadID() string { return u.uploadID }
func (u *upload) Key() string      { return u.key }

func (u *upload) UploadPart(ctx context.Context, partNumber int, body io.Reader) (storage.MultipartPart, error) {
	if partNumber < 1 || partNumber > 10000 {
		return storage.MultipartPart{}, fmt.Errorf("%w: partNumber must be in [1, 10000] (got %d)", storage.ErrInvalidArgument, partNumber)
	}
	if err := u.checkActive(); err != nil {
		return storage.MultipartPart{}, err
	}
	seekable, err := materializeForRetry(body, u.parent.cfg.UploadPartSize)
	if err != nil {
		return storage.MultipartPart{}, err
	}
	size, err := readerSize(seekable)
	if err != nil {
		return storage.MultipartPart{}, fmt.Errorf("s3compat: determine part size: %w", err)
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
	part := storage.MultipartPart{
		PartNumber: partNumber,
		Token:      aws.ToString(out.ETag),
		Size:       size,
	}
	u.recordPart(part)
	return part, nil
}

func (u *upload) recordPart(p storage.MultipartPart) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.parts[p.PartNumber] = p
}

// checkActive returns ErrInvalidArgument if the upload was already
// completed or aborted. Per the storage contract, terminated uploads
// must not accept further part uploads or completions.
func (u *upload) checkActive() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.terminated {
		return fmt.Errorf("%w: upload %s has already been completed or aborted", storage.ErrInvalidArgument, u.uploadID)
	}
	return nil
}

// markTerminated flips the upload to terminated state under the
// existing mutex. Idempotent; subsequent calls do nothing.
func (u *upload) markTerminated() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.terminated = true
}

func (u *upload) Abort(ctx context.Context) error {
	_, err := u.parent.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(u.parent.cfg.Bucket),
		Key:      aws.String(u.storeKey),
		UploadId: aws.String(u.uploadID),
	})
	if err == nil {
		u.markTerminated()
		return nil
	}
	classified := classify(opAbortMultipart, err)
	// Abort is idempotent: a NoSuchUpload / 404 means the upload was
	// already aborted or completed. Treat as success.
	if errors.Is(classified, storage.ErrNotFound) {
		// Already aborted/completed — also terminal.
		u.markTerminated()
		return nil
	}
	return classified
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
		parts:    map[int]storage.MultipartPart{},
	}, nil
}

func (s *S3Compat) CompleteMultipartIfAbsent(ctx context.Context, mu storage.MultipartUpload, parts []storage.MultipartPart) (storage.ObjectVersion, error) {
	u, ok := mu.(*upload)
	if !ok {
		return storage.ObjectVersion{}, fmt.Errorf("%w: CompleteMultipartIfAbsent: upload is %T, want *s3compat.upload", storage.ErrInvalidArgument, mu)
	}
	if u.parent != s {
		return storage.ObjectVersion{}, fmt.Errorf("%w: CompleteMultipartIfAbsent: upload was created by a different S3Compat instance", storage.ErrInvalidArgument)
	}
	if err := u.checkActive(); err != nil {
		return storage.ObjectVersion{}, err
	}
	if err := validateCompleteParts(u, parts); err != nil {
		return storage.ObjectVersion{}, err
	}
	completed := make([]types.CompletedPart, 0, len(parts))
	for _, p := range parts {
		completed = append(completed, types.CompletedPart{
			ETag:       aws.String(p.Token),
			PartNumber: aws.Int32(int32(p.PartNumber)),
		})
	}
	out, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(s.cfg.Bucket),
		Key:             aws.String(u.storeKey),
		UploadId:        aws.String(u.uploadID),
		IfNoneMatch:     aws.String("*"),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	})
	if err != nil {
		// On NoSuchUpload-after-complete (a concurrent racer won), the
		// caller would otherwise see ErrNotFound which is misleading.
		// We still classify here; the terminated-state guard above
		// catches the fast path. The race window between checkActive
		// and the SDK call is narrow but possible — leave as-is and
		// document.
		return storage.ObjectVersion{}, classify(opCompleteIfAbsent, err)
	}
	u.markTerminated()
	return storage.ObjectVersion{
		Provider: "s3compat",
		Token:    aws.ToString(out.ETag),
		Kind:     storage.VersionEtag,
	}, nil
}

// readerSize returns the byte length of body. Body must be a Seeker
// (which materializeForRetry guarantees today). After this returns,
// body is rewound to its start.
func readerSize(r io.Reader) (int64, error) {
	seeker, ok := r.(io.Seeker)
	if !ok {
		return 0, fmt.Errorf("s3compat: body is not seekable")
	}
	end, err := seeker.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	if _, err := seeker.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	return end, nil
}

// validateCompleteParts checks that parts is non-empty, contiguous from 1,
// has non-empty tokens that match recorded uploads, and matches the
// recorded sizes. The latter catches callers tampering with Part.Size
// after UploadPart returned.
func validateCompleteParts(u *upload, parts []storage.MultipartPart) error {
	if len(parts) == 0 {
		return fmt.Errorf("%w: CompleteMultipartIfAbsent: parts list is empty", storage.ErrInvalidArgument)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	for i, p := range parts {
		if p.PartNumber != i+1 {
			return fmt.Errorf("%w: CompleteMultipartIfAbsent: parts must be contiguous starting at 1; parts[%d].PartNumber = %d", storage.ErrInvalidArgument, i, p.PartNumber)
		}
		if p.Token == "" {
			return fmt.Errorf("%w: CompleteMultipartIfAbsent: parts[%d].Token is empty", storage.ErrInvalidArgument, i)
		}
		recorded, ok := u.parts[p.PartNumber]
		if !ok {
			return fmt.Errorf("%w: CompleteMultipartIfAbsent: parts[%d].PartNumber %d was not uploaded", storage.ErrInvalidArgument, i, p.PartNumber)
		}
		if p.Token != recorded.Token {
			return fmt.Errorf("%w: CompleteMultipartIfAbsent: parts[%d].Token does not match uploaded part", storage.ErrInvalidArgument, i)
		}
		if p.Size != recorded.Size {
			return fmt.Errorf("%w: CompleteMultipartIfAbsent: parts[%d].Size = %d, recorded = %d", storage.ErrInvalidArgument, i, p.Size, recorded.Size)
		}
	}
	return nil
}
