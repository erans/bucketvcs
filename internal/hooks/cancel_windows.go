//go:build windows

package hooks

import "os/exec"

// cancelHookProcess terminates a running hook when its context is cancelled.
// Windows has no POSIX process groups or signals here, so we kill the process
// directly. (Hooks on Windows run only under --hooks-unsafe-no-sandbox; the
// bwrap sandbox is Linux-only.)
func cancelHookProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
