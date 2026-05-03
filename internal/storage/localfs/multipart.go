package localfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/google/uuid"
)

// uploadManifest is the JSON record persisted in the upload directory so
// CompleteMultipartIfAbsent can validate the target key was the same one
// the caller used at CreateMultipart time.
type uploadManifest struct {
	Version     int       `json:"version"`
	UploadID    string    `json:"upload_id"`
	Key         string    `json:"key"`
	ContentType string    `json:"content_type"`
	CreatedAt   time.Time `json:"created_at"`
}

const (
	uploadManifestVersion = 1
	uploadManifestName    = "manifest.json"
	uploadPartsDirName    = "parts"
)

// localfsUpload is the MultipartUpload returned by Localfs.CreateMultipart.
//
// Lifecycle: created live; Abort or successful Complete sets terminated.
// Once terminated, further UploadPart and Complete calls return
// ErrInvalidArgument so a stale handle cannot resurrect a finished
// upload via UploadPart's MkdirAll. The on-disk manifest is the
// cross-process witness — missing manifest also fails active-state
// checks.
type localfsUpload struct {
	parent      *Localfs
	uploadID    string
	key         string
	contentType string
	dir         string // <root>/uploads/<id>
	terminated  atomic.Bool
}

func (u *localfsUpload) UploadID() string { return u.uploadID }
func (u *localfsUpload) Key() string      { return u.key }

// validateActive returns an error if the upload cannot accept further
// part uploads or completion: the parent Localfs is closed, the
// in-memory terminated flag is set, or the on-disk manifest is missing
// (e.g., another instance Aborted, or the uploads/ tree was tampered
// with). UploadPart and CompleteMultipartIfAbsent gate on this so a
// stale handle held after Abort/Complete cannot drive new I/O.
func (u *localfsUpload) validateActive() error {
	if err := u.parent.checkOpen(); err != nil {
		return err
	}
	if u.terminated.Load() {
		return fmt.Errorf("%w: upload %s already terminated", storage.ErrInvalidArgument, u.uploadID)
	}
	if _, err := os.Stat(filepath.Join(u.dir, uploadManifestName)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: upload manifest missing (upload aborted or never created)", storage.ErrInvalidArgument)
		}
		return err
	}
	return nil
}

func (u *localfsUpload) UploadPart(ctx context.Context, partNumber int, body io.Reader) (storage.MultipartPart, error) {
	if err := u.validateActive(); err != nil {
		return storage.MultipartPart{}, err
	}
	if partNumber < 1 {
		return storage.MultipartPart{}, fmt.Errorf("%w: partNumber must be >= 1", storage.ErrInvalidArgument)
	}
	partsDir := filepath.Join(u.dir, uploadPartsDirName)
	// The parts directory is created at CreateMultipart time. If it is
	// gone we treat the upload as terminated rather than recreating it,
	// so a stale handle cannot drive new I/O after Abort/Complete.
	if _, err := os.Stat(partsDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return storage.MultipartPart{}, fmt.Errorf("%w: upload parts directory missing (upload terminated)", storage.ErrInvalidArgument)
		}
		return storage.MultipartPart{}, err
	}
	partPath := filepath.Join(partsDir, fmt.Sprintf("%05d", partNumber))
	tmp, err := os.CreateTemp(partsDir, fmt.Sprintf(".%05d.tmp.*", partNumber))
	if err != nil {
		return storage.MultipartPart{}, err
	}
	tmpName := tmp.Name()
	h := sha256.New()
	tee := io.TeeReader(body, h)
	n, err := io.Copy(tmp, tee)
	if err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return storage.MultipartPart{}, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return storage.MultipartPart{}, err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return storage.MultipartPart{}, err
	}
	if err := os.Rename(tmpName, partPath); err != nil {
		_ = os.Remove(tmpName)
		return storage.MultipartPart{}, err
	}
	return storage.MultipartPart{
		PartNumber: partNumber,
		Token:      hex.EncodeToString(h.Sum(nil)),
		Size:       n,
	}, nil
}

func (u *localfsUpload) Abort(ctx context.Context) error {
	if err := u.parent.checkOpen(); err != nil {
		return err
	}
	// Set terminated *before* RemoveAll so a concurrent UploadPart
	// observes the terminated state even if it loses the race against
	// the directory removal.
	u.terminated.Store(true)
	return os.RemoveAll(u.dir)
}

// CreateMultipart begins a multipart upload, creating the upload
// directory and writing its manifest.
func (l *Localfs) CreateMultipart(ctx context.Context, key string, opts *storage.MultipartOptions) (storage.MultipartUpload, error) {
	if err := l.checkOpen(); err != nil {
		return nil, err
	}
	if err := validateKey(key); err != nil {
		return nil, err
	}
	id := uuid.NewString()
	dir := filepath.Join(l.root, uploadsDir, id)
	if err := os.MkdirAll(filepath.Join(dir, uploadPartsDirName), 0o755); err != nil {
		return nil, err
	}
	contentType := ""
	if opts != nil {
		contentType = opts.ContentType
	}
	manifest := uploadManifest{
		Version:     uploadManifestVersion,
		UploadID:    id,
		Key:         key,
		ContentType: contentType,
		CreatedAt:   time.Now().UTC(),
	}
	mb, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(filepath.Join(dir, uploadManifestName), mb); err != nil {
		return nil, err
	}
	return &localfsUpload{
		parent:      l,
		uploadID:    id,
		key:         key,
		contentType: contentType,
		dir:         dir,
	}, nil
}

// CompleteMultipartIfAbsent assembles parts in order and atomically
// promotes them to the target key, only if the target does not already
// exist. Returns ErrAlreadyExists otherwise. Each part is hashed during
// reassembly and its hash is compared against the MultipartPart.Token
// the caller supplied; mismatch returns ErrInvalidArgument and leaves
// the target key untouched.
func (l *Localfs) CompleteMultipartIfAbsent(ctx context.Context, upload storage.MultipartUpload, parts []storage.MultipartPart) (storage.ObjectVersion, error) {
	if err := l.checkOpen(); err != nil {
		return storage.ObjectVersion{}, err
	}
	u, ok := upload.(*localfsUpload)
	if !ok {
		return storage.ObjectVersion{}, fmt.Errorf("%w: upload not from this adapter", storage.ErrInvalidArgument)
	}
	if u.parent != l {
		return storage.ObjectVersion{}, fmt.Errorf("%w: upload not from this Localfs instance", storage.ErrInvalidArgument)
	}
	if err := u.validateActive(); err != nil {
		return storage.ObjectVersion{}, err
	}
	if len(parts) == 0 {
		return storage.ObjectVersion{}, fmt.Errorf("%w: no parts", storage.ErrInvalidArgument)
	}
	for i, p := range parts {
		if p.PartNumber != i+1 {
			return storage.ObjectVersion{}, fmt.Errorf("%w: parts not contiguously numbered (parts[%d].PartNumber=%d)", storage.ErrInvalidArgument, i, p.PartNumber)
		}
		if p.Token == "" {
			return storage.ObjectVersion{}, fmt.Errorf("%w: parts[%d] has empty Token (must round-trip the value returned by UploadPart)", storage.ErrInvalidArgument, i)
		}
	}

	l.mutexes.lock(u.key)
	defer l.mutexes.unlock(u.key)

	if _, err := os.Stat(l.objectPath(u.key)); err == nil {
		return storage.ObjectVersion{}, storage.ErrAlreadyExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return storage.ObjectVersion{}, err
	}

	objPath := l.objectPath(u.key)
	if err := os.MkdirAll(filepath.Dir(objPath), 0o755); err != nil {
		return storage.ObjectVersion{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(objPath), "."+filepath.Base(objPath)+".tmp.*")
	if err != nil {
		return storage.ObjectVersion{}, err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	h := sha256.New()
	var total int64
	partsDir := filepath.Join(u.dir, uploadPartsDirName)
	for _, p := range parts {
		partPath := filepath.Join(partsDir, fmt.Sprintf("%05d", p.PartNumber))
		f, err := os.Open(partPath)
		if err != nil {
			_ = tmp.Close()
			cleanup()
			return storage.ObjectVersion{}, err
		}
		partHash := sha256.New()
		tee := io.TeeReader(f, io.MultiWriter(h, partHash))
		n, err := io.Copy(tmp, tee)
		_ = f.Close()
		if err != nil {
			_ = tmp.Close()
			cleanup()
			return storage.ObjectVersion{}, err
		}
		if n != p.Size {
			_ = tmp.Close()
			cleanup()
			return storage.ObjectVersion{}, fmt.Errorf("%w: part %d size mismatch (manifest=%d, on-disk=%d)", storage.ErrInvalidArgument, p.PartNumber, p.Size, n)
		}
		actualToken := hex.EncodeToString(partHash.Sum(nil))
		if actualToken != p.Token {
			_ = tmp.Close()
			cleanup()
			return storage.ObjectVersion{}, fmt.Errorf("%w: part %d token mismatch (caller=%q, on-disk=%q)", storage.ErrInvalidArgument, p.PartNumber, p.Token, actualToken)
		}
		total += n
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return storage.ObjectVersion{}, err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return storage.ObjectVersion{}, err
	}
	if err := os.Rename(tmpName, objPath); err != nil {
		cleanup()
		return storage.ObjectVersion{}, err
	}
	if err := fsyncDir(filepath.Dir(objPath)); err != nil {
		return storage.ObjectVersion{}, err
	}

	sum := hex.EncodeToString(h.Sum(nil))
	sc := newSidecar(sum, total, u.contentType, time.Now().UTC())
	scBytes, err := encodeSidecar(sc)
	if err != nil {
		return storage.ObjectVersion{}, err
	}
	if err := writeFileAtomic(l.metaPath(u.key), scBytes); err != nil {
		return storage.ObjectVersion{}, err
	}

	// Mark the upload terminated *before* removing the on-disk dir so a
	// concurrent UploadPart sees the in-memory flag even if it races the
	// directory removal.
	u.terminated.Store(true)
	if err := os.RemoveAll(u.dir); err != nil {
		// Non-fatal: the object is committed; the upload dir leak is a
		// gc concern, not a correctness one.
		_ = err
	}

	return storage.ObjectVersion{
		Provider: "localfs",
		Token:    sum,
		Kind:     storage.VersionEtag,
	}, nil
}
