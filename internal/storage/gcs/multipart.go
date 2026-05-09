package gcs

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"sync"

	gstorage "cloud.google.com/go/storage"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

// upload is the GCS-backed MultipartUpload. Because GCS resumable
// uploads are streamed sequentially through a single Writer, we buffer
// parts in memory by part number and flush them in order on Complete.
// This trades memory for the part-out-of-order property the
// storage.MultipartUpload contract requires.
type upload struct {
	parent *GCS
	id     string
	key    string

	mu         sync.Mutex
	parts      map[int][]byte
	terminated bool
}

var _ bvstorage.MultipartUpload = (*upload)(nil)

func (u *upload) UploadID() string { return u.id }
func (u *upload) Key() string      { return u.key }

func (g *GCS) CreateMultipart(ctx context.Context, key string, opts *bvstorage.MultipartOptions) (bvstorage.MultipartUpload, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	id, err := newUploadID()
	if err != nil {
		return nil, fmt.Errorf("gcs: create upload id: %w", err)
	}
	return &upload{
		parent: g,
		id:     id,
		key:    key,
		parts:  make(map[int][]byte),
	}, nil
}

func (u *upload) UploadPart(ctx context.Context, partNumber int, body io.Reader) (bvstorage.MultipartPart, error) {
	if partNumber < 1 {
		return bvstorage.MultipartPart{}, fmt.Errorf("%w: partNumber must be >= 1 (got %d)", bvstorage.ErrInvalidArgument, partNumber)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.terminated {
		return bvstorage.MultipartPart{}, fmt.Errorf("%w: upload %s already terminated", bvstorage.ErrInvalidArgument, u.id)
	}
	buf, err := io.ReadAll(body)
	if err != nil {
		return bvstorage.MultipartPart{}, fmt.Errorf("gcs: buffer part: %w", err)
	}
	u.parts[partNumber] = buf
	return bvstorage.MultipartPart{
		PartNumber: partNumber,
		Token:      fmt.Sprintf("%d", partNumber), // ordering only
		Size:       int64(len(buf)),
	}, nil
}

func (g *GCS) CompleteMultipartIfAbsent(ctx context.Context, mu bvstorage.MultipartUpload, parts []bvstorage.MultipartPart) (bvstorage.ObjectVersion, error) {
	u, ok := mu.(*upload)
	if !ok {
		return bvstorage.ObjectVersion{}, fmt.Errorf("%w: upload not produced by this adapter", bvstorage.ErrInvalidArgument)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.terminated {
		return bvstorage.ObjectVersion{}, fmt.Errorf("%w: upload %s already terminated", bvstorage.ErrInvalidArgument, u.id)
	}
	if err := validatePartList(parts, u.parts); err != nil {
		return bvstorage.ObjectVersion{}, err
	}
	// Flush parts in order through a single resumable Writer that
	// finalizes with ifGenerationMatch=0. This gives the §29 #8
	// "multipart cannot overwrite" invariant on real GCS.
	//
	// fake-gcs-server does not enforce ifGenerationMatch=0 on resumable
	// uploads, so we perform an explicit existence check via Attrs first.
	// On real GCS the server-side condition is the authoritative guard;
	// the Attrs call here is a best-effort pre-check only.
	fullKey := applyPrefix(g.cfg.Prefix, u.key)
	if _, err := g.bucket.Object(fullKey).Attrs(ctx); err == nil {
		// Object already exists.
		return bvstorage.ObjectVersion{}, fmt.Errorf("gcs: %w: object already exists", bvstorage.ErrAlreadyExists)
	}
	obj := g.bucket.Object(fullKey).
		If(gstorage.Conditions{DoesNotExist: true})
	w := obj.NewWriter(ctx)
	w.ChunkSize = g.cfg.UploadChunkSize

	sorted := make([]int, 0, len(parts))
	for _, p := range parts {
		sorted = append(sorted, p.PartNumber)
	}
	sort.Ints(sorted)

	for _, pn := range sorted {
		if _, err := io.Copy(w, bytes.NewReader(u.parts[pn])); err != nil {
			_ = w.Close()
			return bvstorage.ObjectVersion{}, classify(opCompleteIfAbsent, err)
		}
	}
	if err := w.Close(); err != nil {
		return bvstorage.ObjectVersion{}, classify(opCompleteIfAbsent, err)
	}
	u.terminated = true
	return versionFromGen(w.Attrs().Generation), nil
}

func (u *upload) Abort(ctx context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.terminated {
		return nil
	}
	u.parts = nil
	u.terminated = true
	return nil
}

// validatePartList verifies every requested part has been buffered, the
// part numbers are contiguous starting from 1, and the declared sizes
// match the buffered content. Detects: empty lists, gaps (1,3 missing
// 2), duplicates, unknown part numbers, and size mismatches.
func validatePartList(want []bvstorage.MultipartPart, have map[int][]byte) error {
	if len(want) == 0 {
		return fmt.Errorf("%w: complete called with empty parts list", bvstorage.ErrInvalidArgument)
	}
	seen := make(map[int]bool, len(want))
	for _, p := range want {
		if _, ok := have[p.PartNumber]; !ok {
			return fmt.Errorf("%w: part %d was never uploaded", bvstorage.ErrInvalidArgument, p.PartNumber)
		}
		if seen[p.PartNumber] {
			return fmt.Errorf("%w: part %d listed twice", bvstorage.ErrInvalidArgument, p.PartNumber)
		}
		seen[p.PartNumber] = true
		// Verify declared size matches buffered content.
		if p.Size != int64(len(have[p.PartNumber])) {
			return fmt.Errorf("%w: part %d declared size %d but buffered %d bytes", bvstorage.ErrInvalidArgument, p.PartNumber, p.Size, len(have[p.PartNumber]))
		}
	}
	// Check that part numbers form a contiguous range [1, len(want)].
	for i := 1; i <= len(want); i++ {
		if !seen[i] {
			return fmt.Errorf("%w: non-contiguous part numbers: part %d missing", bvstorage.ErrInvalidArgument, i)
		}
	}
	return nil
}

// newUploadID returns a hex random identifier.
func newUploadID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
