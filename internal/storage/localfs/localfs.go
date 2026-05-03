// Package localfs implements storage.ObjectStore over a regular local
// filesystem. It is intended for development, tests, and small
// self-hosted deployments. Localfs is single-process: holding two open
// Localfs instances against the same root directory in different
// processes is undefined.
package localfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

// ErrClosed is returned by Localfs operations called after Close has
// fully succeeded (lockfile released). A closed instance refuses to
// service any read or write so it cannot scribble on a bucket whose
// lock another process may now hold.
var ErrClosed = errors.New("localfs: instance is closed")

// checkOpen returns ErrClosed if the lockfile has been released. Every
// method that touches the bucket calls this before acquiring per-key
// mutexes or performing I/O.
func (l *Localfs) checkOpen() error {
	if l.lockfileRemoved {
		return ErrClosed
	}
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
	if err := l.checkOpen(); err != nil {
		return nil, err
	}
	if err := validateKey(key); err != nil {
		return nil, err
	}
	l.mutexes.lock(key)
	defer l.mutexes.unlock(key)

	if err := lstatNoSymlink(l.objectPath(key)); err != nil {
		return nil, err
	}
	md, err := l.headLocked(key)
	if err != nil {
		return nil, err
	}
	if opts != nil && opts.IfVersionMatches != nil && opts.IfVersionMatches.Token != md.Version.Token {
		return nil, fmt.Errorf("%w: get if-version-matches", storage.ErrVersionMismatch)
	}
	f, err := os.Open(l.objectPath(key))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, storage.ErrNotFound
		}
		return nil, err
	}
	// Note: f remains valid for the caller to read after we release
	// the keyed mutex on return. POSIX guarantees the inode stays
	// reachable through the open file descriptor even if a subsequent
	// writer renames a new file over the path.
	return &storage.Object{Body: f, Metadata: *md}, nil
}

func (l *Localfs) Head(ctx context.Context, key string) (*storage.ObjectMetadata, error) {
	if err := l.checkOpen(); err != nil {
		return nil, err
	}
	if err := validateKey(key); err != nil {
		return nil, err
	}
	l.mutexes.lock(key)
	defer l.mutexes.unlock(key)

	if err := lstatNoSymlink(l.objectPath(key)); err != nil {
		return nil, err
	}
	return l.headLocked(key)
}

func (l *Localfs) GetRange(ctx context.Context, key string, start, endInclusive int64) (io.ReadCloser, error) {
	if err := l.checkOpen(); err != nil {
		return nil, err
	}
	if err := validateKey(key); err != nil {
		return nil, err
	}
	if start < 0 || endInclusive < start {
		return nil, fmt.Errorf("%w: invalid range [%d,%d]", storage.ErrInvalidArgument, start, endInclusive)
	}
	l.mutexes.lock(key)
	defer l.mutexes.unlock(key)

	if err := lstatNoSymlink(l.objectPath(key)); err != nil {
		return nil, err
	}
	// Run the sidecar gate before streaming bytes. headLocked fails
	// closed on ErrUnsupportedSidecarSchema; without this check, an
	// older binary could happily stream bytes for an object whose
	// future-schema sidecar Head/Get would refuse, hiding the
	// downgrade signal from range-read callers.
	if _, err := l.headLocked(key); err != nil {
		return nil, err
	}
	f, err := os.Open(l.objectPath(key))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, storage.ErrNotFound
		}
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if start >= info.Size() {
		_ = f.Close()
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	end := endInclusive
	if end >= info.Size() {
		end = info.Size() - 1
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, err
	}
	// As with Get, the open file descriptor remains valid for the
	// caller after we release the keyed mutex on return.
	return &limitedReadCloser{Reader: io.LimitReader(f, end-start+1), Closer: f}, nil
}

type limitedReadCloser struct {
	io.Reader
	io.Closer
}

func (l *Localfs) PutIfAbsent(ctx context.Context, key string, body io.Reader, opts *storage.PutOptions) (storage.ObjectVersion, error) {
	if err := l.checkOpen(); err != nil {
		return storage.ObjectVersion{}, err
	}
	if err := validateKey(key); err != nil {
		return storage.ObjectVersion{}, err
	}
	l.mutexes.lock(key)
	defer l.mutexes.unlock(key)

	objPath := l.objectPath(key)
	// rename(2) silently overwrites existing targets, so the absence
	// check must be performed under the same mutex held during the
	// atomic write below. Defense-in-depth on Linux would use
	// renameat2(RENAME_NOREPLACE), deferred.
	if _, err := os.Lstat(objPath); err == nil {
		return storage.ObjectVersion{}, storage.ErrAlreadyExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return storage.ObjectVersion{}, err
	}

	contentType := ""
	if opts != nil {
		contentType = opts.ContentType
	}
	v, err := l.writeAtomic(key, body, contentType)
	if err != nil {
		return storage.ObjectVersion{}, err
	}
	return v, nil
}

func (l *Localfs) PutIfVersionMatches(ctx context.Context, key string, expected storage.ObjectVersion, body io.Reader, opts *storage.PutOptions) (storage.ObjectVersion, error) {
	if err := l.checkOpen(); err != nil {
		return storage.ObjectVersion{}, err
	}
	if err := validateKey(key); err != nil {
		return storage.ObjectVersion{}, err
	}
	l.mutexes.lock(key)
	defer l.mutexes.unlock(key)

	current, err := l.headLocked(key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return storage.ObjectVersion{}, fmt.Errorf("%w: object absent", storage.ErrVersionMismatch)
		}
		return storage.ObjectVersion{}, err
	}
	if current.Version.Token != expected.Token {
		return storage.ObjectVersion{}, fmt.Errorf("%w: have %s want %s", storage.ErrVersionMismatch, current.Version.Token, expected.Token)
	}

	contentType := ""
	if opts != nil {
		contentType = opts.ContentType
	}
	return l.writeAtomic(key, body, contentType)
}

func (l *Localfs) DeleteIfVersionMatches(ctx context.Context, key string, expected storage.ObjectVersion) error {
	if err := l.checkOpen(); err != nil {
		return err
	}
	if err := validateKey(key); err != nil {
		return err
	}
	l.mutexes.lock(key)
	defer l.mutexes.unlock(key)

	current, err := l.headLocked(key)
	if err != nil {
		return err
	}
	if current.Version.Token != expected.Token {
		return fmt.Errorf("%w: have %s want %s", storage.ErrVersionMismatch, current.Version.Token, expected.Token)
	}
	// Order: remove content first, then sidecar. A crash between the
	// two leaves "no content + orphan sidecar"; subsequent Head returns
	// ErrNotFound (correct outcome). The reverse order would leave
	// "content present + missing sidecar"; Head's self-heal would
	// regenerate the sidecar and the deleted object would resurrect.
	objPath := l.objectPath(key)
	objDir := filepath.Dir(objPath)
	if err := os.Remove(objPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return storage.ErrNotFound
		}
		return err
	}
	if err := fsyncDir(objDir); err != nil {
		return err
	}
	if err := os.Remove(l.metaPath(key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := fsyncDir(objDir); err != nil {
		return err
	}
	return nil
}

func (l *Localfs) List(ctx context.Context, prefix string, opts *storage.ListOptions) (*storage.ListPage, error) {
	if err := l.checkOpen(); err != nil {
		return nil, err
	}
	maxKeys := 1000
	delimiter := ""
	cont := ""
	if opts != nil {
		if opts.MaxKeys > 0 {
			maxKeys = opts.MaxKeys
		}
		delimiter = opts.Delimiter
		cont = opts.ContinuationToken
	}

	keys, err := l.collectKeys(prefix)
	if err != nil {
		return nil, err
	}
	// Filter to keys strictly greater than the continuation token, if any.
	if cont != "" {
		idx := sort.SearchStrings(keys, cont)
		// Skip the cont key itself (token is the last key returned).
		for idx < len(keys) && keys[idx] <= cont {
			idx++
		}
		keys = keys[idx:]
	}

	page := &storage.ListPage{}
	commonSeen := map[string]bool{}
	for _, k := range keys {
		if delimiter != "" {
			rest := strings.TrimPrefix(k, prefix)
			if i := strings.Index(rest, delimiter); i >= 0 {
				cp := prefix + rest[:i+len(delimiter)]
				if !commonSeen[cp] {
					commonSeen[cp] = true
					page.CommonPrefixes = append(page.CommonPrefixes, cp)
				}
				continue
			}
		}
		md, err := l.head(k)
		if err != nil {
			return nil, err
		}
		page.Objects = append(page.Objects, *md)
		if len(page.Objects)+len(page.CommonPrefixes) >= maxKeys {
			page.NextToken = k
			return page, nil
		}
	}
	return page, nil
}

// collectKeys walks the objects directory under prefix and returns
// matching keys in lexicographic order. Sidecar files are excluded.
func (l *Localfs) collectKeys(prefix string) ([]string, error) {
	root := filepath.Join(l.root, objectsDir)
	prefixFs := filepath.FromSlash(prefix)
	walkRoot := root
	if prefixFs != "" {
		walkRoot = filepath.Join(root, prefixFs)
		// If walkRoot is a file (the prefix happens to be a complete key),
		// list the parent directory and filter by prefix.
		info, err := os.Stat(walkRoot)
		if err != nil || !info.IsDir() {
			walkRoot = root
		}
	}

	var keys []string
	err := filepath.WalkDir(walkRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		// Skip symlinks per AD11. filepath.WalkDir does not follow
		// symlinks; d.Type() reports ModeSymlink for them.
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if strings.HasSuffix(path, metaSuffix) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		keys = append(keys, key)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(keys)
	return keys, nil
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

// objectPath returns the filesystem path for an object's content.
func (l *Localfs) objectPath(key string) string {
	return filepath.Join(l.root, objectsDir, filepath.FromSlash(key))
}

// metaPath returns the filesystem path for an object's sidecar.
func (l *Localfs) metaPath(key string) string {
	return l.objectPath(key) + metaSuffix
}

// lstatNoSymlink rejects symlinks under the bucket root per AD11.
// Returns ErrNotFound if the path does not exist, ErrInvalidArgument
// if it does and is a symlink, nil otherwise. Not TOCTOU-safe: an
// attacker who can write to the bucket can race symlink replacement
// against subsequent open calls. For the localfs dev/test threat model
// this is acceptable; full path-resolution sandboxing
// (openat2 RESOLVE_BENEATH) is deferred.
func lstatNoSymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return storage.ErrNotFound
		}
		return err
	}
	if info.Mode().Type()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%w: path is a symlink (not allowed)", storage.ErrInvalidArgument)
	}
	return nil
}

// headLocked reads the sidecar (or self-heals from content if the
// sidecar is missing, unreadable, or stale relative to content) and
// returns metadata. Caller MUST hold l.mutexes for the key. Returns
// ErrNotFound if the object content does not exist.
//
// The "stale relative to content" check is a size-mismatch fast-path
// that catches the post-crash "content (new) + sidecar (old)" window
// when the new content has a different size from the old. Same-size
// post-crash torn states are NOT detected by this fast-path; operators
// must run Localfs.Verify (Task 35) after unclean shutdown to fully
// reconcile.
func (l *Localfs) headLocked(key string) (*storage.ObjectMetadata, error) {
	contentInfo, err := os.Stat(l.objectPath(key))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, storage.ErrNotFound
		}
		return nil, err
	}

	var sc sidecar
	scBytes, err := os.ReadFile(l.metaPath(key))
	if err == nil {
		sc, err = parseSidecar(scBytes)
	}
	if err == nil && sc.Size != contentInfo.Size() {
		// Stale sidecar: content size disagrees with sidecar's recorded
		// size. Most likely a crash mid-PutIfVersionMatches between
		// content rename and sidecar rename. Treat as if the sidecar is
		// missing.
		err = fmt.Errorf("sidecar size %d != content size %d (stale)", sc.Size, contentInfo.Size())
	}
	if err != nil {
		// Fail closed on unsupported schema version: an older binary
		// must NOT overwrite a future-schema sidecar with the current
		// schema, because that would silently downgrade the on-disk
		// format. Operators upgrade by running a binary that knows the
		// future schema.
		if errors.Is(err, ErrUnsupportedSidecarSchema) {
			return nil, err
		}
		// Self-heal: recompute sha256 from content. Sidecar may be
		// missing, truncated, JSON-malformed, or stale relative to
		// content (size-mismatch fast-path).
		sc, err = l.healSidecar(key, contentInfo)
		if err != nil {
			return nil, err
		}
	}

	return &storage.ObjectMetadata{
		Key: key,
		Version: storage.ObjectVersion{
			Provider: "localfs",
			Token:    sc.Sha256,
			Kind:     storage.VersionEtag,
		},
		Size:        sc.Size,
		ContentType: sc.ContentType,
		ModifiedAt:  sc.ModifiedAt,
	}, nil
}

// head is the locking wrapper for headLocked. Used by callers that do
// NOT already hold the per-key mutex (notably List, which walks across
// keys and locks each one individually).
func (l *Localfs) head(key string) (*storage.ObjectMetadata, error) {
	l.mutexes.lock(key)
	defer l.mutexes.unlock(key)
	return l.headLocked(key)
}

// healSidecar recomputes a sidecar from content when the on-disk sidecar
// is missing or unreadable. Writes the new sidecar back so subsequent
// reads are fast. Caller holds the keyed mutex.
func (l *Localfs) healSidecar(key string, contentInfo os.FileInfo) (sidecar, error) {
	f, err := os.Open(l.objectPath(key))
	if err != nil {
		return sidecar{}, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return sidecar{}, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	sc := newSidecar(sum, contentInfo.Size(), "", contentInfo.ModTime())
	scBytes, err := encodeSidecar(sc)
	if err != nil {
		return sidecar{}, err
	}
	if err := writeFileAtomic(l.metaPath(key), scBytes); err != nil {
		return sidecar{}, err
	}
	return sc, nil
}

// writeAtomic streams body to a temp file in the destination directory,
// hashes it as it goes, atomically renames into place, fsyncs the
// directory, then writes the sidecar via the same pattern. Caller holds
// the keyed mutex.
func (l *Localfs) writeAtomic(key string, body io.Reader, contentType string) (storage.ObjectVersion, error) {
	objPath := l.objectPath(key)
	objDir := filepath.Dir(objPath)
	if err := os.MkdirAll(objDir, 0o755); err != nil {
		return storage.ObjectVersion{}, err
	}
	tmp, err := os.CreateTemp(objDir, "."+filepath.Base(objPath)+".tmp.*")
	if err != nil {
		return storage.ObjectVersion{}, err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	h := sha256.New()
	tee := io.TeeReader(body, h)
	n, err := io.Copy(tmp, tee)
	if err != nil {
		_ = tmp.Close()
		cleanup()
		return storage.ObjectVersion{}, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return storage.ObjectVersion{}, err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return storage.ObjectVersion{}, err
	}
	if err := os.Rename(tmpName, objPath); err != nil {
		cleanup()
		return storage.ObjectVersion{}, err
	}
	if err := fsyncDir(objDir); err != nil {
		return storage.ObjectVersion{}, err
	}

	sum := hex.EncodeToString(h.Sum(nil))
	sc := newSidecar(sum, n, contentType, time.Now().UTC())
	scBytes, err := encodeSidecar(sc)
	if err != nil {
		return storage.ObjectVersion{}, err
	}
	if err := writeFileAtomic(l.metaPath(key), scBytes); err != nil {
		return storage.ObjectVersion{}, err
	}

	return storage.ObjectVersion{
		Provider: "localfs",
		Token:    sum,
		Kind:     storage.VersionEtag,
	}, nil
}

// writeFileAtomic writes data to path via temp + rename, then fsyncs
// the parent directory so the rename is durable across crashes.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return fsyncDir(dir)
}

// fsyncDir opens the directory and calls Sync to durably persist any
// rename or unlink that happened in it. POSIX requires this for crash
// durability of directory-entry changes; without it, a rename can be
// lost across a crash even though the file's own fsync succeeded.
func fsyncDir(path string) error {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
