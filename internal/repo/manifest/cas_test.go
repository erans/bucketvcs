package manifest_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func newStore(t *testing.T) storage.ObjectStore {
	t.Helper()
	s, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestReadRoot_NotFound(t *testing.T) {
	s := newStore(t)
	_, _, _, err := manifest.ReadRoot(context.Background(), s, "tenants/a/repos/b/manifest/root.json")
	if !errors.Is(err, repo.ErrRepoNotFound) {
		t.Errorf("want ErrRepoNotFound, got %v", err)
	}
}

func TestReadRootAndCASRoot_RoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	key := "tenants/a/repos/b/manifest/root.json"

	header := manifest.RootHeader{
		SchemaVersion:    1,
		MinReaderVersion: "0.1.0",
		RepoID:           "b",
		RepoFormat:       manifest.Format{ObjectFormat: "sha1"},
		ManifestVersion:  1,
		LatestTx:         "tx_init",
		CreatedAt:        time.Now().UTC().Truncate(time.Second),
		UpdatedAt:        time.Now().UTC().Truncate(time.Second),
	}
	body := json.RawMessage(`{"refs":{},"packs":[],"default_branch":"refs/heads/main"}`)
	wrapped, err := manifest.WrapHeaderInBody(header, body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutIfAbsent(ctx, key, strings.NewReader(string(wrapped)), nil); err != nil {
		t.Fatal(err)
	}

	gotHeader, gotBody, ver, err := manifest.ReadRoot(ctx, s, key)
	if err != nil {
		t.Fatal(err)
	}
	if gotHeader.RepoID != "b" || gotHeader.ManifestVersion != 1 {
		t.Errorf("header round-trip wrong: %+v", gotHeader)
	}
	if !json.Valid(gotBody) {
		t.Errorf("body not valid JSON: %s", gotBody)
	}

	header.ManifestVersion = 2
	header.LatestTx = "tx_2"
	header.UpdatedAt = time.Now().UTC().Truncate(time.Second)
	wrapped2, _ := manifest.WrapHeaderInBody(header, body)
	if _, err := manifest.CASRoot(ctx, s, key, wrapped2, ver); err != nil {
		t.Fatal(err)
	}
}

func TestCASRoot_VersionMismatch(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	key := "tenants/a/repos/b/manifest/root.json"

	header := manifest.RootHeader{
		SchemaVersion: 1, RepoID: "b",
		RepoFormat:      manifest.Format{ObjectFormat: "sha1"},
		ManifestVersion: 1,
	}
	wrapped, _ := manifest.WrapHeaderInBody(header, json.RawMessage(`{}`))
	if _, err := s.PutIfAbsent(ctx, key, strings.NewReader(string(wrapped)), nil); err != nil {
		t.Fatal(err)
	}

	stale := storage.ObjectVersion{Provider: "localfs", Token: "deadbeef"}
	_, err := manifest.CASRoot(ctx, s, key, wrapped, stale)
	if !errors.Is(err, storage.ErrVersionMismatch) {
		t.Errorf("want ErrVersionMismatch, got %v", err)
	}
}

func TestWrapHeaderInBody_RejectsHeaderKeysInBody(t *testing.T) {
	header := manifest.RootHeader{SchemaVersion: 1}
	body := json.RawMessage(`{"refs":{},"manifest_version":99}`)
	if _, err := manifest.WrapHeaderInBody(header, body); err == nil {
		t.Fatal("expected error when body contains a reserved header key")
	}
}

func TestReadRoot_FutureSchemaRejected(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	key := "tenants/a/repos/b/manifest/root.json"

	header := manifest.RootHeader{
		SchemaVersion:    999, // future
		MinReaderVersion: "0.1.0",
		RepoID:           "b",
		RepoFormat:       manifest.Format{ObjectFormat: "sha1"},
		ManifestVersion:  1,
		LatestTx:         "tx_init",
		CreatedAt:        time.Now().UTC().Truncate(time.Second),
		UpdatedAt:        time.Now().UTC().Truncate(time.Second),
	}
	wrapped, err := manifest.WrapHeaderInBody(header, json.RawMessage(`{"refs":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutIfAbsent(ctx, key, strings.NewReader(string(wrapped)), nil); err != nil {
		t.Fatal(err)
	}

	_, _, _, err = manifest.ReadRoot(ctx, s, key)
	if !errors.Is(err, repo.ErrUnsupportedSchema) {
		t.Errorf("want ErrUnsupportedSchema, got %v", err)
	}
}
