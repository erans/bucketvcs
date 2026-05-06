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
// The mu field is reserved for the Manager-level RWMutex that Task 8 wires
// up; Task 7 does not expose Lock/Unlock yet.
type Mirror struct {
	root   string // <root>/<tenant>/<repo>/
	tenant string
	repoID string
	store  storage.ObjectStore

	mu sync.RWMutex // hooked up in Task 8
}

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

// openForTest is the in-package entry point used by tests. The Manager in
// Task 8 will replace it with NewManager + Manager.Open.
//
// We open and read the repo BEFORE creating any mirror directories so
// that names which pass the URL-routing-layer regex but are rejected by
// the stricter internal/repo/keys validation (e.g. "acme.prod") never
// leave dangling cache directories. Concrete sequence:
//  1. mirror.validName (M3 §10 regex + traversal/length checks)
//  2. repo.Open + ReadRoot (full keys.validID + manifest existence)
//  3. mkdir <root>/<tenant>/<repo>/incoming
//  4. write bare/ via the M2 exporter and the sentinel
func openForTest(ctx context.Context, rootDir string, store storage.ObjectStore, tenant, repoID string) (*Mirror, error) {
	if !validName(tenant) {
		return nil, fmt.Errorf("mirror: invalid tenant %q", tenant)
	}
	if !validName(repoID) {
		return nil, fmt.Errorf("mirror: invalid repoID %q", repoID)
	}
	r, err := repo.Open(ctx, store, tenant, repoID)
	if err != nil {
		return nil, fmt.Errorf("mirror: repo.Open: %w", err)
	}
	view, err := r.ReadRoot(ctx)
	if err != nil {
		return nil, fmt.Errorf("mirror: repo.ReadRoot: %w", err)
	}

	root := filepath.Join(rootDir, tenant, repoID)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("mirror: mkdir root: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "incoming"), 0o755); err != nil {
		return nil, fmt.Errorf("mirror: mkdir incoming: %w", err)
	}
	m := &Mirror{root: root, tenant: tenant, repoID: repoID, store: store}
	if err := m.syncTo(ctx, view); err != nil {
		return nil, err
	}
	return m, nil
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
