// Package localfs implements storage.ObjectStore over a regular local
// filesystem. It is intended for development, tests, and small
// self-hosted deployments. Localfs is single-process: holding two open
// Localfs instances against the same root directory in different
// processes is undefined.
package localfs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// ErrAlreadyLocked is returned by Open when another Localfs instance
// (in this process) already holds the root.
var ErrAlreadyLocked = errors.New("localfs: root is already locked by another instance")

const (
	objectsDir = "objects"
	uploadsDir = "uploads"
	lockFile   = ".lock"
	metaSuffix = ".meta"
)

// Localfs is the local-filesystem ObjectStore implementation.
type Localfs struct {
	root            string
	lockPath        string
	lock            *os.File
	lockfileRemoved bool
	mutexes         *keyedMutex
}

// Compile-time assertion that *Localfs satisfies storage.ObjectStore.
var _ storage.ObjectStore = (*Localfs)(nil)

// Open returns a Localfs rooted at the given directory. The directory
// is created if missing. Open holds a process-wide lockfile at
// <root>/.lock; a second Open against the same root returns
// ErrAlreadyLocked.
//
// Initialization order is deliberate to keep mutual exclusion strict:
// only the root directory is created before the O_CREATE|O_EXCL
// lockfile acquisition. Subdirectories (objects/, uploads/) are
// created afterwards while holding the lock, so a second concurrent
// Open is refused before it can mutate the bucket.
func Open(root string) (*Localfs, error) {
	if root == "" {
		return nil, errors.New("localfs: root must be non-empty")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("localfs: resolve root path: %w", err)
	}
	if err := os.MkdirAll(absRoot, 0o755); err != nil {
		return nil, fmt.Errorf("localfs: mkdir root: %w", err)
	}

	lockPath := filepath.Join(absRoot, lockFile)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, ErrAlreadyLocked
		}
		return nil, fmt.Errorf("localfs: create lockfile: %w", err)
	}
	// Write lockfile content per AD12: {pid, host, acquired_at}.
	// Verify uses pid + host for the liveness check; acquired_at is
	// forensics-only and is NOT consulted by the liveness logic.
	host, _ := os.Hostname()
	acquiredAt := time.Now().UTC()
	lockContent, err := json.Marshal(lockfileContent{
		PID:        os.Getpid(),
		Host:       host,
		AcquiredAt: acquiredAt,
	})
	if err != nil {
		_ = f.Close()
		_ = os.Remove(lockPath)
		return nil, fmt.Errorf("localfs: encode lockfile: %w", err)
	}
	if _, err := f.Write(lockContent); err != nil {
		_ = f.Close()
		_ = os.Remove(lockPath)
		return nil, fmt.Errorf("localfs: write lockfile: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(lockPath)
		return nil, fmt.Errorf("localfs: sync lockfile: %w", err)
	}

	// We hold the lock now: safe to create subdirectories.
	if err := os.MkdirAll(filepath.Join(absRoot, objectsDir), 0o755); err != nil {
		_ = f.Close()
		_ = os.Remove(lockPath)
		return nil, fmt.Errorf("localfs: mkdir objects: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(absRoot, uploadsDir), 0o755); err != nil {
		_ = f.Close()
		_ = os.Remove(lockPath)
		return nil, fmt.Errorf("localfs: mkdir uploads: %w", err)
	}

	return &Localfs{
		root:     absRoot,
		lockPath: lockPath,
		lock:     f,
		mutexes:  newKeyedMutex(),
	}, nil
}

// lockfileContent is the JSON payload of <root>/.lock. Per AD12,
// `pid` and `host` are used by Verify's liveness check;
// `acquired_at` is forensics-only (logging, debugging) and is NOT
// consulted by the liveness logic.
type lockfileContent struct {
	PID        int       `json:"pid"`
	Host       string    `json:"host"`
	AcquiredAt time.Time `json:"acquired_at"`
}

// Close releases the lockfile. If either closing the file handle or
// removing the on-disk lockfile fails, Close leaves enough state on
// the receiver that a subsequent Close call retries the failed step.
// Callers MUST keep calling Close until it returns nil; otherwise a
// stranded lockfile blocks future Open calls.
//
// Close removes the absolute lockfile path captured at Open time, not
// a path recomputed from l.root, so an os.Chdir between Open and
// Close cannot redirect the unlink at a different file.
//
// Recovery from a process crash (no Close ran at all) is the job of
// the package-level Verify with WithForce(true) — see verify.go.
func (l *Localfs) Close() error {
	if l.lockfileRemoved {
		return nil
	}
	if l.lock != nil {
		if err := l.lock.Close(); err != nil {
			return fmt.Errorf("localfs: close lockfile handle: %w", err)
		}
		l.lock = nil
	}
	if err := os.Remove(l.lockPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("localfs: remove lockfile: %w", err)
		}
	}
	l.lockfileRemoved = true
	return nil
}

func (l *Localfs) Capabilities() storage.Capabilities {
	return storage.Capabilities{
		SignedURLs:           false,
		MultipartMinPartSize: 0,
		MultipartMaxParts:    0,
		MaxObjectSize:        0,
		StrongList:           true,
	}
}

func (l *Localfs) Get(ctx context.Context, key string, opts *storage.GetOptions) (*storage.Object, error) {
	return nil, storage.ErrNotSupported
}

func (l *Localfs) Head(ctx context.Context, key string) (*storage.ObjectMetadata, error) {
	return nil, storage.ErrNotSupported
}

func (l *Localfs) GetRange(ctx context.Context, key string, start, endInclusive int64) (io.ReadCloser, error) {
	return nil, storage.ErrNotSupported
}

func (l *Localfs) PutIfAbsent(ctx context.Context, key string, body io.Reader, opts *storage.PutOptions) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, storage.ErrNotSupported
}

func (l *Localfs) PutIfVersionMatches(ctx context.Context, key string, expected storage.ObjectVersion, body io.Reader, opts *storage.PutOptions) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, storage.ErrNotSupported
}

func (l *Localfs) DeleteIfVersionMatches(ctx context.Context, key string, expected storage.ObjectVersion) error {
	return storage.ErrNotSupported
}

func (l *Localfs) List(ctx context.Context, prefix string, opts *storage.ListOptions) (*storage.ListPage, error) {
	return nil, storage.ErrNotSupported
}

func (l *Localfs) CreateMultipart(ctx context.Context, key string, opts *storage.MultipartOptions) (storage.MultipartUpload, error) {
	return nil, storage.ErrNotSupported
}

func (l *Localfs) CompleteMultipartIfAbsent(ctx context.Context, upload storage.MultipartUpload, parts []storage.MultipartPart) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, storage.ErrNotSupported
}

func (l *Localfs) SignedGetURL(ctx context.Context, key string, opts storage.SignedURLOptions) (string, error) {
	return "", storage.ErrNotSupported
}
