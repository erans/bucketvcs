package hooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// RunnerConfig controls subprocess execution. Populated from operator flags.
type RunnerConfig struct {
	HooksRoot       string              // absolute dir containing script files
	UseSandbox      bool                // false ⇒ --hooks-unsafe-no-sandbox path
	BwrapPath       string              // resolved at Service construction; empty when UseSandbox=false
	TimeoutSec      int                 // wall-clock timeout
	CPUSec          int                 // RLIMIT_CPU (applied via bwrap --rlimit-cpu)
	MemoryMB        int                 // RLIMIT_AS in MiB (applied via bwrap --rlimit-as)
	OutputMaxKB     int                 // stdout+stderr cap
	AllowNetworkSet map[string]struct{} // script_names that get --share-net
	ExtraEnv        []string            // operator-supplied KEY=VALUE; passed to every hook
	Logger          *slog.Logger
}

// RunResult captures one subprocess invocation outcome. Stderr is already
// truncated to OutputMaxKB.
type RunResult struct {
	ExitCode int    // 0 on success
	Stdout   []byte // truncated
	Stderr   []byte // truncated
	Err      error  // ErrTimeout, ErrScriptNotFound, ErrSandboxMissing, ErrInternal, or nil
}

// Runner executes one hook script under sandbox + rlimits.
type Runner struct {
	cfg RunnerConfig
}

func NewRunner(cfg RunnerConfig) *Runner { return &Runner{cfg: cfg} }

// Run executes <hooksRoot>/<scriptName> with the supplied stdin and env vars
// merged on top of the runner's ExtraEnv. Returns a RunResult that captures
// exit code, truncated stdout/stderr, and any sentinel error.
//
// Run executes <hooksRoot>/<scriptName> with the supplied stdin and env vars.
// bareDir is the absolute path to the repo's bare directory; when non-empty,
// it's bind-mounted read-only at /repo inside the sandbox (sandbox mode) and
// BUCKETVCS_BARE_DIR is exported in the env (sandbox: "/repo"; unsafe mode:
// the actual bareDir path). bareDir may be "" — useful for unit tests that
// don't exercise bare-repo access.
//
// Non-zero exit is NOT an error from Run's perspective — Err remains nil and
// ExitCode is populated. The caller (Service) maps non-zero exits to
// HookRejection for pre-receive and to async logging for post-receive.
func (r *Runner) Run(ctx context.Context, bareDir, scriptName string, stdin []byte, perHookEnv map[string]string) RunResult {
	// Defense-in-depth: script_name was validated at registration time, but
	// re-check at runtime in case the sqlite was tampered with directly.
	if !ValidScriptName(scriptName) {
		return RunResult{Err: fmt.Errorf("%w: invalid script_name %q", ErrInternal, scriptName)}
	}
	scriptPath := filepath.Join(r.cfg.HooksRoot, scriptName)
	if _, err := os.Stat(scriptPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return RunResult{Err: fmt.Errorf("%w: %s", ErrScriptNotFound, scriptPath)}
		}
		return RunResult{Err: fmt.Errorf("%w: stat %s: %v", ErrInternal, scriptPath, err)}
	}

	// Build the command — bwrap-wrapped or direct.
	argv := r.buildArgv(scriptPath, scriptName, bareDir)
	if argv == nil {
		return RunResult{Err: fmt.Errorf("%w: argv builder returned nil", ErrInternal)}
	}

	// Inject BUCKETVCS_BARE_DIR into the env so hooks can locate the bare
	// repo. In sandbox mode the path is the in-namespace /repo (where the
	// --ro-bind mount lives); in unsafe mode it's the host path.
	env := perHookEnv
	if bareDir != "" {
		if env == nil {
			env = make(map[string]string, 1)
		} else {
			cloned := make(map[string]string, len(env)+1)
			for k, v := range env {
				cloned[k] = v
			}
			env = cloned
		}
		if r.cfg.UseSandbox {
			env["BUCKETVCS_BARE_DIR"] = "/repo"
		} else {
			env["BUCKETVCS_BARE_DIR"] = bareDir
		}
	}

	// Wall-clock timeout via derived ctx.
	timeout := time.Duration(r.cfg.TimeoutSec) * time.Second
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, argv[0], argv[1:]...)
	cmd.Env = r.buildEnv(env)
	cmd.Stdin = bytes.NewReader(stdin)

	// Capped output buffers.
	maxBytes := r.cfg.OutputMaxKB * 1024
	stdout := newCappedBuf(maxBytes)
	stderr := newCappedBuf(maxBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	// Rlimits via SysProcAttr (Linux-only; on other platforms returns nil).
	// In sandbox mode, rlimits are also applied via bwrap --rlimit-cpu /
	// --rlimit-as inside buildArgv.
	cmd.SysProcAttr = sysProcAttrForRunner(r.cfg)

	// On context cancel: send SIGTERM to the whole process group (Setpgid is
	// set in sysProcAttrForRunner on Linux), so a script that spawned children
	// like `sleep` can be killed cleanly. After WaitDelay grace, exec will
	// SIGKILL whatever's still alive and close the I/O pipes.
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			// Negative PID = signal the process group on Unix. On platforms
			// without Setpgid support this falls back to signaling just the
			// leader, which is still better than nothing.
			if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
				_ = cmd.Process.Signal(syscall.SIGTERM)
			}
		}
		return nil
	}
	cmd.WaitDelay = time.Second

	err := cmd.Run()
	res := RunResult{
		Stdout: stdout.Bytes(),
		Stderr: stderr.Bytes(),
	}
	if cmdCtx.Err() == context.DeadlineExceeded {
		res.Err = fmt.Errorf("%w (after %ds)", ErrTimeout, r.cfg.TimeoutSec)
		return res
	}
	if err == nil {
		res.ExitCode = 0
		return res
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
		return res // non-zero exit; Err remains nil — that's HookRejection territory for the caller
	}
	res.Err = fmt.Errorf("%w: %v", ErrInternal, err)
	return res
}

// buildArgv assembles either ["bwrap", ...flags, "--", scriptPath] (sandbox mode)
// or [scriptPath] (unsafe-no-sandbox mode). When bareDir is non-empty in
// sandbox mode, it's bind-mounted read-only at /repo and the working
// directory is set to /repo so scripts can `git --git-dir=/repo log`.
func (r *Runner) buildArgv(scriptPath, scriptName, bareDir string) []string {
	if !r.cfg.UseSandbox {
		return []string{scriptPath}
	}
	if r.cfg.BwrapPath == "" {
		return nil // Service construction failed to resolve bwrap
	}
	args := []string{
		r.cfg.BwrapPath,
		"--die-with-parent",
		"--unshare-all",
		"--ro-bind", "/usr", "/usr",
		// /lib, /lib64, /bin may be absent on musl/Alpine and certain arm64
		// layouts (where /usr/lib + /usr/bin are the canonical paths and the
		// short forms are merged-usr symlinks or missing). Use --ro-bind-try
		// so bwrap silently skips a missing source instead of aborting with
		// "Can't find source path".
		"--ro-bind-try", "/lib", "/lib",
		"--ro-bind-try", "/lib64", "/lib64",
		"--ro-bind-try", "/bin", "/bin",
		"--tmpfs", "/tmp",
		"--tmpfs", "/run",
		"--proc", "/proc",
		"--dev", "/dev",
	}
	if bareDir != "" {
		// Read-only mount of the bare repo at /repo + chdir there so the
		// script's CWD is /repo. Empty bareDir falls back to chdir / so
		// the script doesn't start in an undefined working dir.
		args = append(args, "--ro-bind", bareDir, "/repo", "--chdir", "/repo")
	} else {
		args = append(args, "--chdir", "/")
	}
	if r.cfg.CPUSec > 0 {
		args = append(args, "--rlimit-cpu", fmt.Sprintf("%d", r.cfg.CPUSec))
	}
	if r.cfg.MemoryMB > 0 {
		args = append(args, "--rlimit-as", fmt.Sprintf("%d", r.cfg.MemoryMB*1024*1024))
	}
	if _, ok := r.cfg.AllowNetworkSet[scriptName]; ok {
		args = append(args, "--share-net")
	}
	args = append(args, "--", scriptPath)
	return args
}

// buildEnv composes the final environment. Order:
//
//	PATH=/usr/bin:/bin  (always)
//	runner-wide ExtraEnv from --hooks-env
//	per-hook BUCKETVCS_* vars (overlay)
//
// Importantly, os.Environ is NOT inherited — hooks see only what the operator
// configured plus the per-hook vars the Service supplies.
func (r *Runner) buildEnv(perHook map[string]string) []string {
	env := []string{"PATH=/usr/bin:/bin"}
	env = append(env, r.cfg.ExtraEnv...)
	// Deterministic order for testability.
	keys := make([]string, 0, len(perHook))
	for k := range perHook {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+perHook[k])
	}
	return env
}

// cappedBuf — bytes.Buffer that stops writing after `cap` bytes and records
// truncation. Returns the truncated bytes plus a "[output truncated]" marker
// from Bytes() if truncation occurred.
type cappedBuf struct {
	buf       bytes.Buffer
	maxBytes  int
	truncated bool
}

func newCappedBuf(maxBytes int) *cappedBuf { return &cappedBuf{maxBytes: maxBytes} }

func (c *cappedBuf) Write(p []byte) (int, error) {
	remaining := c.maxBytes - c.buf.Len()
	if remaining <= 0 {
		c.truncated = true
		return len(p), nil // pretend we wrote it; subprocess shouldn't block
	}
	if len(p) > remaining {
		c.buf.Write(p[:remaining])
		c.truncated = true
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *cappedBuf) Bytes() []byte {
	if !c.truncated {
		return c.buf.Bytes()
	}
	out := append([]byte(nil), c.buf.Bytes()...)
	return append(out, []byte("\n[output truncated]")...)
}
