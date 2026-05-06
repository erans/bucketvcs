// Package gitcli provides thin, well-tested wrappers around the upstream
// `git` binary. M2 import/export and the differential harness use these
// for Track A operations (shell out to git for plumbing). A single git
// binary path is resolved once at first use; tests may override it via
// SetBinaryForTest.
package gitcli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
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

// urlCredsPattern matches URL userinfo (the segment between `scheme://`
// and `@`). This catches both `user:password@host` and token-only forms
// like `TOKEN@host` (common for HTTPS git remotes that embed a PAT).
var urlCredsPattern = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)([^@/\s]+)@`)

// redactCreds replaces any URL userinfo (the segment before `@`) with
// REDACTED in s. Unchanged for strings that contain no scheme://...@.
func redactCreds(s string) string {
	return urlCredsPattern.ReplaceAllString(s, "${1}REDACTED@")
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
		e.cmd, redactCreds(args), dir, e.exit, e.cause, redactCreds(e.stderr))
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

// validRefOrOID reports whether s is a safe value to pass to git as a
// ref name or object ID — i.e., it doesn't look like a flag and doesn't
// contain whitespace. This is a defensive check against caller-supplied
// strings that might begin with `-`.
func validRefOrOID(s string) bool {
	if s == "" {
		return false
	}
	if s[0] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == 0 {
			return false
		}
	}
	return true
}

// validHexOID reports whether s is a strict lowercase-hex object ID of
// SHA-1 (40 chars) or SHA-256 (64 chars) length. Unlike validRefOrOID it
// rejects any character that is not [0-9a-f], which makes it safe to feed
// untrusted client input to git commands that consume revision lists on
// stdin (e.g. `git pack-objects --revs`), where leading dashes, newlines,
// or revision-syntax (`^`, `..`, `:`, `~`) could otherwise inject extra
// revs or options.
func validHexOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
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
	args := []string{"--no-replace-objects", "fsck"}
	if strict {
		args = append(args, "--strict")
	}
	_, err := run(ctx, dir, args...)
	return err
}

// CloneBareMirror runs `git clone --bare --mirror <src> <dst>`. dst must
// not already exist (git creates it).
func CloneBareMirror(ctx context.Context, src, dst string) error {
	_, err := run(ctx, "", "clone", "--bare", "--mirror", "--quiet", "--", src, dst)
	return err
}

// PackObjectsAll produces a single pack containing every reachable
// object in dir, written as `outPrefix-{pack_id}.pack` (and the
// corresponding `outPrefix-{pack_id}.idx`). Returns the pack_id (40-
// char hex SHA-1, the Git-native pack name from §3.2 of the M2
// design). The function pipes `git rev-list --all --objects` into
// `git pack-objects` to keep behavior deterministic across git
// versions.
//
// Returns an error if pack-objects produces zero packs (empty repo)
// or splits the output across multiple packs; bucketvcs callers are
// expected to ensure the input fits in one pack.
func PackObjectsAll(ctx context.Context, dir, outPrefix string) (string, error) {
	bin, err := resolveBinary()
	if err != nil {
		return "", err
	}
	// Use an explicit os.Pipe so we control the close ordering. Using
	// StdoutPipe + Run/Wait would close the read end (consumed by
	// pack-objects) when rev-list exits, racing with pack-objects'
	// remaining reads from the kernel pipe buffer.
	pr, pw, err := os.Pipe()
	if err != nil {
		return "", fmt.Errorf("gitcli: PackObjectsAll: pipe: %w", err)
	}

	revList := exec.CommandContext(ctx, bin, "-C", dir, "--no-replace-objects", "rev-list", "--all", "--objects")
	revList.Env = scrubGitRepoEnv(os.Environ())
	revList.Stdout = pw
	var rlStderr bytes.Buffer
	revList.Stderr = &rlStderr

	pack := exec.CommandContext(ctx, bin, "-C", dir, "--no-replace-objects", "pack-objects", "--quiet", outPrefix)
	pack.Env = scrubGitRepoEnv(os.Environ())
	pack.Stdin = pr
	var packStdout, packStderr bytes.Buffer
	pack.Stdout = &packStdout
	pack.Stderr = &packStderr

	// Start pack-objects first so it's ready to consume; then start
	// rev-list to feed it.
	if err := pack.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return "", fmt.Errorf("gitcli: PackObjectsAll: pack start: %w", err)
	}
	// pack now owns its own dup of pr; close the parent's copy so when
	// pack exits the read side is fully closed (lets rev-list get SIGPIPE
	// if pack dies first).
	_ = pr.Close()
	if err := revList.Start(); err != nil {
		_ = pw.Close()
		_ = pack.Wait()
		return "", fmt.Errorf("gitcli: PackObjectsAll: rev-list start: %w", err)
	}
	// rev-list now owns its own dup of pw; close the parent's copy so
	// when rev-list exits the write side is fully closed and pack sees EOF.
	_ = pw.Close()
	rlErr := revList.Wait()
	packErr := pack.Wait()

	if rlErr != nil {
		return "", fmt.Errorf("gitcli: PackObjectsAll: rev-list: %w: stderr=%q",
			rlErr, redactCreds(rlStderr.String()))
	}
	if packErr != nil {
		return "", fmt.Errorf("gitcli: PackObjectsAll: pack-objects: %w: stderr=%q",
			packErr, redactCreds(packStderr.String()))
	}
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
	if packPath == "" || packPath[0] == '-' {
		return fmt.Errorf("gitcli: IndexPack: invalid packPath %q", packPath)
	}
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
		return fmt.Errorf("gitcli: UnpackObjects: %w: stderr=%q", err, redactCreds(stderr.String()))
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

// UpdateRef runs `git update-ref <ref> <oid>` in dir.
func UpdateRef(ctx context.Context, dir, ref, oid string) error {
	if !validRefOrOID(ref) {
		return fmt.Errorf("gitcli: UpdateRef: invalid ref %q", ref)
	}
	if !validRefOrOID(oid) {
		return fmt.Errorf("gitcli: UpdateRef: invalid oid %q", oid)
	}
	_, err := run(ctx, dir, "update-ref", "--", ref, oid)
	return err
}

// SymbolicRef returns the target of a symbolic ref (e.g. "HEAD").
func SymbolicRef(ctx context.Context, dir, name string) (string, error) {
	if !validRefOrOID(name) {
		return "", fmt.Errorf("gitcli: SymbolicRef: invalid ref name %q", name)
	}
	out, err := run(ctx, dir, "symbolic-ref", "--", name)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// SymbolicRefSet sets the target of a symbolic ref (e.g. HEAD ->
// refs/heads/main). target must be a full ref name.
func SymbolicRefSet(ctx context.Context, dir, name, target string) error {
	if !validRefOrOID(name) {
		return fmt.Errorf("gitcli: SymbolicRefSet: invalid name %q", name)
	}
	if !validRefOrOID(target) {
		return fmt.Errorf("gitcli: SymbolicRefSet: invalid target %q", target)
	}
	_, err := run(ctx, dir, "symbolic-ref", "--", name, target)
	return err
}

// ShowRef returns the map of full ref name -> 40-char hex OID for every
// ref under refs/. HEAD and other symbolic refs are not included; use
// SymbolicRef separately.
func ShowRef(ctx context.Context, dir string) (map[string]string, error) {
	out, err := run(ctx, dir, "show-ref")
	if err != nil {
		// `git show-ref` exits non-zero on a repo with no refs. The
		// stderr is empty in that case (modern git); older versions
		// may emit nothing as well. Treat exit==1 with empty stderr
		// as "no refs."
		//
		// TODO(M-later): consider migrating to `git for-each-ref` which
		// exits 0 with empty stdout on no refs and is documented to
		// never warn — would side-step this heuristic entirely.
		var rerr *runError
		if errors.As(err, &rerr) && rerr.exit == 1 && rerr.stderr == "" {
			return map[string]string{}, nil
		}
		return nil, err
	}
	refs := make(map[string]string)
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 || len(parts[0]) != 40 {
			return nil, fmt.Errorf("gitcli: ShowRef: malformed line %q", line)
		}
		refs[parts[1]] = parts[0]
	}
	return refs, nil
}

// RevListAllObjects returns every reachable object ID in dir, as 40-char
// hex strings. Equivalent to `git rev-list --all --objects` but stripped
// of trailing path metadata.
func RevListAllObjects(ctx context.Context, dir string) ([]string, error) {
	out, err := run(ctx, dir, "--no-replace-objects", "rev-list", "--all", "--objects")
	if err != nil {
		return nil, err
	}
	var oids []string
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		// Each line is "<oid>" or "<oid> <path-or-tagname>" (root tree
		// has empty path; first-space split still yields the OID).
		oid := line
		if sp := strings.IndexByte(line, ' '); sp != -1 {
			oid = line[:sp]
		}
		if len(oid) != 40 {
			return nil, fmt.Errorf("gitcli: RevListAllObjects: bad oid %q", oid)
		}
		oids = append(oids, oid)
	}
	return oids, nil
}

// RevParse resolves an arbitrary ref-like expression to its OID.
// Used to dereference HEAD on detached-HEAD repos.
func RevParse(ctx context.Context, dir, expr string) (string, error) {
	if !validRefOrOID(expr) {
		return "", fmt.Errorf("gitcli: RevParse: invalid expr %q", expr)
	}
	out, err := run(ctx, dir, "rev-parse", expr)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(out))
	if len(s) != 40 {
		return "", fmt.Errorf("gitcli: RevParse: unexpected output %q", s)
	}
	return s, nil
}

// CatFilePretty returns the pretty-printed bytes for an object, matching
// `git cat-file -p <oid>`.
func CatFilePretty(ctx context.Context, dir, oid string) ([]byte, error) {
	if !validRefOrOID(oid) {
		return nil, fmt.Errorf("gitcli: CatFilePretty: invalid oid %q", oid)
	}
	return run(ctx, dir, "--no-replace-objects", "cat-file", "-p", oid)
}

// CatFileType returns the type ("commit", "tree", "blob", "tag") for an
// object, matching `git cat-file -t <oid>`.
func CatFileType(ctx context.Context, dir, oid string) (string, error) {
	if !validRefOrOID(oid) {
		return "", fmt.Errorf("gitcli: CatFileType: invalid oid %q", oid)
	}
	out, err := run(ctx, dir, "--no-replace-objects", "cat-file", "-t", oid)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// CatFileSize returns the size of an object's content, matching
// `git cat-file -s <oid>`.
func CatFileSize(ctx context.Context, dir, oid string) (int64, error) {
	if !validRefOrOID(oid) {
		return 0, fmt.Errorf("gitcli: CatFileSize: invalid oid %q", oid)
	}
	out, err := run(ctx, dir, "--no-replace-objects", "cat-file", "-s", oid)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(out))
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("gitcli: CatFileSize: parse %q: %w", s, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("gitcli: CatFileSize: negative size %d", n)
	}
	return n, nil
}

// CheckRefFormat returns nil if name is a valid Git ref name per
// `git check-ref-format <name>`, or an error describing why git
// rejected it.
func CheckRefFormat(ctx context.Context, name string) error {
	if !validRefOrOID(name) {
		return fmt.Errorf("gitcli: CheckRefFormat: invalid name %q", name)
	}
	_, err := run(ctx, "", "check-ref-format", name)
	return err
}

// PackForFetchOptions configures PackObjectsForFetch.
type PackForFetchOptions struct {
	Wants       []string
	Haves       []string // ^<oid> exclusion list
	ThinPack    bool
	IncludeTag  bool
	OfsDelta    bool
	NoProgress  bool   // suppress stderr-as-progress
	ShallowFile string // optional path to a shallow file (passed via GIT_SHALLOW_FILE)
	// Depth is informational only — actual depth handling for shallow fetches
	// is via the ShallowFile contents written by the caller before invocation.
	// Stored for caller bookkeeping; not consumed by this package.
	Depth int
}

// PackObjectsForFetch invokes "git pack-objects --revs --stdout" against the
// bare repo at dir, feeding wants and ^haves via stdin and returning an
// io.ReadCloser over the resulting pack stream. The caller MUST Close() the
// returned reader (which waits for git to exit and surfaces nonzero exit
// status as an error).
//
// Wants/haves are validated as strict hex object IDs before invocation;
// callers do NOT need to pre-sanitize for shell metacharacters.
//
// SECURITY — REACHABILITY IS THE CALLER'S JOB. This wrapper does NOT verify
// that wants are advertised, reachable from advertised refs, or otherwise
// authorized. `git pack-objects --revs` will happily pack any object the
// caller names if it exists in the repo, so a client that knows or guesses
// an OID could exfiltrate hidden objects. Callers (the v2proto fetch
// handler / gateway) MUST enforce upload-pack-style allow-tip / allow-reachable-sha1
// semantics against an advertised want set BEFORE handing OIDs to this
// function. See M3 Tasks 14-15 (gateway info/refs + git-upload-pack).
//
// Any output on stderr is captured into the returned error on close-failure;
// it is NOT streamed to a side-band by this layer (the caller wraps it).
func PackObjectsForFetch(ctx context.Context, dir string, opts PackForFetchOptions) (io.ReadCloser, error) {
	bin, err := resolveBinary()
	if err != nil {
		return nil, err
	}
	// Strict OID validation: wants/haves are written to `git pack-objects
	// --revs` stdin. They may be client-controlled, so reject anything that
	// is not a strict hex OID — otherwise newlines, leading dashes, or
	// revision syntax (^, .., :, ~) could inject extra revs or exclusions
	// and cause git to pack unintended objects.
	for i, w := range opts.Wants {
		if !validHexOID(w) {
			return nil, fmt.Errorf("pack-objects: invalid want[%d] %q (must be hex object id)", i, w)
		}
	}
	for i, h := range opts.Haves {
		if !validHexOID(h) {
			return nil, fmt.Errorf("pack-objects: invalid have[%d] %q (must be hex object id)", i, h)
		}
	}
	args := []string{"--no-replace-objects", "pack-objects", "--revs", "--stdout"}
	if opts.ThinPack {
		args = append(args, "--thin")
	}
	if opts.IncludeTag {
		args = append(args, "--include-tag")
	}
	if opts.OfsDelta {
		args = append(args, "--delta-base-offset")
	}
	if opts.NoProgress {
		args = append(args, "-q")
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	env := scrubGitRepoEnv(os.Environ())
	if opts.ShallowFile != "" {
		// GIT_SHALLOW_FILE is the documented, supported mechanism for
		// pointing pack-objects (and other plumbing) at an alternate
		// shallow boundary. The previously used `-c core.shallow=...`
		// is silently ignored by git — there is no such config key.
		env = append(env, "GIT_SHALLOW_FILE="+opts.ShallowFile)
	}
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("pack-objects: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("pack-objects: stdout pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("pack-objects: start: %w", err)
	}

	go func() {
		defer stdin.Close()
		bw := bufio.NewWriter(stdin)
		for _, w := range opts.Wants {
			fmt.Fprintf(bw, "%s\n", w)
		}
		for _, h := range opts.Haves {
			fmt.Fprintf(bw, "^%s\n", h)
		}
		_ = bw.Flush()
	}()

	return &packObjectsReader{r: stdout, cmd: cmd, dir: dir, stderr: &stderr}, nil
}

type packObjectsReader struct {
	r      io.ReadCloser
	cmd    *exec.Cmd
	dir    string
	stderr *bytes.Buffer
}

func (p *packObjectsReader) Read(b []byte) (int, error) { return p.r.Read(b) }

func (p *packObjectsReader) Close() error {
	closeErr := p.r.Close()
	waitErr := p.cmd.Wait()
	if waitErr != nil {
		exit := -1
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			exit = ee.ExitCode()
		}
		return &runError{
			cmd:    p.cmd.Path,
			args:   p.cmd.Args[1:],
			dir:    p.dir,
			exit:   exit,
			cause:  waitErr,
			stderr: redactCreds(p.stderr.String()),
		}
	}
	return closeErr
}

// RevParseObjectKind returns the object type ("commit", "tag", "tree",
// "blob") for the OID in the bare repo at dir, or an error if the object is
// missing or unparseable. The oid argument MUST be a strict hex object ID
// (40-char SHA-1 or 64-char SHA-256, lowercase). Ref names, short OIDs, and
// revision syntax (HEAD, main, HEAD^{tree}, HEAD:path, etc.) are rejected
// — callers that want to resolve a name to a kind must first translate it
// to an OID via a path that performs its own authorization.
func RevParseObjectKind(ctx context.Context, dir, oid string) (string, error) {
	if !validHexOID(oid) {
		return "", fmt.Errorf("rev-parse: invalid oid %q (must be hex object id)", oid)
	}
	out, err := run(ctx, dir, "--no-replace-objects", "cat-file", "-t", oid)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
