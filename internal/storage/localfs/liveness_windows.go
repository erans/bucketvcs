//go:build windows

package localfs

import "os"

// processAlive reports whether pid is a live process on the current host.
// On Windows os.FindProcess actually opens the process and fails for a PID
// that no longer exists, which is enough to reclaim a stale lock left by a
// crashed holder. A still-open handle is treated as alive (conservative).
// localfs is a development/test backend, so best-effort liveness is fine.
func processAlive(pid int) (bool, error) {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false, nil
	}
	_ = p.Release()
	return true, nil
}
