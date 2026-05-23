package maintenance

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// localFilePackStore is a minimal read-only ObjectStore that backs only
// two known keys ("p.pack" / "p.idx") with files on local disk. It
// exists so buildIndexesLocal can drive pack.Reader against the local
// pack without uploading first.
type localFilePackStore struct {
	packPath, idxPath string
}

func newLocalFilePackStore(packPath, idxPath string) (*localFilePackStore, error) {
	for _, p := range []string{packPath, idxPath} {
		if _, err := os.Stat(p); err != nil {
			return nil, err
		}
	}
	return &localFilePackStore{packPath: packPath, idxPath: idxPath}, nil
}

func (s *localFilePackStore) pathFor(key string) (string, error) {
	switch key {
	case "p.pack":
		return s.packPath, nil
	case "p.idx":
		return s.idxPath, nil
	}
	return "", fmt.Errorf("localFilePackStore: unknown key %q", key)
}

func (s *localFilePackStore) Name() string { return "localfile-test" }

func (s *localFilePackStore) Capabilities() storage.Capabilities {
	return storage.Capabilities{}
}

// Get returns the underlying file as the response body. opts is
// intentionally ignored: this adapter is only used by maintenance
// internally with the two fixed keys "p.pack" / "p.idx" and never
// needs IfVersionMatches semantics (local files have no version
// token). Callers wanting range reads use GetRange directly.
func (s *localFilePackStore) Get(ctx context.Context, key string, opts *storage.GetOptions) (*storage.Object, error) {
	p, err := s.pathFor(key)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	return &storage.Object{
		Body:     f,
		Metadata: storage.ObjectMetadata{Key: key, Size: st.Size()},
	}, nil
}

func (s *localFilePackStore) Head(ctx context.Context, key string) (*storage.ObjectMetadata, error) {
	p, err := s.pathFor(key)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	return &storage.ObjectMetadata{Key: key, Size: st.Size()}, nil
}

func (s *localFilePackStore) GetRange(ctx context.Context, key string, start, endInclusive int64) (io.ReadCloser, error) {
	p, err := s.pathFor(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return &lengthReader{f: f, remaining: endInclusive - start + 1}, nil
}

func (s *localFilePackStore) PutIfAbsent(ctx context.Context, key string, body io.Reader, opts *storage.PutOptions) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, fmt.Errorf("localFilePackStore: read-only")
}

func (s *localFilePackStore) PutIfVersionMatches(ctx context.Context, key string, expected storage.ObjectVersion, body io.Reader, opts *storage.PutOptions) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, fmt.Errorf("localFilePackStore: read-only")
}

func (s *localFilePackStore) DeleteIfVersionMatches(ctx context.Context, key string, expected storage.ObjectVersion) error {
	return fmt.Errorf("localFilePackStore: read-only")
}

func (s *localFilePackStore) List(ctx context.Context, prefix string, opts *storage.ListOptions) (*storage.ListPage, error) {
	return nil, fmt.Errorf("localFilePackStore: list not supported")
}

func (s *localFilePackStore) CreateMultipart(ctx context.Context, key string, opts *storage.MultipartOptions) (storage.MultipartUpload, error) {
	return nil, fmt.Errorf("localFilePackStore: multipart not supported")
}

func (s *localFilePackStore) CompleteMultipartIfAbsent(ctx context.Context, upload storage.MultipartUpload, parts []storage.MultipartPart) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, fmt.Errorf("localFilePackStore: multipart not supported")
}

func (s *localFilePackStore) SignedGetURL(ctx context.Context, key string, opts storage.SignedURLOptions) (string, http.Header, error) {
	return "", nil, fmt.Errorf("localFilePackStore: signed URLs not supported")
}

type lengthReader struct {
	f         *os.File
	remaining int64
}

func (lr *lengthReader) Read(p []byte) (int, error) {
	if lr.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > lr.remaining {
		p = p[:lr.remaining]
	}
	n, err := lr.f.Read(p)
	lr.remaining -= int64(n)
	return n, err
}

func (lr *lengthReader) Close() error { return lr.f.Close() }
