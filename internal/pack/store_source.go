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

// ReadAt implements io.ReaderAt. Returns io.EOF only when the read is
// short (off+len(p) exceeds the object end). An exact-fit read that fills
// p returns (len(p), nil), per the io.ReaderAt contract.
func (s *StoreSource) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("pack: StoreSource.ReadAt: negative offset %d", off)
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= s.size {
		return 0, io.EOF
	}
	// Compute available bytes from off to end of object. Avoid
	// overflow by deriving from s.size, not off+len(p).
	avail := s.size - off
	want := len(p)
	atEOF := false
	if int64(want) > avail {
		want = int(avail)
		atEOF = true
	}
	end := off + int64(want) - 1
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
		// `want` was already clipped to the known object size, so a
		// short read here means the backend returned fewer bytes than
		// the (clipped) requested range. Surface as a hard error in
		// every case: this is corruption / truncation, not normal EOF.
		return n, fmt.Errorf("pack: StoreSource.ReadAt: short read %d/%d (atEOF=%v): %w",
			n, want, atEOF, io.ErrUnexpectedEOF)
	}
	if atEOF {
		return n, io.EOF
	}
	return n, nil
}
