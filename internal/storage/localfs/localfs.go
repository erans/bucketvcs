// Package localfs implements storage.ObjectStore over a regular local
// filesystem. It is intended for development, tests, and small
// self-hosted deployments. Localfs is single-process: holding two open
// Localfs instances against the same root directory in different
// processes is undefined.
package localfs

import (
	"context"
	"errors"
	"io"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Localfs is the local-filesystem ObjectStore implementation.
type Localfs struct {
	root string
}

// Compile-time assertion that *Localfs satisfies storage.ObjectStore.
var _ storage.ObjectStore = (*Localfs)(nil)

// Open returns a Localfs rooted at the given directory. This stub
// performs no on-disk validation beyond rejecting an empty root and
// returns ErrNotSupported on every method, so the package compiles
// and the conformance suite has a target to fail against. Real root
// validation (directory exists, is a directory, lock file acquired)
// lands in Task 13.
func Open(root string) (*Localfs, error) {
	if root == "" {
		return nil, errors.New("localfs: root must be non-empty")
	}
	return &Localfs{root: root}, nil
}

// Close releases any resources held by the Localfs instance.
func (l *Localfs) Close() error {
	return nil
}

func (l *Localfs) Capabilities() storage.Capabilities {
	return storage.Capabilities{}
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
