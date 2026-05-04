// Package gitcli provides thin, well-tested wrappers around the upstream
// `git` binary. M2 import/export and the differential harness use these
// for Track A operations (shell out to git for plumbing). A single git
// binary path is resolved once at first use; tests may override it via
// SetBinaryForTest.
package gitcli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
)

var (
	binMu  sync.Mutex
	binVal string
)

// gitRepoScopingVars is the ordered list of Git environment variables that
// scope the repository location. Any of these inherited from the caller can
// redirect git away from cmd.Dir, so they are stripped before every invocation.
// Sourced from `git help environment` — "The Git Repository" section.
var gitRepoScopingVars = []string{
	"GIT_DIR",
	"GIT_WORK_TREE",
	"GIT_INDEX_FILE",
	"GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_COMMON_DIR",
	"GIT_NAMESPACE",
	"GIT_CEILING_DIRECTORIES",
	"GIT_DISCOVERY_ACROSS_FILESYSTEM",
}

// scrubGitRepoEnv returns a copy of env with all entries whose key matches one
// of the repo-scoping variables removed. Comparison is case-sensitive (env
// keys on Linux are case-sensitive). All other variables are preserved.
func scrubGitRepoEnv(env []string) []string {
	deny := make(map[string]struct{}, len(gitRepoScopingVars))
	for _, k := range gitRepoScopingVars {
		deny[k] = struct{}{}
	}
	out := make([]string, 0, len(env))
	for _, entry := range env {
		key := entry
		if idx := strings.Index(entry, "="); idx >= 0 {
			key = entry[:idx]
		}
		if _, blocked := deny[key]; !blocked {
			out = append(out, entry)
		}
	}
	return out
}

// SetBinaryForTest overrides the resolved git binary path. Returns the
// previous value so tests can restore it. The override is process-global
// and lasts until the next call. Pass "" to clear the cache so the next
// call re-resolves from $GIT_BINARY then $PATH. Production code should
// not call this.
func SetBinaryForTest(path string) string {
	binMu.Lock()
	defer binMu.Unlock()
	old := binVal
	binVal = path
	return old
}

func resolveBinary() (string, error) {
	binMu.Lock()
	defer binMu.Unlock()
	if binVal != "" {
		return binVal, nil
	}
	if v := os.Getenv("GIT_BINARY"); v != "" {
		binVal = v
		return binVal, nil
	}
	p, err := exec.LookPath("git")
	if err != nil {
		return "", fmt.Errorf("gitcli: git not found in PATH: %w", err)
	}
	binVal = p
	return binVal, nil
}

// runError wraps an exec failure with stderr captured for diagnosis.
type runError struct {
	cmd    string
	args   []string
	dir    string
	exit   int
	stderr string
	cause  error
}

func (e *runError) Error() string {
	args := strings.Join(e.args, " ")
	dir := e.dir
	if dir == "" {
		dir = "<no dir>"
	}
	return fmt.Sprintf("gitcli: %s %s (dir=%s exit=%d): %v: stderr=%q",
		e.cmd, args, dir, e.exit, e.cause, e.stderr)
}

func (e *runError) Unwrap() error { return e.cause }

func run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	bin, err := resolveBinary()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = scrubGitRepoEnv(os.Environ())
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		exit := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exit = ee.ExitCode()
		}
		return stdout.Bytes(), &runError{
			cmd: bin, args: args, dir: dir, exit: exit,
			stderr: stderr.String(), cause: err,
		}
	}
	return stdout.Bytes(), nil
}

// Version returns the output of `git --version` (e.g. "git version 2.43.0").
func Version(ctx context.Context) (string, error) {
	out, err := run(ctx, "", "--version")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// InitBare runs `git init --bare` in dir. dir must exist.
func InitBare(ctx context.Context, dir string) error {
	_, err := run(ctx, dir, "init", "--bare")
	return err
}

// Fsck runs `git fsck` (with --strict if strict) inside dir.
func Fsck(ctx context.Context, dir string, strict bool) error {
	args := []string{"fsck"}
	if strict {
		args = append(args, "--strict")
	}
	_, err := run(ctx, dir, args...)
	return err
}

// CloneBareMirror runs `git clone --bare --mirror <src> <dst>`. dst must
// not already exist (git creates it).
func CloneBareMirror(ctx context.Context, src, dst string) error {
	_, err := run(ctx, "", "clone", "--bare", "--mirror", "--quiet", src, dst)
	return err
}

// PackObjectsAll produces a single pack containing every reachable object
// in dir, written as outPrefix + "-{pack_id}.pack" + ".idx". Returns the
// pack_id (40-char hex SHA-1, the Git-native pack name from §3.2 of the
// M2 design). The function pipes `git rev-list --all --objects` into
// `git pack-objects` to keep behavior deterministic across git versions.
func PackObjectsAll(ctx context.Context, dir, outPrefix string) (string, error) {
	bin, err := resolveBinary()
	if err != nil {
		return "", err
	}
	revList := exec.CommandContext(ctx, bin, "-C", dir, "rev-list", "--all", "--objects")
	revList.Env = scrubGitRepoEnv(os.Environ())
	pipe, err := revList.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("gitcli: PackObjectsAll: rev-list pipe: %w", err)
	}
	var rlStderr bytes.Buffer
	revList.Stderr = &rlStderr

	pack := exec.CommandContext(ctx, bin, "-C", dir,
		"pack-objects", "--quiet", outPrefix)
	pack.Env = scrubGitRepoEnv(os.Environ())
	pack.Stdin = pipe
	var packStdout, packStderr bytes.Buffer
	pack.Stdout = &packStdout
	pack.Stderr = &packStderr

	if err := pack.Start(); err != nil {
		return "", fmt.Errorf("gitcli: PackObjectsAll: pack start: %w", err)
	}
	if err := revList.Run(); err != nil {
		_ = pack.Wait()
		return "", fmt.Errorf("gitcli: PackObjectsAll: rev-list: %w: stderr=%q",
			err, rlStderr.String())
	}
	if err := pack.Wait(); err != nil {
		return "", fmt.Errorf("gitcli: PackObjectsAll: pack-objects: %w: stderr=%q",
			err, packStderr.String())
	}
	// pack-objects prints exactly one pack_id line on stdout when one
	// pack is produced. The output may include trailing whitespace.
	id := strings.TrimSpace(packStdout.String())
	if len(id) != 40 {
		return "", fmt.Errorf("gitcli: PackObjectsAll: unexpected pack-objects stdout %q",
			packStdout.String())
	}
	return id, nil
}

// IndexPack runs `git index-pack` against an existing .pack file,
// producing the corresponding .idx alongside it.
func IndexPack(ctx context.Context, dir, packPath string) error {
	_, err := run(ctx, dir, "index-pack", packPath)
	return err
}

// UnpackObjects reads a pack from packPath and explodes it into loose
// objects in dir's object database. dir must be a git repo.
func UnpackObjects(ctx context.Context, dir, packPath string) error {
	bin, err := resolveBinary()
	if err != nil {
		return err
	}
	f, err := os.Open(packPath)
	if err != nil {
		return fmt.Errorf("gitcli: UnpackObjects: open pack: %w", err)
	}
	defer f.Close()
	cmd := exec.CommandContext(ctx, bin, "-C", dir, "unpack-objects", "-q")
	cmd.Env = scrubGitRepoEnv(os.Environ())
	cmd.Stdin = f
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gitcli: UnpackObjects: %w: stderr=%q", err, stderr.String())
	}
	return nil
}

// RunForTest runs git in dir with the given args and returns combined
// output. Tests pass GIT_AUTHOR/COMMITTER env identity inline via -c
// flags. Production code should NOT use this; use the typed wrappers.
func RunForTest(dir string, args ...string) ([]byte, error) {
	bin, err := resolveBinary()
	if err != nil {
		return nil, err
	}
	full := append([]string{"-C", dir}, args...)
	cmd := exec.Command(bin, full...)
	cmd.Env = scrubGitRepoEnv(os.Environ())
	out, err := cmd.CombinedOutput()
	return out, err
}
