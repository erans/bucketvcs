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
	"syscall"
)

// ErrLockedByLiveProcess is returned by package-level Verify when the
// recorded lockfile holder is alive on the current host (or the
// lockfile was written by a different host where M0 cannot probe
// liveness). Pass WithForce(true) to override; doing so against a
// truly live holder will corrupt that holder's state.
var ErrLockedByLiveProcess = errors.New("localfs: lockfile holder is alive")

// VerifyOption configures Verify behavior.
type VerifyOption func(*verifyConfig)

type verifyConfig struct {
	force    bool
	progress func(processed int)
}

// WithForce overrides the AD12 liveness check. Destructive: only use
// when the operator has independently confirmed no other process is
// using the bucket.
func WithForce(force bool) VerifyOption {
	return func(c *verifyConfig) { c.force = force }
}

// WithProgress installs a callback invoked after each object is
// processed. Called synchronously in the Verify goroutine. The
// callback is fire-and-forget: panics propagate (caller's
// responsibility to recover); blocking blocks Verify; no return
// value, so signal cancellation via ctx instead.
func WithProgress(cb func(processed int)) VerifyOption {
	return func(c *verifyConfig) { c.progress = cb }
}

// Verify (instance method) walks every object under <root>/objects/,
// reconciles sidecars, and returns. Acquires the per-key mutex per
// object so it can run alongside normal writes. Safe to call as
// periodic maintenance.
func (l *Localfs) Verify(ctx context.Context) error {
	if err := l.checkOpen(); err != nil {
		return err
	}
	root := filepath.Join(l.root, objectsDir)
	return walkAndReconcile(ctx, root, func(path string) error {
		key := keyForPath(l.root, path)
		l.mutexes.lock(key)
		defer l.mutexes.unlock(key)
		return reconcileOne(path)
	})
}

// Verify (package-level) recovers an unclean-shutdown bucket. Scoped
// exclusively to recovery — clean buckets (no lockfile) get nil
// immediately with no reconciliation; periodic maintenance on a
// healthy open bucket is the instance-method's job. AD12 + AD13
// govern the live-lock and snapshot-recheck behavior.
func Verify(ctx context.Context, root string, opts ...VerifyOption) error {
	if root == "" {
		return errors.New("localfs: Verify root must be non-empty")
	}
	cfg := &verifyConfig{}
	for _, o := range opts {
		o(cfg)
	}

	lockPath := filepath.Join(root, lockFile)
	preLockBytes, lockExists, err := readLockfileSnapshot(lockPath)
	if err != nil {
		return fmt.Errorf("localfs.Verify: read lockfile: %w", err)
	}
	if !lockExists {
		// Clean bucket — no recovery needed. Periodic maintenance on a
		// healthy bucket is the instance-method Verify(ctx)'s job.
		return nil
	}

	// AD12 liveness check.
	var lc lockfileContent
	if perr := json.Unmarshal(preLockBytes, &lc); perr == nil {
		alive, checkErr := isLockHolderAlive(lc)
		if checkErr != nil && !cfg.force {
			return fmt.Errorf("localfs.Verify: liveness check: %w", checkErr)
		}
		if alive && !cfg.force {
			return ErrLockedByLiveProcess
		}
	}
	// Malformed JSON: treat as stale, proceed. AD13 recheck still applies.

	// Reconcile every object under objects/.
	processed := 0
	if err := walkAndReconcile(ctx, filepath.Join(root, objectsDir), func(path string) error {
		if err := reconcileOne(path); err != nil {
			return err
		}
		processed++
		if cfg.progress != nil {
			cfg.progress(processed)
		}
		return nil
	}); err != nil {
		return err
	}

	// Sweep for orphan sidecars.
	if err := sweepOrphanSidecars(ctx, filepath.Join(root, objectsDir)); err != nil {
		return err
	}

	// AD13 step 5: re-read the lockfile and remove only if unchanged.
	postLockBytes, postExists, err := readLockfileSnapshot(lockPath)
	if err != nil {
		return fmt.Errorf("localfs.Verify: re-read lockfile: %w", err)
	}
	if !postExists {
		return nil
	}
	if !bytes.Equal(preLockBytes, postLockBytes) {
		// Lock contents changed during repair — a different process now
		// holds the bucket. Refuse to remove.
		return ErrLockedByLiveProcess
	}
	if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("localfs.Verify: clear lockfile: %w", err)
	}
	if err := fsyncDir(root); err != nil {
		return fmt.Errorf("localfs.Verify: fsync root after lockfile remove: %w", err)
	}
	return nil
}

// readLockfileSnapshot reads the lockfile and returns its bytes plus
// an "exists" flag. ErrNotExist returns (nil, false, nil); other
// errors propagate.
func readLockfileSnapshot(path string) ([]byte, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return b, true, nil
}

// isLockHolderAlive returns true iff the lockfile's recorded process
// is alive on the current host. Returns an error if the recorded host
// is not the current host (M0 cannot probe remote liveness).
func isLockHolderAlive(lc lockfileContent) (bool, error) {
	currentHost, _ := os.Hostname()
	if lc.Host != "" && lc.Host != currentHost {
		return true, fmt.Errorf("lockfile from different host %q (current host %q)", lc.Host, currentHost)
	}
	if lc.PID <= 0 {
		return false, nil
	}
	// POSIX kill(pid, 0) returns nil if the process exists and we have
	// permission to signal it; ESRCH if the PID is dead; EPERM if
	// alive but we can't signal. Treat EPERM as alive (conservative).
	err := syscall.Kill(lc.PID, 0)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	if errors.Is(err, syscall.EPERM) {
		return true, nil
	}
	return false, err
}

// walkAndReconcile walks objects/ and calls reconcile for each non-
// symlink, non-sidecar regular file. Cancellation surfaces as
// ctx.Err().
func walkAndReconcile(ctx context.Context, objectsRoot string, reconcile func(path string) error) error {
	return filepath.WalkDir(objectsRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil // AD11: skip symlinks
		}
		if filepath.Ext(path) == metaSuffix {
			return nil // sidecars handled alongside content
		}
		// Skip atomic-write temp files (basename starts with ".") so a
		// crash that left a tmp inside objects/ does not get a sidecar
		// written for it during reconcile.
		if base := d.Name(); len(base) > 0 && base[0] == '.' {
			return nil
		}
		return reconcile(path)
	})
}

// sweepOrphanSidecars walks objects/ and removes any .meta file whose
// corresponding content file is absent. Only safe when called by
// package-level Verify (instance-method Verify is concurrent with
// writes; removing sidecars under writers is unsafe).
func sweepOrphanSidecars(ctx context.Context, objectsRoot string) error {
	return filepath.WalkDir(objectsRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if filepath.Ext(path) != metaSuffix {
			return nil
		}
		contentPath := path[:len(path)-len(metaSuffix)]
		if _, err := os.Stat(contentPath); errors.Is(err, os.ErrNotExist) {
			if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
				return rerr
			}
		}
		return nil
	})
}

// reconcileOne ensures the (content, sidecar) pair at path is
// consistent. Recomputes sha256; rewrites sidecar if missing,
// unparseable, size-mismatched, or sha-mismatched (the same-size torn
// case Verify exists for).
func reconcileOne(contentPath string) error {
	contentInfo, err := os.Stat(contentPath)
	if err != nil {
		return err
	}
	f, err := os.Open(contentPath)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, copyErr := io.Copy(h, f)
	_ = f.Close()
	if copyErr != nil {
		return copyErr
	}
	actualSha := hex.EncodeToString(h.Sum(nil))

	metaPath := contentPath + metaSuffix
	scBytes, readErr := os.ReadFile(metaPath)
	if readErr == nil {
		if sc, perr := parseSidecar(scBytes); perr == nil {
			if sc.Sha256 == actualSha && sc.Size == contentInfo.Size() {
				return nil // already consistent
			}
		}
	}
	sc := newSidecar(actualSha, contentInfo.Size(), "", contentInfo.ModTime())
	out, err := encodeSidecar(sc)
	if err != nil {
		return err
	}
	return writeFileAtomic(metaPath, out)
}

// keyForPath inverts objectPath.
func keyForPath(root, contentPath string) string {
	rel, err := filepath.Rel(filepath.Join(root, objectsDir), contentPath)
	if err != nil {
		return contentPath
	}
	return filepath.ToSlash(rel)
}
