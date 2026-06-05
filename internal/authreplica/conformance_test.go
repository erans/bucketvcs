package authreplica

// Conformance scenarios adapted from litestream's replica_client_test.go
// (Apache-2.0, github.com/benbjohnson/litestream) — that harness lives in
// package litestream_test and cannot be imported. Re-run this file when
// bumping the pinned litestream version.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/superfly/ltx"

	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/azureblob"
	"github.com/bucketvcs/bucketvcs/internal/storage/gcs"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
	"github.com/bucketvcs/bucketvcs/internal/storage/s3compat"
)

// eachBackend runs fn against every reachable backend. localfs always runs;
// cloud backends skip unless their conformance env vars are set (same
// convention as internal/storage/*/*_conformance_test.go).
func eachBackend(t *testing.T, fn func(t *testing.T, store storage.ObjectStore)) {
	t.Run("localfs", func(t *testing.T) {
		s, err := localfs.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		fn(t, s)
	})
	t.Run("s3compat", func(t *testing.T) {
		bucket := os.Getenv("BUCKETVCS_S3_BUCKET")
		region := os.Getenv("BUCKETVCS_S3_REGION")
		if bucket == "" || region == "" {
			t.Skip("S3 conformance: set BUCKETVCS_S3_BUCKET, BUCKETVCS_S3_REGION, AWS credentials")
		}
		s, err := s3compat.Open(context.Background(), s3compat.Config{
			Bucket:          bucket,
			Region:          region,
			Endpoint:        os.Getenv("BUCKETVCS_S3_ENDPOINT"),
			ForcePathStyle:  os.Getenv("BUCKETVCS_S3_FORCE_PATH_STYLE") == "true",
			AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
			SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
			SessionToken:    os.Getenv("AWS_SESSION_TOKEN"),
		})
		if err != nil {
			t.Fatal(err)
		}
		fn(t, s)
	})
	t.Run("gcs", func(t *testing.T) {
		bucket := os.Getenv("BUCKETVCS_GCS_BUCKET")
		if bucket == "" {
			t.Skip("BUCKETVCS_GCS_BUCKET unset — skipping live GCS conformance")
		}
		s, err := gcs.Open(context.Background(), gcs.Config{
			Bucket:          bucket,
			Endpoint:        os.Getenv("BUCKETVCS_GCS_ENDPOINT"),
			CredentialsFile: os.Getenv("BUCKETVCS_GCS_CREDENTIALS_FILE"),
		})
		if err != nil {
			t.Fatal(err)
		}
		fn(t, s)
	})
	t.Run("azureblob", func(t *testing.T) {
		cont := os.Getenv("BUCKETVCS_AZURE_CONTAINER")
		if cont == "" {
			t.Skip("BUCKETVCS_AZURE_CONTAINER unset — skipping live azureblob conformance")
		}
		s, err := azureblob.Open(context.Background(), azureblob.Config{
			Container:        cont,
			Account:          os.Getenv("BUCKETVCS_AZURE_ACCOUNT"),
			AccountKey:       os.Getenv("BUCKETVCS_AZURE_ACCOUNT_KEY"),
			ConnectionString: os.Getenv("BUCKETVCS_AZURE_CONNECTION_STRING"),
			ServiceURL:       os.Getenv("BUCKETVCS_AZURE_SERVICE_URL"),
		})
		if err != nil {
			t.Fatal(err)
		}
		fn(t, s)
	})
}

// newConformanceClient gives each test run a unique prefix so live-bucket
// runs do not collide, and cleans up after itself.
func newConformanceClient(t *testing.T, store storage.ObjectStore) *Client {
	t.Helper()
	c := NewClient(store, fmt.Sprintf("sys/authdb-conformance/%s", t.Name()))
	t.Cleanup(func() { _ = c.DeleteAll(context.Background()) })
	return c
}

func TestConformance_WriteThenList(t *testing.T) {
	eachBackend(t, func(t *testing.T, store storage.ObjectStore) {
		ctx := context.Background()
		c := newConformanceClient(t, store)
		for _, r := range [][2]ltx.TXID{{1, 1}, {2, 4}, {5, 5}} {
			if _, err := c.WriteLTXFile(ctx, 0, r[0], r[1], bytes.NewReader(ltxPayload(512))); err != nil {
				t.Fatal(err)
			}
		}
		itr, err := c.LTXFiles(ctx, 0, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		defer itr.Close()
		var n int
		var last ltx.TXID
		for itr.Next() {
			item := itr.Item()
			if item.MinTXID < last {
				t.Fatalf("iterator out of order: %d after %d", item.MinTXID, last)
			}
			last = item.MinTXID
			if item.Size != 512 {
				t.Fatalf("size mismatch: %d", item.Size)
			}
			if item.CreatedAt.IsZero() {
				t.Fatal("zero CreatedAt")
			}
			n++
		}
		if n != 3 {
			t.Fatalf("want 3 files, got %d", n)
		}
	})
}

func TestConformance_OpenMissingIsNotExist(t *testing.T) {
	eachBackend(t, func(t *testing.T, store storage.ObjectStore) {
		c := newConformanceClient(t, store)
		_, err := c.OpenLTXFile(context.Background(), 0, 1000, 1001, 0, 0)
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("want os.ErrNotExist, got %v", err)
		}
	})
}

func TestConformance_RangedRead(t *testing.T) {
	eachBackend(t, func(t *testing.T, store storage.ObjectStore) {
		ctx := context.Background()
		c := newConformanceClient(t, store)
		body := ltxPayload(2048)
		if _, err := c.WriteLTXFile(ctx, 2, 7, 9, bytes.NewReader(body)); err != nil {
			t.Fatal(err)
		}
		rc, err := c.OpenLTXFile(ctx, 2, 7, 9, 1024, 512)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if !bytes.Equal(got, body[1024:1536]) {
			t.Fatalf("range read mismatch (%d bytes)", len(got))
		}
	})
}

func TestConformance_DeleteAndDeleteAll(t *testing.T) {
	eachBackend(t, func(t *testing.T, store storage.ObjectStore) {
		ctx := context.Background()
		c := newConformanceClient(t, store)
		info, err := c.WriteLTXFile(ctx, 0, 1, 2, bytes.NewReader(ltxPayload(64)))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.DeleteLTXFiles(ctx, []*ltx.FileInfo{info}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.OpenLTXFile(ctx, 0, 1, 2, 0, 0); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("want deleted, got %v", err)
		}
		if _, err := c.WriteLTXFile(ctx, 1, 3, 4, bytes.NewReader(ltxPayload(64))); err != nil {
			t.Fatal(err)
		}
		if err := c.DeleteAll(ctx); err != nil {
			t.Fatal(err)
		}
		itr, err := c.LTXFiles(ctx, 1, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		defer itr.Close()
		if itr.Next() {
			t.Fatal("files remain after DeleteAll")
		}
	})
}

// casConflictStore wraps a real store and forces the first
// PutIfVersionMatches to fail with ErrVersionMismatch, simulating a
// concurrent writer between Head and the CAS put.
type casConflictStore struct {
	storage.ObjectStore
	conflicts int
}

func (s *casConflictStore) PutIfVersionMatches(ctx context.Context, key string, expected storage.ObjectVersion, body io.Reader, opts *storage.PutOptions) (storage.ObjectVersion, error) {
	if s.conflicts > 0 {
		s.conflicts--
		return storage.ObjectVersion{}, storage.ErrVersionMismatch
	}
	return s.ObjectStore.PutIfVersionMatches(ctx, key, expected, body, opts)
}

func TestConformance_WriteRetriesThroughCASConflict(t *testing.T) {
	ctx := context.Background()
	base, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := &casConflictStore{ObjectStore: base, conflicts: 2}
	c := NewClient(store, "sys/authdb")
	if _, err := c.WriteLTXFile(ctx, 0, 1, 2, bytes.NewReader(ltxPayload(64))); err != nil {
		t.Fatal(err)
	}
	second := ltxPayload(96)
	if _, err := c.WriteLTXFile(ctx, 0, 1, 2, bytes.NewReader(second)); err != nil {
		t.Fatalf("overwrite through CAS conflicts failed: %v", err)
	}
	rc, err := c.OpenLTXFile(ctx, 0, 1, 2, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, second) {
		t.Fatal("retried write did not win")
	}
}
