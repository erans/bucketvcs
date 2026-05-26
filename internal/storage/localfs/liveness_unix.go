//go:build !windows

package localfs

import (
	"errors"
	"syscall"
)

// processAlive reports whether pid is a live process on the current host.
// POSIX kill(pid, 0) returns nil if the process exists and we may signal it;
// ESRCH if the PID is dead; EPERM if alive but not signalable (treated as
// alive — conservative, we never reclaim a lock we can't prove is dead).
func processAlive(pid int) (bool, error) {
	err := syscall.Kill(pid, 0)
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
