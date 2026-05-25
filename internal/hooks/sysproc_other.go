//go:build !linux

package hooks

import "syscall"

func sysProcAttrForRunner(cfg RunnerConfig) *syscall.SysProcAttr { return nil }
