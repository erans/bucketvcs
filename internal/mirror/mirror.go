// Package mirror manages per-repo on-disk bare-repo caches that the gateway
// uses for `git pack-objects` (fetch) and `git index-pack` (push). The
// authoritative state lives in the bucket; the mirror is a derived view that
// can be wiped and rebuilt from the M2 exporter at any time.
package mirror

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"

	"github.com/bucketvcs/bucketvcs/internal/exporter"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// nameRE matches valid tenant/repo identifiers per M3 spec §10
// (URL routing) and the M0/M1 key-component constraints.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

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

// validName enforces the M3 §10 identifier shape and additionally rejects
// the path-traversal sentinels "." and ".." that nameRE would otherwise
// accept. filepath.Join would resolve these, letting a caller escape the
// mirror root.
func validName(s string) bool {
	if s == "." || s == ".." {
		return false
	}
	return nameRE.MatchString(s)
}

// openForTest is the in-package entry point used by tests. The Manager in
// Task 8 will replace it with NewManager + Manager.Open.
func openForTest(ctx context.Context, rootDir string, store storage.ObjectStore, tenant, repoID string) (*Mirror, error) {
	if !validName(tenant) {
		return nil, fmt.Errorf("mirror: invalid tenant %q", tenant)
	}
	if !validName(repoID) {
		return nil, fmt.Errorf("mirror: invalid repoID %q", repoID)
	}
	root := filepath.Join(rootDir, tenant, repoID)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("mirror: mkdir root: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "incoming"), 0o755); err != nil {
		return nil, fmt.Errorf("mirror: mkdir incoming: %w", err)
	}
	m := &Mirror{root: root, tenant: tenant, repoID: repoID, store: store}
	if err := m.SyncToCurrent(ctx); err != nil {
		return nil, err
	}
	return m, nil
}

// SyncToCurrent compares the on-disk sentinel against the bucket's current
// root manifest version. If they match and bare/ exists, returns nil. If
// they don't, wipes and rebuilds bare/ via the M2 exporter, and writes the
// new sentinel.
//
// The sentinel records the decimal string of the bucket manifest's
// monotonic ManifestVersion (header field), e.g. "7". This is durable
// across restarts and re-imports because every successful Commit advances
// ManifestVersion by exactly one.
func (m *Mirror) SyncToCurrent(ctx context.Context) error {
	r, err := repo.Open(ctx, m.store, m.tenant, m.repoID)
	if err != nil {
		return fmt.Errorf("mirror: repo.Open: %w", err)
	}
	view, err := r.ReadRoot(ctx)
	if err != nil {
		return fmt.Errorf("mirror: repo.ReadRoot: %w", err)
	}
	want := strconv.FormatUint(view.Header.ManifestVersion, 10)

	bareExists := dirExists(m.BareDir())
	gotSentinel, _ := os.ReadFile(m.VersionFile())
	if bareExists && string(gotSentinel) == want {
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
	if err := atomicWrite(m.VersionFile(), []byte(want)); err != nil {
		return fmt.Errorf("mirror: write sentinel: %w", err)
	}
	return nil
}

// CurrentVersion reads the on-disk sentinel.
func (m *Mirror) CurrentVersion() (string, error) {
	b, err := os.ReadFile(m.VersionFile())
	if err != nil {
		return "", err
	}
	return string(b), nil
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
