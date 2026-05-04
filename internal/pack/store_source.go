package pack

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// StoreSource adapts a storage.ObjectStore object into io.ReaderAt by
// translating each ReadAt into a GetRange. It is safe for concurrent
// use; each ReadAt issues its own GetRange.
//
// The ctx is captured at construction rather than passed per-call because
// io.ReaderAt has no context parameter. This is intentional: pack.Reader
// operations are scoped to a single Reader's lifetime, so the Reader's
// construction context naturally bounds all downstream I/O.
type StoreSource struct {
	ctx   context.Context //nolint:containedctx // intentional, see doc above
	store storage.ObjectStore
	key   string
	size  int64
}

// NewStoreSource constructs a StoreSource. size must equal the object's
// content length so EOF semantics are correct.
func NewStoreSource(ctx context.Context, store storage.ObjectStore, key string, size int64) *StoreSource {
	return &StoreSource{ctx: ctx, store: store, key: key, size: size}
}

// Size returns the object's known content length.
func (s *StoreSource) Size() int64 { return s.size }

// ReadAt implements io.ReaderAt. Returns io.EOF when off+len(p) reaches
// or exceeds the object's end, per the io.ReaderAt contract.
func (s *StoreSource) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("pack: StoreSource.ReadAt: negative offset %d", off)
	}
	if off >= s.size {
		return 0, io.EOF
	}
	end := off + int64(len(p)) - 1
	want := len(p)
	atEOF := false
	if end >= s.size {
		end = s.size - 1
		want = int(end - off + 1)
		atEOF = true
	}
	rc, err := s.store.GetRange(s.ctx, s.key, off, end)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return 0, fmt.Errorf("pack: StoreSource.ReadAt: %w", err)
		}
		return 0, fmt.Errorf("pack: StoreSource.ReadAt: GetRange: %w", err)
	}
	defer rc.Close()
	n, err := io.ReadFull(rc, p[:want])
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return n, fmt.Errorf("pack: StoreSource.ReadAt: ReadFull: %w", err)
	}
	if n < want {
		// Short read: storage backend returned fewer bytes than the
		// requested closed-interval range. Surface as a hard error
		// regardless of EOF status; io.ReaderAt forbids n < len(p)
		// without an error explaining it.
		if atEOF {
			return n, io.EOF
		}
		return n, fmt.Errorf("pack: StoreSource.ReadAt: short read %d/%d", n, want)
	}
	if atEOF {
		return n, io.EOF
	}
	return n, nil
}
