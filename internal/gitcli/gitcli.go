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
	"path/filepath"
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

// validRevPath permits the "<oid>:<path>" rev form used by code browse, where
// the path component may contain spaces. It rejects a leading '-' (flag
// injection) and any NUL/CR/LF, but allows spaces because the value is always
// passed as a single argv element. It is intentionally more permissive than
// validRefOrOID, which guards bare ref/OID args.
func validRevPath(s string) bool {
	if s == "" || s[0] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case 0x00, '\n', '\r':
			return false
		}
	}
	return true
}

// validPackBasename returns true iff s matches exactly `pack-<40hex>.pack`.
// This is the canonical form pack files take inside a bare repo's
// `objects/pack/` directory (set by `git index-pack` and by our
// exporter.downloadAndIndexPack). Strict matching prevents an
// attacker-influenced KeepPacks value from injecting option syntax via
// `--keep-pack=<value>` argv elements.
func validPackBasename(s string) bool {
	const prefix, suffix = "pack-", ".pack"
	if len(s) != len(prefix)+40+len(suffix) {
		return false
	}
	if !strings.HasPrefix(s, prefix) || !strings.HasSuffix(s, suffix) {
		return false
	}
	return validHexOID(s[len(prefix) : len(prefix)+40])
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

// PackObjectsAllWithBitmap is similar to PackObjectsAll but additionally
// writes a `.bitmap` sidecar via --write-bitmap-index. Produces three
// output files: <outPrefix>-<id>.pack, .idx, and .bitmap. Returns the
// pack ID.
//
// Implementation note: --write-bitmap-index is only valid when
// pack-objects walks reachability itself (--all / --revs / etc.), NOT
// when the object set comes from stdin via rev-list. So this variant
// invokes `pack-objects --revs --all --write-bitmap-index` directly
// without the rev-list pipeline. The resulting pack covers the same
// object set as PackObjectsAll on the same repo, but the encoding
// (delta selection, ordering) and therefore the pack ID / pack-checksum
// trailer WILL differ between the two functions — pack-objects'
// internal walker chooses deltas differently than feeding it a flat
// rev-list --all --objects stream. Callers that compare maintenance
// outputs across milestones, or any reproducibility harness, MUST
// account for this: pack IDs are not stable across the switch from
// PackObjectsAll to PackObjectsAllWithBitmap.
//
// Empty-repo behavior also differs from PackObjectsAll: this variant
// produces the well-known empty-pack hash (029d088…) and exits
// cleanly, with NO .bitmap sidecar. PackObjectsAll on the same input
// also returns 40-char output via stdin piping. Callers MUST tolerate
// a missing .bitmap file regardless.
func PackObjectsAllWithBitmap(ctx context.Context, dir, outPrefix string) (string, error) {
	// Empty-repo portability note: on the git versions bucketvcs is
	// tested against (>= 2.41), `pack-objects --revs --all` on a bare
	// repo with zero refs prints the well-known empty-pack hash and
	// exits 0. A future git version that instead emits no stdout
	// would trip the `len(id) != 40` branch below — surfacing as a
	// hard error to the caller. This is a theoretical concern today
	// because maintenance never materializes an empty bare (the
	// manifest always carries at least one ref before reaching the
	// Repack phase); we accept the empty-pack-hash path because it
	// matches PackObjectsAll's behavior on the same input.
	bin, err := resolveBinary()
	if err != nil {
		return "", err
	}
	pack := exec.CommandContext(ctx, bin,
		"-C", dir, "--no-replace-objects",
		"pack-objects", "--quiet", "--revs", "--all", "--write-bitmap-index", outPrefix)
	pack.Env = scrubGitRepoEnv(os.Environ())
	var packStdout, packStderr bytes.Buffer
	pack.Stdout = &packStdout
	pack.Stderr = &packStderr
	if err := pack.Run(); err != nil {
		return "", fmt.Errorf("gitcli: PackObjectsAllWithBitmap: pack-objects: %w: stderr=%q",
			err, redactCreds(packStderr.String()))
	}
	id := strings.TrimSpace(packStdout.String())
	if len(id) != 40 {
		return "", fmt.Errorf("gitcli: PackObjectsAllWithBitmap: unexpected pack-objects stdout %q",
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
// object, matching `git cat-file -t <oid>`. The oid may also be a
// "<oid>:<path>" rev (path may contain spaces) — guarded by validRevPath
// rather than validRefOrOID deliberately, for the code-browse reader.
func CatFileType(ctx context.Context, dir, oid string) (string, error) {
	if !validRevPath(oid) {
		return "", fmt.Errorf("gitcli: CatFileType: invalid oid %q", oid)
	}
	out, err := run(ctx, dir, "--no-replace-objects", "cat-file", "-t", oid)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// CatFileSize returns the size of an object's content, matching
// `git cat-file -s <oid>`. Like CatFileType, accepts "<oid>:<path>" revs
// (validRevPath) deliberately, for the code-browse reader.
func CatFileSize(ctx context.Context, dir, oid string) (int64, error) {
	if !validRevPath(oid) {
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
	// KeepPacks is a list of pack basenames (e.g. "pack-<40hex>.pack")
	// forwarded to `git pack-objects --keep-pack=<name>`. Objects already
	// present in any named pack are excluded from the output stream even
	// if they would otherwise be packed. Used by M11 Phase 8.2's
	// packfile-uri path: when the server advertises a pack URL for an
	// existing canonical pack, the inline pack returned in the same
	// response MUST NOT contain those objects — otherwise the client's
	// http-fetch -> index-pack pipeline observes a no-new-objects pack
	// and fetch-pack errors with "expected keep then TAB at start of
	// http-fetch output" (a known git fetch-pack bug; see git's
	// b664e9ffa1).
	//
	// Each entry MUST match `pack-<40hex>.pack` exactly; arbitrary path
	// fragments are rejected to prevent option-value injection from a
	// caller that hands us a partly-controlled string.
	KeepPacks []string
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
	// Strict allowlist validation of KeepPacks: each entry must match
	// `pack-<40hex>.pack` exactly. The basename is forwarded verbatim to
	// `git pack-objects --keep-pack=<name>` as a single shell-safe argv
	// element, but a partly-attacker-controlled value containing `=`,
	// `--`, or path separators could still confuse downstream tooling
	// that re-parses the argv string (e.g. an operator running `ps`).
	// Rejecting non-conforming values keeps every supported input
	// canonical.
	for i, kp := range opts.KeepPacks {
		if !validPackBasename(kp) {
			return nil, fmt.Errorf("pack-objects: invalid keep-pack[%d] %q (must match pack-<40hex>.pack)", i, kp)
		}
		args = append(args, "--keep-pack="+kp)
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

// UpdateRefDelete deletes ref atomically, asserting that ref currently
// resolves to oldOID. Equivalent to "git update-ref -d <ref> <oldOID>".
//
// Stub for M3 Task 9 (mirror.IngestPack); Task 10 promotes this with
// dedicated tests and any additional hardening the push path needs.
func UpdateRefDelete(ctx context.Context, dir, ref, oldOID string) error {
	if !validRefOrOID(ref) {
		return fmt.Errorf("gitcli: UpdateRefDelete: invalid ref %q", ref)
	}
	if !validRefOrOID(oldOID) {
		return fmt.Errorf("gitcli: UpdateRefDelete: invalid oldOID %q", oldOID)
	}
	_, err := run(ctx, dir, "update-ref", "-d", "--", ref, oldOID)
	return err
}

// UpdateRefCAS updates ref to newOID, asserting it currently resolves to
// oldOID. Equivalent to "git update-ref <ref> <newOID> <oldOID>".
//
// Stub for M3 Task 9 (mirror.IngestPack); Task 10 promotes this with
// dedicated tests and any additional hardening the push path needs.
func UpdateRefCAS(ctx context.Context, dir, ref, newOID, oldOID string) error {
	if !validRefOrOID(ref) {
		return fmt.Errorf("gitcli: UpdateRefCAS: invalid ref %q", ref)
	}
	if !validRefOrOID(newOID) {
		return fmt.Errorf("gitcli: UpdateRefCAS: invalid newOID %q", newOID)
	}
	if !validRefOrOID(oldOID) {
		return fmt.Errorf("gitcli: UpdateRefCAS: invalid oldOID %q", oldOID)
	}
	_, err := run(ctx, dir, "update-ref", "--", ref, newOID, oldOID)
	return err
}

// IndexPackStrict runs `git index-pack --stdin --strict --fix-thin --keep`
// against the bare repo at dir, streaming packPath via stdin. The pack is
// written into dir/objects/pack/pack-<hash>.{pack,idx,keep}. Returns the
// path to the produced .idx file.
//
// The .keep file is intentionally left in place to prevent `git gc` /
// `git repack` from racing the caller; callers MUST remove the .keep file
// after they have either advertised the new objects via a ref update
// (mirror.IngestPack) or decided to abandon the pack. `--fix-thin` requires
// `--stdin`, so this helper unconditionally streams the input file rather
// than passing it as a positional argument.
func IndexPackStrict(ctx context.Context, dir, packPath string) (string, error) {
	if packPath == "" || packPath[0] == '-' {
		return "", fmt.Errorf("gitcli: IndexPackStrict: invalid packPath %q", packPath)
	}
	bin, err := resolveBinary()
	if err != nil {
		return "", err
	}
	f, err := os.Open(packPath)
	if err != nil {
		return "", fmt.Errorf("gitcli: IndexPackStrict: open pack: %w", err)
	}
	defer f.Close()
	cmd := exec.CommandContext(ctx, bin, "-C", dir,
		"--no-replace-objects", "index-pack",
		"--stdin", "--strict", "--fix-thin", "--keep")
	cmd.Env = scrubGitRepoEnv(os.Environ())
	cmd.Stdin = f
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		exit := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exit = ee.ExitCode()
		}
		return "", &runError{
			cmd:    bin,
			args:   []string{"-C", dir, "--no-replace-objects", "index-pack", "--stdin", "--strict", "--fix-thin", "--keep"},
			dir:    dir,
			exit:   exit,
			cause:  err,
			stderr: stderr.String(),
		}
	}
	// Stdout is "<pack-hash>\n" without --keep, "keep\t<pack-hash>\n"
	// when --keep is in effect and the pack is newly indexed, or
	// "pack\t<pack-hash>\n" when --keep is requested but the pack file
	// already exists in objects/pack (git 2.30+; e.g. observed after a
	// receive-pack precheck rejection leaves the pack on disk and a
	// subsequent retry re-indexes the same bytes). Strip either prefix
	// and validate.
	hash := strings.TrimSpace(stdout.String())
	hash = strings.TrimPrefix(hash, "keep\t")
	hash = strings.TrimPrefix(hash, "pack\t")
	if !validHexOID(hash) {
		return "", fmt.Errorf("gitcli: IndexPackStrict: unexpected stdout %q", stdout.String())
	}
	idx := filepath.Join(dir, "objects", "pack", "pack-"+hash+".idx")
	if _, err := os.Stat(idx); err != nil {
		return "", fmt.Errorf("gitcli: IndexPackStrict: idx not produced at %s: %w", idx, err)
	}
	return idx, nil
}

// RevListNotAll runs `git rev-list --objects <oids...> --not --all` against
// the bare repo at dir. Returns the list of OIDs that are reachable from
// the given oids but NOT from any current ref. After a successful
// IndexPackStrict, the inbound pack's contents should be the only members
// of this set; if any "missing" OIDs from the pack don't appear, connectivity
// is intact.
//
// In the validation flow we typically just want "any output ⇒ unreachable
// objects exist"; the caller intersects with pack contents to detect
// missing parents/trees/blobs.
func RevListNotAll(ctx context.Context, dir string, oids []string) ([]string, error) {
	for _, o := range oids {
		if !validHexOID(o) {
			return nil, fmt.Errorf("gitcli: RevListNotAll: invalid oid %q", o)
		}
	}
	args := []string{"--no-replace-objects", "rev-list", "--objects"}
	args = append(args, oids...)
	args = append(args, "--not", "--all")
	out, err := run(ctx, dir, args...)
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil, nil
	}
	var found []string
	for _, line := range strings.Split(trimmed, "\n") {
		if line == "" {
			continue
		}
		// "<oid> [<path>]"
		if i := strings.IndexByte(line, ' '); i >= 0 {
			found = append(found, line[:i])
		} else {
			found = append(found, line)
		}
	}
	return found, nil
}

// RevListCommitsOnly runs `git rev-list <tips> [excludes...]` in dir, emitting
// only commit OIDs (no --objects flag, so trees and blobs are excluded).
// excludes should be in "^<oid>" form. Useful when the caller only needs commits
// (e.g. building a commit-graph delta) and wants to avoid per-object type probes.
func RevListCommitsOnly(ctx context.Context, dir string, tips, excludes []string) ([]string, error) {
	for _, o := range tips {
		if !validHexOID(o) {
			return nil, fmt.Errorf("gitcli: RevListCommitsOnly: invalid tip oid %q", o)
		}
	}
	for i, ex := range excludes {
		if len(ex) != 41 || ex[0] != '^' {
			return nil, fmt.Errorf("gitcli: RevListCommitsOnly: invalid exclude %q at index %d", ex, i)
		}
		if !validHexOID(ex[1:]) {
			return nil, fmt.Errorf("gitcli: RevListCommitsOnly: invalid exclude OID %q at index %d", ex, i)
		}
	}
	args := []string{"--no-replace-objects", "rev-list"}
	args = append(args, tips...)
	args = append(args, excludes...)
	out, err := run(ctx, dir, args...)
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil, nil
	}
	var found []string
	for _, line := range strings.Split(trimmed, "\n") {
		if line == "" {
			continue
		}
		found = append(found, line)
	}
	return found, nil
}

// FsckConnectivityOnly runs `git fsck --connectivity-only --no-dangling
// --no-progress` against dir. Used as a defensive double-check after
// IndexPackStrict.
func FsckConnectivityOnly(ctx context.Context, dir string) error {
	_, err := run(ctx, dir, "--no-replace-objects", "fsck", "--connectivity-only", "--no-dangling", "--no-progress")
	return err
}

// nullOIDHex is the all-zero OID used to mark ref creation/deletion in
// the receive-pack protocol.
const nullOIDHex = "0000000000000000000000000000000000000000"

// maxNewRefDiffPaths caps the number of paths returned when oldOID is
// the null OID (ref creation). Above the cap, DiffTreeChangedPaths
// returns ErrTooManyPaths.
const maxNewRefDiffPaths = 10000

// ErrTooManyPaths is returned by DiffTreeChangedPaths when a new-ref
// creation introduces more paths than maxNewRefDiffPaths. Receivepack
// treats this as an internal-error rejection with operator-friendly
// "squash or rebase" guidance.
var ErrTooManyPaths = errors.New("gitcli: new-ref diff exceeds cap; squash or rebase")

// DiffTreeChangedPaths returns the union of paths touched by commits in
// the range (oldOID, newOID]. Single `git log --name-only` subprocess
// regardless of commit count.
//
// Special cases:
//   - newOID == nullOIDHex (deletion): returns nil, nil
//   - oldOID == nullOIDHex (creation): walks every commit reachable from
//     newOID (`--root` so the initial commit's create-event is included)
//     with cap maxNewRefDiffPaths; returns ErrTooManyPaths on cap
//   - oldOID == newOID (no-op): returns nil, nil
//
// Path order matches `git log` output; duplicates collapsed.
func DiffTreeChangedPaths(ctx context.Context, dir, oldOID, newOID string) ([]string, error) {
	if !validHexOID(oldOID) {
		return nil, fmt.Errorf("gitcli: DiffTreeChangedPaths: invalid oldOID %q", oldOID)
	}
	if !validHexOID(newOID) {
		return nil, fmt.Errorf("gitcli: DiffTreeChangedPaths: invalid newOID %q", newOID)
	}
	if newOID == nullOIDHex || oldOID == newOID {
		return nil, nil
	}
	// We use `git log --name-only --pretty=format:` so the union of paths
	// touched across all commits in the range is enumerated — a single
	// subprocess regardless of commit count. `git diff-tree A..B` would
	// only give the endpoint-to-endpoint diff, missing intermediate
	// changes that were later reverted but still subject to policy.
	var args []string
	if oldOID == nullOIDHex {
		// New ref: walk every commit reachable from newOID. `--root`
		// makes the initial commit show as a big create event so its
		// files are listed too. `-m --first-parent` ensures merge
		// commits emit a diff against their first parent (mainline),
		// closing the conflict-resolution bypass where a file
		// introduced only on the merge commit M (present on neither
		// parent) would silently escape the path-policy gate.
		// `--first-parent` constrains the diff to a single parent so
		// paths are not duplicated N times for an N-parent merge.
		args = []string{
			"log", "--root", "-m", "--first-parent", "--name-only",
			"--pretty=format:", "--diff-filter=ACMDRT", newOID,
		}
	} else {
		// Range push: walk commits in (oldOID, newOID].
		// See above re: `-m --first-parent` for merge-commit handling.
		args = []string{
			"log", "-m", "--first-parent", "--name-only",
			"--pretty=format:", "--diff-filter=ACMDRT",
			oldOID + ".." + newOID,
		}
	}
	out, err := run(ctx, dir, args...)
	if err != nil {
		return nil, fmt.Errorf("gitcli: diff-tree %s..%s: %w",
			shortOID(oldOID), shortOID(newOID), err)
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil, nil
	}
	lines := strings.Split(trimmed, "\n")
	seen := make(map[string]struct{}, len(lines))
	var paths []string
	capped := false
	for _, line := range lines {
		if line == "" {
			continue
		}
		if _, dup := seen[line]; dup {
			continue
		}
		seen[line] = struct{}{}
		paths = append(paths, line)
		if oldOID == nullOIDHex && len(paths) > maxNewRefDiffPaths {
			capped = true
			break
		}
	}
	if capped {
		return nil, ErrTooManyPaths
	}
	return paths, nil
}

// shortOID returns the first 12 hex chars of oid for error messages.
func shortOID(oid string) string {
	if len(oid) > 12 {
		return oid[:12]
	}
	return oid
}

// ErrOutputCapped is returned (wrapped) by capped helpers when git produced
// more stdout than the helper's byte cap. The returned bytes are the prefix
// captured before the cap was hit.
var ErrOutputCapped = errors.New("gitcli: output exceeded cap")

// cappedWriter stores up to cap bytes and sets capped=true on overflow so the
// exec copy loop aborts (killing git via the write error) instead of streaming
// an unbounded payload into memory. The capped flag is inspected after
// cmd.Run() returns rather than relying on error propagation, because
// cmd.Run()/Wait() prefers the process exit error (git SIGPIPE, exit non-zero)
// over the goroutine copy error when both occur simultaneously.
type cappedWriter struct {
	buf    bytes.Buffer
	cap    int64
	capped bool
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	remain := w.cap - int64(w.buf.Len())
	if remain <= 0 {
		w.capped = true
		return 0, ErrOutputCapped
	}
	if int64(len(p)) <= remain {
		return w.buf.Write(p)
	}
	n, _ := w.buf.Write(p[:remain])
	w.capped = true
	return n, ErrOutputCapped
}

// runCapped is run() with a stdout byte cap. On overflow it returns the
// captured prefix together with an error wrapping ErrOutputCapped; other
// failures behave like run(). The cap is enforced by cappedWriter; when the
// cap is hit the write error propagates to the io.Copy goroutine, causing git
// to receive SIGPIPE and exit non-zero. cmd.Run()/Wait() prefers the process
// exit error over the goroutine copy error, so we check cappedWriter.capped
// directly instead of relying on errors.Is(err, ErrOutputCapped).
func runCapped(ctx context.Context, dir string, capBytes int64, args ...string) ([]byte, error) {
	bin, err := resolveBinary()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = scrubGitRepoEnv(os.Environ())
	out := &cappedWriter{cap: capBytes}
	var stderr bytes.Buffer
	cmd.Stdout = out
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if out.capped {
		return out.buf.Bytes(), fmt.Errorf("git %s: %w", args[0], ErrOutputCapped)
	}
	if runErr != nil {
		exit := -1
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			exit = ee.ExitCode()
		}
		return out.buf.Bytes(), &runError{
			cmd: bin, args: args, dir: dir, exit: exit,
			stderr: stderr.String(), cause: runErr,
		}
	}
	return out.buf.Bytes(), nil
}

// maxDiffPatchBytes caps the raw unified patch read from git diff-tree.
// Callers receive the prefix and ErrOutputCapped on overflow.
const maxDiffPatchBytes = 20 << 20 // 20 MiB

// maxLsTreeBytes caps the raw ls-tree output.
const maxLsTreeBytes = 32 << 20 // 32 MiB

// maxCommitObjBytes caps the raw commit object read from git cat-file commit.
const maxCommitObjBytes = 4 << 20 // 4 MiB

// LsTree returns the raw `git ls-tree --long -z <treeish>` output for a tree-ish.
// treeish is typically "<commitOID>" for the root tree or "<commitOID>:<dir>" for
// a subdirectory. Output is NUL-terminated records, each:
//
//	"<mode> SP <type> SP <oid> SP <size|-> TAB <name>" \0
//
// Output is capped at maxLsTreeBytes; overflow returns the prefix and
// ErrOutputCapped.
func LsTree(ctx context.Context, dir, treeish string) ([]byte, error) {
	if !validRevPath(treeish) {
		return nil, fmt.Errorf("gitcli: LsTree: invalid treeish %q", treeish)
	}
	return runCapped(ctx, dir, maxLsTreeBytes, "--no-replace-objects", "ls-tree", "--long", "-z", treeish)
}

// CatBlob returns raw blob bytes for a rev, matching `git cat-file blob <rev>`.
// rev is typically "<commitOID>:<path>".
func CatBlob(ctx context.Context, dir, rev string) ([]byte, error) {
	if !validRevPath(rev) {
		return nil, fmt.Errorf("gitcli: CatBlob: invalid rev %q", rev)
	}
	return run(ctx, dir, "--no-replace-objects", "cat-file", "blob", rev)
}

// LogRaw returns commit-log records for rev, paginated by skip/max. Each record
// is unit-separated (0x1f) fields terminated by a record separator (0x1e):
//
//	<full-oid> 0x1f <author-name> 0x1f <author-email> 0x1f <author-unixtime> 0x1f <subject> 0x1e
func LogRaw(ctx context.Context, dir, rev string, skip, max int) ([]byte, error) {
	if !validRefOrOID(rev) {
		return nil, fmt.Errorf("gitcli: LogRaw: invalid rev %q", rev)
	}
	if skip < 0 || max <= 0 {
		return nil, fmt.Errorf("gitcli: LogRaw: bad skip/max %d/%d", skip, max)
	}
	const format = "--pretty=format:%H%x1f%an%x1f%ae%x1f%at%x1f%s%x1e"
	return run(ctx, dir, "--no-replace-objects", "log", rev,
		fmt.Sprintf("--skip=%d", skip), fmt.Sprintf("--max-count=%d", max),
		"--no-color", format)
}

// LogRawPath is LogRaw scoped to a single pathspec (git log [--follow] <rev> -- <path>).
// Same record format as LogRaw. follow tracks renames and is valid only for a
// single file (git rejects --follow with a directory); callers must pass
// follow=false for directories. path is validated as a rev-path (rejects a
// leading '-' and NUL/CR/LF; spaces allowed).
func LogRawPath(ctx context.Context, dir, rev, path string, follow bool, skip, max int) ([]byte, error) {
	if !validRefOrOID(rev) {
		return nil, fmt.Errorf("gitcli: LogRawPath: invalid rev %q", rev)
	}
	if !validRevPath(path) {
		return nil, fmt.Errorf("gitcli: LogRawPath: invalid path %q", path)
	}
	if skip < 0 || max <= 0 {
		return nil, fmt.Errorf("gitcli: LogRawPath: bad skip/max %d/%d", skip, max)
	}
	const format = "--pretty=format:%H%x1f%an%x1f%ae%x1f%at%x1f%s%x1e"
	args := []string{"-c", "core.quotePath=false", "--no-replace-objects", "log", rev,
		fmt.Sprintf("--skip=%d", skip), fmt.Sprintf("--max-count=%d", max), "--no-color", format}
	if follow {
		args = append(args, "--follow")
	}
	args = append(args, "--", path)
	return run(ctx, dir, args...)
}

// PathKind reports the object kind ("blob"|"tree") of path at rev via
// `git cat-file -t <rev>:<path>`. A lookup failure (path absent at rev, etc.)
// returns "" plus the error; callers that only need it to choose --follow
// should treat any error as "not a blob" and proceed without following.
func PathKind(ctx context.Context, dir, rev, path string) (string, error) {
	spec := rev + ":" + path
	if !validRevPath(spec) {
		return "", fmt.Errorf("gitcli: PathKind: invalid spec %q", spec)
	}
	out, err := run(ctx, dir, "--no-replace-objects", "cat-file", "-t", spec)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// CatFileCommit returns the raw commit object bytes, matching
// `git cat-file commit <oid>` (headers: tree/parent/author/committer, blank line,
// then the message). Output is capped at maxCommitObjBytes; overflow returns the
// captured prefix and ErrOutputCapped (the header block comes first, so
// callers may still parse author/parent metadata from the prefix).
func CatFileCommit(ctx context.Context, dir, oid string) ([]byte, error) {
	if !validRefOrOID(oid) {
		return nil, fmt.Errorf("gitcli: CatFileCommit: invalid oid %q", oid)
	}
	return runCapped(ctx, dir, maxCommitObjBytes, "--no-replace-objects", "cat-file", "commit", oid)
}

// DiffTreePatch returns the unified patch for a commit. When parent is
// non-empty the diff is computed against that parent (two-tree form — required
// for merge commits, where bare `diff-tree -p <oid>` suppresses the patch);
// when parent is "" the commit is treated as a root and diffed against the
// empty tree via --root.
//
// Filenames in diff headers are emitted verbatim (no c-quoting) because
// core.quotePath is explicitly disabled via -c core.quotePath=false.
//
// Output is capped at maxDiffPatchBytes; overflow returns the captured prefix
// and ErrOutputCapped.
func DiffTreePatch(ctx context.Context, dir, oid, parent string) ([]byte, error) {
	if !validRefOrOID(oid) {
		return nil, fmt.Errorf("gitcli: DiffTreePatch: invalid oid %q", oid)
	}
	if parent != "" {
		if !validRefOrOID(parent) {
			return nil, fmt.Errorf("gitcli: DiffTreePatch: invalid parent %q", parent)
		}
		return runCapped(ctx, dir, maxDiffPatchBytes, "-c", "core.quotePath=false", "--no-replace-objects", "diff-tree", "-p", "-M",
			"--no-color", parent, oid)
	}
	return runCapped(ctx, dir, maxDiffPatchBytes, "-c", "core.quotePath=false", "--no-replace-objects", "diff-tree", "-p", "-M",
		"--root", "--no-color", oid)
}

// DiffRefsPatch returns the two-dot unified patch from base to head
// (git diff-tree -p -M base head): additions in head are '+', removals '-'.
// Both must be valid refs/OIDs. Filenames are emitted verbatim (quotePath off).
// Output is capped at maxDiffPatchBytes; overflow returns the captured prefix
// and ErrOutputCapped (callers parse the prefix and mark the result truncated).
func DiffRefsPatch(ctx context.Context, dir, base, head string) ([]byte, error) {
	if !validRefOrOID(base) {
		return nil, fmt.Errorf("gitcli: DiffRefsPatch: invalid base %q", base)
	}
	if !validRefOrOID(head) {
		return nil, fmt.Errorf("gitcli: DiffRefsPatch: invalid head %q", head)
	}
	return runCapped(ctx, dir, maxDiffPatchBytes, "-c", "core.quotePath=false", "--no-replace-objects",
		"diff-tree", "-p", "-M", "--no-color", base, head)
}

// maxLogNameStatusBytes caps the bounded attribution walk used by the web
// tree view's last-commit column.
const maxLogNameStatusBytes = 8 << 20 // 8 MiB

// LogNameStatus returns up to max commits reachable from oid with the paths
// each touched, as 0x1e-separated records:
//
//	0x1e <oid> 0x1f <author-name> 0x1f <author-email> 0x1f <unixtime> 0x1f <subject> \n
//	<STATUS>\t<path>\n ...
//
// Renames are disabled (--no-renames: a rename reports as A+D, so the rename
// commit is the path's last touch) and paths are emitted verbatim
// (core.quotePath=false). scopePath, when non-empty, restricts the walk to
// commits touching that directory. Output is byte-capped; on overflow the
// captured prefix is returned with an error wrapping ErrOutputCapped.
func LogNameStatus(ctx context.Context, dir, oid string, max int, scopePath string) ([]byte, error) {
	if !validRefOrOID(oid) {
		return nil, fmt.Errorf("gitcli: LogNameStatus: invalid oid %q", oid)
	}
	if max <= 0 {
		return nil, fmt.Errorf("gitcli: LogNameStatus: bad max %d", max)
	}
	args := []string{
		"-c", "core.quotePath=false", "--no-replace-objects", "log", oid,
		fmt.Sprintf("--max-count=%d", max), "--name-status", "--no-color",
		"--no-renames", "--pretty=format:%x1e%H%x1f%an%x1f%ae%x1f%at%x1f%s",
	}
	if scopePath != "" {
		if !validRevPath(scopePath) {
			return nil, fmt.Errorf("gitcli: LogNameStatus: invalid scope path %q", scopePath)
		}
		args = append(args, "--", scopePath+"/")
	}
	return runCapped(ctx, dir, maxLogNameStatusBytes, args...)
}
