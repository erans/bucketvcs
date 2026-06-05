// Package authreplica replicates the sqlite authdb into a
// storage.ObjectStore via embedded Litestream. This file provides the
// litestream.ReplicaClient adapter; lifecycle glue (lease, restore-on-boot,
// replication runner) is layered on top in this package.
package authreplica

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/benbjohnson/litestream"
	"github.com/superfly/ltx"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// ReplicaClientType is reported by Type() and appears in litestream logs.
const ReplicaClientType = "bucketvcs-objectstore"

// DefaultPrefix is the reserved top-level key prefix used by
// --auth-db-replica=auto for the authdb replica. It is deliberately outside
// "tenants/" — the only prefix repo data and GC ever touch — so the replica
// can never collide with (or be swept as) repo data.
const DefaultPrefix = "sys/authdb"

// casRetryLimit bounds the Head+CAS loops so adapter misbehavior cannot hang
// replication forever.
const casRetryLimit = 8

// Client implements litestream.ReplicaClient on top of a storage.ObjectStore.
// Key layout mirrors litestream's file backend:
//
//	<prefix>/ltx/<level>/<ltx.FormatFilename(minTXID, maxTXID)>
//
// Level directories are decimal; the layout is owned entirely by this client and is not
// interchangeable with stock litestream file/s3 replicas (which zero-pad levels).
type Client struct {
	store  storage.ObjectStore
	prefix string
	logger *slog.Logger
}

// NewClient returns a Client writing under prefix (no trailing slash needed).
// An empty (or all-slash) prefix falls back to DefaultPrefix so the write
// keys and DeleteAll's list prefix can never diverge.
func NewClient(store storage.ObjectStore, prefix string) *Client {
	p := strings.Trim(prefix, "/")
	if p == "" {
		p = DefaultPrefix
	}
	return &Client{
		store:  store,
		prefix: p,
		logger: slog.Default(),
	}
}

// Type implements litestream.ReplicaClient.
func (c *Client) Type() string { return ReplicaClientType }

// Init implements litestream.ReplicaClient. Object stores need no setup.
func (c *Client) Init(ctx context.Context) error { return nil }

// SetLogger implements litestream.ReplicaClient.
func (c *Client) SetLogger(l *slog.Logger) {
	if l != nil {
		c.logger = l
	}
}

func (c *Client) levelDir(level int) string {
	return path.Join(c.prefix, "ltx", strconv.Itoa(level))
}

func (c *Client) ltxKey(level int, minTXID, maxTXID ltx.TXID) string {
	return path.Join(c.levelDir(level), ltx.FormatFilename(minTXID, maxTXID))
}

// LTXFiles implements litestream.ReplicaClient. useMetadata is accepted but
// we always serve timestamps from object ModifiedAt: object stores give us no
// mtime control, so header-accurate timestamps would need a per-object GET.
// The skew vs the LTX header timestamp is bounded by the sync interval (~1s).
func (c *Client) LTXFiles(ctx context.Context, level int, seek ltx.TXID, useMetadata bool) (ltx.FileIterator, error) {
	prefix := c.levelDir(level) + "/"
	var infos []*ltx.FileInfo
	var token string
	for {
		page, err := c.store.List(ctx, prefix, &storage.ListOptions{ContinuationToken: token})
		if err != nil {
			return nil, fmt.Errorf("authreplica: list %s: %w", prefix, err)
		}
		for _, om := range page.Objects {
			minTXID, maxTXID, err := ltx.ParseFilename(path.Base(om.Key))
			if err != nil {
				continue // foreign object (e.g. localfs .meta sidecar exposure); skip
			}
			if minTXID < seek {
				continue
			}
			infos = append(infos, &ltx.FileInfo{
				Level:     level,
				MinTXID:   minTXID,
				MaxTXID:   maxTXID,
				Size:      om.Size,
				CreatedAt: om.ModifiedAt.UTC(),
			})
		}
		if page.NextToken == "" {
			break
		}
		token = page.NextToken
	}
	sort.Slice(infos, func(i, j int) bool {
		if infos[i].MinTXID != infos[j].MinTXID {
			return infos[i].MinTXID < infos[j].MinTXID
		}
		return infos[i].MaxTXID < infos[j].MaxTXID
	})
	return ltx.NewFileInfoSliceIterator(infos), nil
}

// OpenLTXFile implements litestream.ReplicaClient.
func (c *Client) OpenLTXFile(ctx context.Context, level int, minTXID, maxTXID ltx.TXID, offset, size int64) (io.ReadCloser, error) {
	key := c.ltxKey(level, minTXID, maxTXID)
	if offset == 0 && size == 0 {
		obj, err := c.store.Get(ctx, key, nil)
		if errors.Is(err, storage.ErrNotFound) {
			return nil, os.ErrNotExist
		}
		if err != nil {
			return nil, fmt.Errorf("authreplica: get %s: %w", key, err)
		}
		return obj.Body, nil
	}
	end := offset + size - 1
	if size <= 0 { // offset to end-of-file
		meta, err := c.store.Head(ctx, key)
		if errors.Is(err, storage.ErrNotFound) {
			return nil, os.ErrNotExist
		}
		if err != nil {
			return nil, fmt.Errorf("authreplica: head %s: %w", key, err)
		}
		if offset >= meta.Size {
			return io.NopCloser(bytes.NewReader(nil)), nil
		}
		end = meta.Size - 1
	}
	rc, err := c.store.GetRange(ctx, key, offset, end)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, fmt.Errorf("authreplica: get-range %s: %w", key, err)
	}
	return rc, nil
}

// WriteLTXFile implements litestream.ReplicaClient. The body is buffered in
// memory: the authdb is small (MBs) and ObjectStore CAS retries need a
// re-readable source. CreatedAt comes from the LTX header timestamp when it
// parses (mirrors the file backend); otherwise falls back to now.
func (c *Client) WriteLTXFile(ctx context.Context, level int, minTXID, maxTXID ltx.TXID, r io.Reader) (*ltx.FileInfo, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("authreplica: read ltx body: %w", err)
	}
	createdAt := time.Now().UTC()
	if hdr, _, err := ltx.PeekHeader(bytes.NewReader(b)); err == nil {
		createdAt = time.UnixMilli(hdr.Timestamp).UTC()
	}
	key := c.ltxKey(level, minTXID, maxTXID)
	if err := c.putUnconditional(ctx, key, b); err != nil {
		return nil, err
	}
	return &ltx.FileInfo{
		Level:     level,
		MinTXID:   minTXID,
		MaxTXID:   maxTXID,
		Size:      int64(len(b)),
		CreatedAt: createdAt,
	}, nil
}

// putUnconditional synthesizes last-writer-wins PUT semantics (what
// litestream's native clients have) from our CAS primitives.
func (c *Client) putUnconditional(ctx context.Context, key string, b []byte) error {
	_, err := c.store.PutIfAbsent(ctx, key, bytes.NewReader(b), nil)
	if err == nil || !errors.Is(err, storage.ErrAlreadyExists) {
		return err
	}
	for i := 0; i < casRetryLimit; i++ {
		meta, err := c.store.Head(ctx, key)
		if errors.Is(err, storage.ErrNotFound) {
			if _, err := c.store.PutIfAbsent(ctx, key, bytes.NewReader(b), nil); err == nil {
				return nil
			} else if !errors.Is(err, storage.ErrAlreadyExists) {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		_, err = c.store.PutIfVersionMatches(ctx, key, meta.Version, bytes.NewReader(b), nil)
		if err == nil {
			return nil
		}
		if !errors.Is(err, storage.ErrVersionMismatch) && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
	}
	return fmt.Errorf("authreplica: put %s: CAS retry limit (%d) exceeded", key, casRetryLimit)
}

// DeleteLTXFiles implements litestream.ReplicaClient. Per-key conditional
// deletes (never a batch-delete API) — sidesteps the documented R2
// DeleteObjects silent-failure bug. Missing files are not an error.
func (c *Client) DeleteLTXFiles(ctx context.Context, a []*ltx.FileInfo) error {
	for _, info := range a {
		key := c.ltxKey(info.Level, info.MinTXID, info.MaxTXID)
		if err := c.deleteKey(ctx, key); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) deleteKey(ctx context.Context, key string) error {
	for i := 0; i < casRetryLimit; i++ {
		meta, err := c.store.Head(ctx, key)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		err = c.store.DeleteIfVersionMatches(ctx, key, meta.Version)
		if err == nil || errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if !errors.Is(err, storage.ErrVersionMismatch) {
			return err
		}
	}
	return fmt.Errorf("authreplica: delete %s: CAS retry limit (%d) exceeded", key, casRetryLimit)
}

// DeleteAll implements litestream.ReplicaClient (tests/reset only).
func (c *Client) DeleteAll(ctx context.Context) error {
	prefix := c.prefix + "/"
	var token string
	for {
		page, err := c.store.List(ctx, prefix, &storage.ListOptions{ContinuationToken: token})
		if err != nil {
			return fmt.Errorf("authreplica: list %s: %w", prefix, err)
		}
		for _, om := range page.Objects {
			if err := c.store.DeleteIfVersionMatches(ctx, om.Key, om.Version); err != nil &&
				!errors.Is(err, storage.ErrNotFound) {
				if errors.Is(err, storage.ErrVersionMismatch) {
					if err := c.deleteKey(ctx, om.Key); err != nil {
						return err
					}
					continue
				}
				return err
			}
		}
		if page.NextToken == "" {
			return nil
		}
		token = page.NextToken
	}
}

// Compile-time interface check.
var _ litestream.ReplicaClient = (*Client)(nil)
