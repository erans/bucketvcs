// Package mirror manages per-repo on-disk bare-repo caches that the gateway
// uses for `git pack-objects` (fetch) and `git index-pack` (push). The
// authoritative state lives in the bucket; the mirror is a derived view that
// can be wiped and rebuilt from the M2 exporter at any time.
package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/bucketvcs/bucketvcs/internal/exporter"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// nameRE matches valid tenant/repo identifiers per M3 spec §10
// (URL routing). validName layers additional checks on top to prevent
// path traversal and to align with the stricter internal/repo/keys rules
// (see validName for details).
var nameRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// maxNameLen mirrors the 128-byte cap that internal/repo/keys.validID
// enforces. Names longer than this would pass the URL-routing regex but
// be rejected by repo.Open after we have already created mirror dirs.
const maxNameLen = 128

// Mirror is the per-repo on-disk bare-repo cache.
//
// The per-repo RWMutex (mu) is exposed via RLock/RUnlock/Lock/Unlock for
// the gateway: fetches RLock, pushes Lock. It is held only for the duration
// of a single HTTP request, never across long external calls.
type Mirror struct {
	root   string // <root>/<tenant>/<repo>/
	tenant string
	repoID string
	store  storage.ObjectStore

	mu sync.RWMutex
}

// RLock acquires the per-repo read lock. Used by fetch handlers so multiple
// concurrent reads can share the bare repo while a writer is excluded.
func (m *Mirror) RLock() { m.mu.RLock() }

// RUnlock releases the per-repo read lock.
func (m *Mirror) RUnlock() { m.mu.RUnlock() }

// Lock acquires the per-repo write lock. Used by push handlers so a writer
// excludes both readers and other writers for the duration of an ingest.
func (m *Mirror) Lock() { m.mu.Lock() }

// Unlock releases the per-repo write lock.
func (m *Mirror) Unlock() { m.mu.Unlock() }

// BareDir returns the absolute path to the bare git repo (suitable for
// `git -C <BareDir>` invocations).
func (m *Mirror) BareDir() string { return filepath.Join(m.root, "bare") }

// VersionFile returns the absolute path to the manifest-version sentinel.
func (m *Mirror) VersionFile() string { return filepath.Join(m.root, "manifest_version.txt") }

// IncomingDir returns the per-repo staging dir for inbound packs (push).
func (m *Mirror) IncomingDir() string { return filepath.Join(m.root, "incoming") }

// validName enforces the M3 §10 identifier shape for URL routing and
// additionally rejects:
//   - "." and ".." which filepath.Join would resolve, letting a caller
//     escape the mirror root.
//   - any ".." substring (e.g. "a..b") to match the path-traversal
//     defense in internal/repo/keys.validID.
//   - names longer than maxNameLen (128) to mirror the keys.validID cap,
//     so we never create a mirror directory for a name that repo.Open
//     will subsequently reject.
//
// nameRE itself stays at the spec §10 charset because the mirror is the
// URL-routing-layer gate; the stricter checks above only narrow the set
// in ways that the routing surface and the repo layer already reject.
func validName(s string) bool {
	if len(s) == 0 || len(s) > maxNameLen {
		return false
	}
	if s == "." || s == ".." || strings.Contains(s, "..") {
		return false
	}
	return nameRE.MatchString(s)
}

// Manager owns the per-process collection of mirrors. Construct one at
// gateway startup with NewManager; close on shutdown to release the
// process-wide flock.
//
// The manager-level mu protects the mirrors map. Per-repo serialization
// uses each *Mirror's RWMutex, not this lock.
type Manager struct {
	rootDir string
	store   storage.ObjectStore
	lock    *os.File

	mu      sync.Mutex
	mirrors map[string]*Mirror
}

// NewManager creates the manager rooted at rootDir. It acquires a
// process-wide flock on <rootDir>/.bucketvcs-mirror-lock so a second
// `bucketvcs serve` against the same mirror dir refuses to start.
func NewManager(rootDir string, store storage.ObjectStore) (*Manager, error) {
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return nil, fmt.Errorf("mirror: mkdir root: %w", err)
	}
	lf, err := acquireLock(filepath.Join(rootDir, ".bucketvcs-mirror-lock"))
	if err != nil {
		return nil, err
	}
	return &Manager{
		rootDir: rootDir,
		store:   store,
		lock:    lf,
		mirrors: map[string]*Mirror{},
	}, nil
}

// Open returns the *Mirror for (tenant, repoID), lazy-materializing it on
// first use. Subsequent calls for the same key return the same *Mirror so
// callers share a single per-repo RWMutex. SyncToCurrent runs on every
// call (cheap when the bare repo is already current).
//
// We open and read the repo BEFORE creating any mirror directories so that
// names which pass the URL-routing-layer regex but are rejected by the
// stricter internal/repo/keys validation (e.g. "acme.prod") never leave
// dangling cache directories.
func (mg *Manager) Open(ctx context.Context, tenant, repoID string) (*Mirror, error) {
	if !validName(tenant) {
		return nil, fmt.Errorf("mirror: invalid tenant %q", tenant)
	}
	if !validName(repoID) {
		return nil, fmt.Errorf("mirror: invalid repoID %q", repoID)
	}
	key := tenant + "/" + repoID

	mg.mu.Lock()
	if m, ok := mg.mirrors[key]; ok {
		mg.mu.Unlock()
		// Hot path: existing mirror, just sync.
		if err := m.SyncToCurrent(ctx); err != nil {
			return nil, err
		}
		return m, nil
	}
	mg.mu.Unlock()

	// Validate at the repo layer FIRST so we don't pollute the dir tree
	// with names the repo layer refuses.
	r, err := repo.Open(ctx, mg.store, tenant, repoID)
	if err != nil {
		return nil, fmt.Errorf("mirror: repo.Open: %w", err)
	}
	view, err := r.ReadRoot(ctx)
	if err != nil {
		return nil, fmt.Errorf("mirror: repo.ReadRoot: %w", err)
	}

	root := filepath.Join(mg.rootDir, tenant, repoID)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("mirror: mkdir root: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "incoming"), 0o755); err != nil {
		return nil, fmt.Errorf("mirror: mkdir incoming: %w", err)
	}
	m := &Mirror{root: root, tenant: tenant, repoID: repoID, store: mg.store}
	if err := m.syncTo(ctx, view); err != nil {
		return nil, err
	}

	// Race recheck under the manager lock: another caller may have created
	// and registered a Mirror for the same key while we were syncing. If
	// so, return theirs so all callers share one *Mirror (and one mutex).
	// The bare/ on-disk state is identical either way.
	mg.mu.Lock()
	if existing, ok := mg.mirrors[key]; ok {
		mg.mu.Unlock()
		return existing, nil
	}
	mg.mirrors[key] = m
	mg.mu.Unlock()
	return m, nil
}

// Close releases the process flock. It does not delete on-disk mirrors.
// Safe to call multiple times.
func (mg *Manager) Close() error {
	f := mg.lock
	mg.lock = nil
	return releaseLock(f)
}

// openForTest is the in-package entry point used by tests. It wraps
// NewManager + Manager.Open so the same code path is exercised. The
// Manager is intentionally leaked: the test process exits soon after and
// releases the flock. Tests that need to assert on flock semantics or
// reuse a manager across calls should use NewManager directly.
func openForTest(ctx context.Context, rootDir string, store storage.ObjectStore, tenant, repoID string) (*Mirror, error) {
	mg, err := NewManager(rootDir, store)
	if err != nil {
		return nil, err
	}
	return mg.Open(ctx, tenant, repoID)
}

// SyncToCurrent compares the on-disk sentinel against the bucket's
// current root manifest identity. If they match and bare/ exists,
// returns nil. Otherwise wipes and rebuilds bare/ via the M2 exporter
// and writes a fresh sentinel.
//
// The sentinel is a small JSON document that records BOTH the
// monotonic ManifestVersion and the per-commit LatestTx ID. Comparing
// only ManifestVersion would miss same-version manifest replacements
// (repo deleted+recreated, restored from backup, swapped from another
// bucket) where a different root happens to land on the same numeric
// version. LatestTx is generated per Commit (M1 ULID-style) so two
// distinct manifests will not share it in practice.
func (m *Mirror) SyncToCurrent(ctx context.Context) error {
	r, err := repo.Open(ctx, m.store, m.tenant, m.repoID)
	if err != nil {
		return fmt.Errorf("mirror: repo.Open: %w", err)
	}
	view, err := r.ReadRoot(ctx)
	if err != nil {
		return fmt.Errorf("mirror: repo.ReadRoot: %w", err)
	}
	return m.syncTo(ctx, view)
}

// syncTo is the inner sync routine that takes an already-fetched
// RootView. openForTest reuses this after pre-opening the repo for
// validation.
func (m *Mirror) syncTo(ctx context.Context, view *repo.RootView) error {
	want := sentinel{
		ManifestVersion: view.Header.ManifestVersion,
		LatestTx:        view.Header.LatestTx,
	}

	bareExists := dirExists(m.BareDir())
	got, gotErr := readSentinel(m.VersionFile())
	if bareExists && gotErr == nil && got == want {
		return nil
	}

	// Stale or absent: wipe bare/ and the sentinel, rebuild via exporter.
	// IMPORTANT: exporter.Export REQUIRES DestDir to be empty/non-existent,
	// so we RemoveAll bare/ rather than MkdirAll-ing it here.
	if bareExists {
		if err := os.RemoveAll(m.BareDir()); err != nil {
			return fmt.Errorf("mirror: wipe bare: %w", err)
		}
	}
	_ = os.Remove(m.VersionFile())

	if _, err := exporter.Export(ctx, m.store, exporter.Options{
		Tenant:  m.tenant,
		Repo:    m.repoID,
		DestDir: m.BareDir(),
	}); err != nil {
		return fmt.Errorf("mirror: exporter.Export: %w", err)
	}
	if err := writeSentinel(m.VersionFile(), want); err != nil {
		return fmt.Errorf("mirror: write sentinel: %w", err)
	}
	return nil
}

// CurrentVersion reads the on-disk sentinel and returns the recorded
// ManifestVersion as a decimal string. Empty string and an error are
// returned if the sentinel is missing or malformed. Tests rely on this
// to assert the sentinel was rewritten after a rebuild.
func (m *Mirror) CurrentVersion() (string, error) {
	s, err := readSentinel(m.VersionFile())
	if err != nil {
		return "", err
	}
	return strconv.FormatUint(s.ManifestVersion, 10), nil
}

// sentinel is the on-disk staleness marker. JSON gives us forward
// compatibility (new fields can be added without breaking old readers,
// since unknown fields are ignored on decode and we compare by value).
type sentinel struct {
	ManifestVersion uint64 `json:"manifest_version"`
	LatestTx        string `json:"latest_tx"`
}

func writeSentinel(path string, s sentinel) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return atomicWrite(path, b)
}

func readSentinel(path string) (sentinel, error) {
	var s sentinel
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, err
	}
	if s.LatestTx == "" {
		// Defensive: treat a sentinel missing LatestTx as unusable so
		// SyncToCurrent forces a rebuild and writes a fresh one.
		return sentinel{}, errors.New("mirror: sentinel missing latest_tx")
	}
	return s, nil
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
