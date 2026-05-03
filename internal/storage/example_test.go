package storage_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// Example_localfsLifecycle demonstrates the full ObjectStore contract on
// localfs: Put, Get, conditional update, conflict detection, multipart,
// list, and delete.
func Example_localfsLifecycle() {
	dir, err := os.MkdirTemp("", "bucketvcs-example-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	s, err := localfs.Open(filepath.Join(dir, "bucket"))
	if err != nil {
		panic(err)
	}
	defer s.Close()

	ctx := context.Background()
	key := "tenants/t1/repos/r1/manifest/root.json"

	// 1. Create-only PUT.
	v0, err := s.PutIfAbsent(ctx, key, bytes.NewReader([]byte(`{"version":1}`)), nil)
	if err != nil {
		panic(err)
	}
	fmt.Println("created v0")

	// 2. Read-after-write.
	obj, err := s.Get(ctx, key, nil)
	if err != nil {
		panic(err)
	}
	body, _ := io.ReadAll(obj.Body)
	_ = obj.Body.Close()
	fmt.Printf("read: %s\n", body)

	// 3. Conditional update against current version.
	v1, err := s.PutIfVersionMatches(ctx, key, v0, bytes.NewReader([]byte(`{"version":2}`)), nil)
	if err != nil {
		panic(err)
	}
	fmt.Println("updated v0 -> v1")

	// 4. Stale CAS rejected.
	_, err = s.PutIfVersionMatches(ctx, key, v0, bytes.NewReader([]byte(`{"version":3}`)), nil)
	if errors.Is(err, storage.ErrVersionMismatch) {
		fmt.Println("stale CAS rejected with ErrVersionMismatch")
	}

	// 5. Multipart upload to a different key.
	mp, err := s.CreateMultipart(ctx, "tenants/t1/repos/r1/packs/canonical/sha256-pack.pack", nil)
	if err != nil {
		panic(err)
	}
	p1, _ := mp.UploadPart(ctx, 1, bytes.NewReader([]byte("part-1")))
	p2, _ := mp.UploadPart(ctx, 2, bytes.NewReader([]byte("part-2")))
	if _, err := s.CompleteMultipartIfAbsent(ctx, mp, []storage.MultipartPart{p1, p2}); err != nil {
		panic(err)
	}
	fmt.Println("multipart pack assembled")

	// 6. Listing.
	page, err := s.List(ctx, "tenants/t1/repos/r1/", &storage.ListOptions{MaxKeys: 100})
	if err != nil {
		panic(err)
	}
	fmt.Printf("listed %d objects\n", len(page.Objects))

	// 7. Conditional delete.
	if err := s.DeleteIfVersionMatches(ctx, key, v1); err != nil {
		panic(err)
	}
	fmt.Println("deleted manifest at v1")

	// Output:
	// created v0
	// read: {"version":1}
	// updated v0 -> v1
	// stale CAS rejected with ErrVersionMismatch
	// multipart pack assembled
	// listed 2 objects
	// deleted manifest at v1
}
