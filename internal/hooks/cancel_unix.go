//go:build !windows

package hooks

import (
	"os/exec"
	"syscall"
)

// cancelHookProcess terminates a running hook when its context is cancelled.
// On Unix it signals the whole process group (Setpgid is set in
// sysProcAttrForRunner on Linux) so children the script spawned are killed
// too, falling back to signaling just the leader where process-group setup
// isn't available.
func cancelHookProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	// Negative PID = signal the process group.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	return nil
}
