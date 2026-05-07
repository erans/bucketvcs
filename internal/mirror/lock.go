//go:build unix

package mirror

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// acquireLock returns an open *os.File holding an exclusive non-blocking
// flock on path. If the lock is already held by another process, it returns
// an error.
func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("mirror: open lock: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("mirror: another bucketvcs serve is using %s: %w", path, err)
	}
	return f, nil
}

// releaseLock unlocks and closes the lock file. Safe to call with a nil file
// (no-op) so Manager.Close is idempotent if NewManager failed mid-construction.
func releaseLock(f *os.File) error {
	if f == nil {
		return nil
	}
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return f.Close()
}
