//go:build !unix

package mirror

import (
	"fmt"
	"os"
)

// acquireLock on non-unix platforms returns an error so the gateway
// refuses to start. M3 only supports unix hosts; if a future milestone
// needs Windows, replace this with a LockFileEx-based implementation.
func acquireLock(path string) (*os.File, error) {
	return nil, fmt.Errorf("mirror: process flock at %s requires a unix build", path)
}

// releaseLock is a no-op on non-unix; acquireLock never returned a file.
func releaseLock(f *os.File) error {
	if f == nil {
		return nil
	}
	return f.Close()
}
