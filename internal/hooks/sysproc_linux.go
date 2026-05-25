//go:build linux

package hooks

import "syscall"

// sysProcAttrForRunner returns SysProcAttr for the subprocess. RLIMIT_CPU and
// RLIMIT_AS are NOT applied here — Go's syscall.SysProcAttr doesn't expose
// them. In sandbox mode we apply them via bwrap's --rlimit-cpu / --rlimit-as
// flags (added to the argv in buildArgv). In unsafe-no-sandbox mode the
// timeout via context.WithTimeout is the only resource bound; that's
// acceptable for the dev/local mode the flag exists to support.
//
// Setpgid lets cmd.Cancel signal the entire process group, so a script that
// spawned grandchildren can still be killed cleanly on timeout.
func sysProcAttrForRunner(cfg RunnerConfig) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
