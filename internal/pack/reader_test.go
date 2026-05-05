package pack

import (
	"bytes"
	"context"
	"crypto/sha1"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestReader_OpenValidatesPackMagic(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "p.pack", strings.NewReader("garbage12345"), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := store.PutIfAbsent(context.Background(), "p.idx", strings.NewReader("garbage"), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := Open(context.Background(), store, "p.pack", "p.idx"); err == nil {
		t.Fatalf("expected Open to fail on garbage pack/idx")
	}
}

func TestReader_GetMatchesGitCatFile(t *testing.T) {
	prefix, id, bareDir := makeOnePackRepo(t)
	store := newTestStore(t)
	uploadFile(t, store, prefix+"-"+id+".pack", "p.pack")
	uploadFile(t, store, prefix+"-"+id+".idx", "p.idx")
	r, err := Open(context.Background(), store, "p.pack", "p.idx")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()

	oids, err := gitcli.RevListAllObjects(context.Background(), bareDir)
	if err != nil {
		t.Fatalf("RevListAllObjects: %v", err)
	}
	for _, oidStr := range oids {
		oid, err := ParseOID(oidStr)
		if err != nil {
			t.Fatalf("ParseOID: %v", err)
		}
		if !r.Has(oid) {
			t.Fatalf("Has(%s) = false", oidStr)
		}
		obj, err := r.Get(context.Background(), oid)
		if err != nil {
			t.Fatalf("Get(%s): %v", oidStr, err)
		}
		// Verify SHA-1 of (typeStr SP size NUL body) matches the OID.
		h := sha1.New()
		fmt.Fprintf(h, "%s %d", obj.Type.String(), obj.Size)
		h.Write([]byte{0})
		h.Write(obj.Data)
		var got OID
		copy(got[:], h.Sum(nil))
		if got != oid {
			t.Fatalf("Get hash mismatch for %s: got=%s", oidStr, got)
		}
	}
}

func TestReader_GetWithDeltaFixture(t *testing.T) {
	prefix, id, bareDir := makeDeltaPackRepo(t)
	store := newTestStore(t)
	uploadFile(t, store, prefix+"-"+id+".pack", "p.pack")
	uploadFile(t, store, prefix+"-"+id+".idx", "p.idx")
	r, err := Open(context.Background(), store, "p.pack", "p.idx")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()
	oids, err := gitcli.RevListAllObjects(context.Background(), bareDir)
	if err != nil {
		t.Fatalf("RevListAllObjects: %v", err)
	}
	for _, oidStr := range oids {
		oid, err := ParseOID(oidStr)
		if err != nil {
			t.Fatalf("ParseOID: %v", err)
		}
		obj, err := r.Get(context.Background(), oid)
		if err != nil {
			t.Fatalf("Get(%s): %v", oidStr, err)
		}
		h := sha1.New()
		fmt.Fprintf(h, "%s %d", obj.Type.String(), obj.Size)
		h.Write([]byte{0})
		h.Write(obj.Data)
		var got OID
		copy(got[:], h.Sum(nil))
		if got != oid {
			t.Fatalf("Get hash mismatch for %s", oidStr)
		}
	}
}

func TestReader_ForEach_OrderAndCount(t *testing.T) {
	prefix, id, _ := makeOnePackRepo(t)
	store := newTestStore(t)
	uploadFile(t, store, prefix+"-"+id+".pack", "p.pack")
	uploadFile(t, store, prefix+"-"+id+".idx", "p.idx")
	r, err := Open(context.Background(), store, "p.pack", "p.idx")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()
	var prev OID
	first := true
	count := 0
	if err := r.ForEach(func(oid OID, off uint64) error {
		if !first && bytes.Compare(oid[:], prev[:]) <= 0 {
			t.Fatalf("ForEach not OID-sorted")
		}
		prev = oid
		first = false
		count++
		return nil
	}); err != nil {
		t.Fatalf("ForEach: %v", err)
	}
	if count == 0 {
		t.Fatalf("ForEach saw no objects")
	}
}

func TestReader_GetMissingOID(t *testing.T) {
	prefix, id, _ := makeOnePackRepo(t)
	store := newTestStore(t)
	uploadFile(t, store, prefix+"-"+id+".pack", "p.pack")
	uploadFile(t, store, prefix+"-"+id+".idx", "p.idx")
	r, err := Open(context.Background(), store, "p.pack", "p.idx")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()
	var bogus OID
	if _, err := r.Get(context.Background(), bogus); err == nil {
		t.Fatalf("Get on missing OID should error")
	}
}

func TestReader_CacheReturnsSameDataOnHit(t *testing.T) {
	prefix, id, _ := makeOnePackRepo(t)
	store := newTestStore(t)
	uploadFile(t, store, prefix+"-"+id+".pack", "p.pack")
	uploadFile(t, store, prefix+"-"+id+".idx", "p.idx")
	r, err := Open(context.Background(), store, "p.pack", "p.idx")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()
	if r.Idx().Count() == 0 {
		t.Skip("empty fixture")
	}
	oid := r.Idx().OIDAt(0)
	a, err := r.Get(context.Background(), oid)
	if err != nil {
		t.Fatalf("Get a: %v", err)
	}
	b, err := r.Get(context.Background(), oid)
	if err != nil {
		t.Fatalf("Get b: %v", err)
	}
	if !bytes.Equal(a.Data, b.Data) {
		t.Fatalf("cache hit returned different bytes")
	}
}

func uploadFile(t *testing.T, store storage.ObjectStore, srcPath, dstKey string) {
	t.Helper()
	f, err := os.Open(srcPath)
	if err != nil {
		t.Fatalf("Open %s: %v", srcPath, err)
	}
	defer f.Close()
	if _, err := store.PutIfAbsent(context.Background(), dstKey, f, nil); err != nil {
		t.Fatalf("PutIfAbsent %s: %v", dstKey, err)
	}
}

func TestReader_Open_RejectsPackIdxMismatch(t *testing.T) {
	// Build a valid pack/idx pair, then mutate a body byte in the pack
	// without recomputing the trailer. The self-SHA-1 check should catch
	// this because SHA-1(header+mutated_body) != pack trailer.
	prefix, id, _ := makeOnePackRepo(t)
	packBytes, err := os.ReadFile(prefix + "-" + id + ".pack")
	if err != nil {
		t.Fatalf("ReadFile pack: %v", err)
	}
	idxBytes, err := os.ReadFile(prefix + "-" + id + ".idx")
	if err != nil {
		t.Fatalf("ReadFile idx: %v", err)
	}
	// Mutate a byte well inside the pack body (after the 12-byte header,
	// before the 20-byte trailer). The trailer is now stale relative to
	// the mutated body, so SHA-1(header+body) != trailer.
	if len(packBytes) < 32+20 {
		t.Skip("pack too small to mutate body safely")
	}
	tampered := append([]byte(nil), packBytes...)
	tampered[12] ^= 0xff // mutate first byte of object data
	store := newTestStore(t)
	if _, err := store.PutIfAbsent(context.Background(), "p.pack", bytes.NewReader(tampered), nil); err != nil {
		t.Fatalf("Put pack: %v", err)
	}
	if _, err := store.PutIfAbsent(context.Background(), "p.idx", bytes.NewReader(idxBytes), nil); err != nil {
		t.Fatalf("Put idx: %v", err)
	}
	if _, err := Open(context.Background(), store, "p.pack", "p.idx"); err == nil {
		t.Fatalf("expected Open to reject tampered pack")
	}
}

func TestReader_Get_RespectsCallContext(t *testing.T) {
	prefix, id, _ := makeOnePackRepo(t)
	baseStore := newTestStore(t)
	uploadFile(t, baseStore, prefix+"-"+id+".pack", "p.pack")
	uploadFile(t, baseStore, prefix+"-"+id+".idx", "p.idx")

	// Wrap the store so GetRange returns ctx.Err() when the context is done.
	// localfs doesn't check ctx on every call, so we need this shim to verify
	// that Reader.Get actually threads the caller's ctx to StoreSource.
	wrapped := &ctxCheckStore{ObjectStore: baseStore}

	r, err := Open(context.Background(), wrapped, "p.pack", "p.idx")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()
	if r.Idx().Count() == 0 {
		t.Skip("empty fixture")
	}
	oid := r.Idx().OIDAt(0)
	// Call Get with an already-canceled ctx; ctxCheckStore will surface it.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Get(ctx, oid); err == nil {
		t.Fatalf("expected error from canceled ctx")
	}
}

// ctxCheckStore wraps an ObjectStore and fails GetRange immediately when
// the context is already done. This lets tests verify ctx is threaded through.
type ctxCheckStore struct {
	storage.ObjectStore
}

func (s *ctxCheckStore) GetRange(ctx context.Context, key string, start, end int64) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.ObjectStore.GetRange(ctx, key, start, end)
}
