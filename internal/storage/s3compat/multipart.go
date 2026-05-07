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

	// Recheck terminated under lock and record the part atomically.
	// If a concurrent Complete or Abort terminated the upload while
	// our SDK call was in flight, refuse to record the part and
	// surface ErrInvalidArgument. The part may be orphaned on S3 —
	// M8 GC reclaims orphan multipart parts.
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.terminated {
		return storage.MultipartPart{}, fmt.Errorf("%w: upload %s was terminated during UploadPart", storage.ErrInvalidArgument, u.uploadID)
	}
	u.parts[partNumber] = part
	return part, nil
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
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.terminated {
		return nil // idempotent: already aborted/completed
	}
	_, err := u.parent.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(u.parent.cfg.Bucket),
		Key:      aws.String(u.storeKey),
		UploadId: aws.String(u.uploadID),
	})
	if err == nil {
		u.terminated = true
		return nil
	}
	classified := classify(opAbortMultipart, err)
	// Already aborted/completed remotely is also terminal.
	if errors.Is(classified, storage.ErrNotFound) {
		u.terminated = true
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

	// Hold the lock across the entire Complete operation (active check,
	// validation, SDK call, terminal-state update). This fully
	// serializes concurrent Complete attempts on the same upload, so a
	// loser observes the terminated flag at checkActive instead of
	// racing through to a NoSuchUpload from S3.
	//
	// Cost: blocks other goroutines holding a reference to the same
	// upload during the SDK round-trip. Complete is a rare terminal
	// operation; UploadPart still uses short-lived locks and is not
	// blocked by an in-flight Complete on a different goroutine
	// (since UploadPart's own checkActive() takes a separate brief
	// lock — note that this means an UploadPart can race a Complete:
	// they will be ordered, but if Complete wins UploadPart will see
	// terminated=true and fail; if UploadPart wins it will record a
	// part that Complete then sees in validateCompletePartsLocked
	// before the SDK call). Documented behavior.
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.terminated {
		return storage.ObjectVersion{}, fmt.Errorf("%w: upload %s has already been completed or aborted", storage.ErrInvalidArgument, u.uploadID)
	}
	if err := validateCompletePartsLocked(u, parts); err != nil {
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
		return storage.ObjectVersion{}, classify(opCompleteIfAbsent, err)
	}
	u.terminated = true // already locked above; no need for markTerminated.
	return storage.ObjectVersion{
		Provider: "s3compat",
		Token:    aws.ToString(out.ETag),
		Kind:     storage.VersionEtag,
	}, nil
}

// readerSize returns the byte length remaining in body from its
// current position. body must be a Seeker (which materializeForRetry
// guarantees today). After this returns, body is rewound to the
// position it was at when called — NOT to absolute zero, so callers
// who pass a partially-consumed ReadSeeker get the expected slice
// uploaded.
func readerSize(r io.Reader) (int64, error) {
	seeker, ok := r.(io.Seeker)
	if !ok {
		return 0, fmt.Errorf("s3compat: body is not seekable")
	}
	start, err := seeker.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	end, err := seeker.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	if _, err := seeker.Seek(start, io.SeekStart); err != nil {
		return 0, err
	}
	return end - start, nil
}

// validateCompletePartsLocked is the helper used by CompleteMultipartIfAbsent
// while holding u.mu. It assumes the caller holds the lock and checks
// the parts list against the recorded uploads.
func validateCompletePartsLocked(u *upload, parts []storage.MultipartPart) error {
	if len(parts) == 0 {
		return fmt.Errorf("%w: CompleteMultipartIfAbsent: parts list is empty", storage.ErrInvalidArgument)
	}
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
