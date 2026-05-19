package sshd

import (
	"errors"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/gateway/routenames"
)

// ExecOp is the SSH-side equivalent of gateway.Op for upload/receive.
// Receive-pack maps to ActionWrite; upload-pack to ActionRead.
type ExecOp int

const (
	OpUpload ExecOp = iota + 1
	OpReceive
	// OpLFSAuthenticate handles `git-lfs-authenticate <owner>/<repo>
	// <upload|download>`. The LFS sub-op lives on ExecCommand.LFSOp
	// because it controls the auth.Action mapping at dispatch time.
	OpLFSAuthenticate
)

// RequiredAction returns the auth.Action for this op.
//
// OpLFSAuthenticate defaults to ActionWrite (fail-closed): the SSH
// session dispatcher refines this to ActionRead for `download` via
// ExecCommand.LFSOp before calling auth.Decide. An unconfigured caller
// that ignores LFSOp therefore gets a write-level check, not a read.
func (o ExecOp) RequiredAction() auth.Action {
	switch o {
	case OpReceive, OpLFSAuthenticate:
		return auth.ActionWrite
	default:
		return auth.ActionRead
	}
}

// ExecCommand is the parsed shape of a Git SSH exec command.
type ExecCommand struct {
	Op     ExecOp
	Tenant string
	Repo   string
	// LFSOp is "upload" or "download" when Op == OpLFSAuthenticate;
	// empty for other ops.
	LFSOp string
}

// ParseExecCommand validates the command string a Git client passes to
// `ssh git@host "<command>"`. Accepted forms:
//
//	git-upload-pack 'tenant/repo.git'
//	git-upload-pack "tenant/repo.git"
//	git-upload-pack tenant/repo.git
//	git-upload-pack /tenant/repo.git
//
// And the same shapes for git-receive-pack. Anything else returns an error.
//
// The repo path is normalized through the same name-validation regex as
// the HTTP route parser, so SSH and HTTP accept exactly the same set of
// tenant/repo names.
func ParseExecCommand(s string) (*ExecCommand, error) {
	if s == "" {
		return nil, errors.New("empty command")
	}
	verb, rest, hasArg := strings.Cut(s, " ")
	if !hasArg || rest == "" {
		return nil, errors.New("command requires a path argument")
	}

	switch verb {
	case "git-upload-pack", "git-receive-pack":
		op := OpUpload
		if verb == "git-receive-pack" {
			op = OpReceive
		}
		arg, err := stripQuotes(rest)
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(arg, "/") {
			arg = arg[1:]
		}
		tenant, repo, err := splitRepoPath(arg)
		if err != nil {
			return nil, err
		}
		return &ExecCommand{Op: op, Tenant: tenant, Repo: repo}, nil

	case "git-lfs-authenticate":
		// Expect exactly two whitespace-separated args: <path> <op>.
		// We do NOT support quoted paths here — git-lfs never quotes
		// the path on this command, and accepting quoting would
		// complicate the two-arg split.
		fields := strings.Fields(rest)
		if len(fields) != 2 {
			return nil, errors.New("git-lfs-authenticate requires exactly <owner>/<repo> <upload|download>")
		}
		path, lfsOp := fields[0], fields[1]
		if lfsOp != "upload" && lfsOp != "download" {
			return nil, errors.New("git-lfs-authenticate op must be upload or download")
		}
		// git-lfs never quotes the path on this command; reject quoted
		// forms up front so the caller sees a clear message instead of
		// an opaque splitRepoPath error.
		if strings.ContainsAny(path, `'"`) {
			return nil, errors.New("git-lfs-authenticate path must not be quoted")
		}
		// Accept both with and without a trailing .git suffix.
		if strings.HasPrefix(path, "/") {
			path = path[1:]
		}
		if path == "" {
			return nil, errors.New("git-lfs-authenticate requires <owner>/<repo>")
		}
		if !strings.HasSuffix(path, ".git") {
			path += ".git"
		}
		tenant, repo, err := splitRepoPath(path)
		if err != nil {
			return nil, err
		}
		return &ExecCommand{Op: OpLFSAuthenticate, Tenant: tenant, Repo: repo, LFSOp: lfsOp}, nil

	default:
		return nil, errors.New("command not allowed")
	}
}

// stripQuotes removes a single matched pair of single or double quotes
// surrounding the argument. Rejects mixed (e.g. 'foo") or unbalanced
// quoting and trailing characters after the closing quote.
func stripQuotes(s string) (string, error) {
	if len(s) == 0 {
		return s, nil
	}
	first := s[0]
	if first != '\'' && first != '"' {
		// No quotes at all — accept as-is.
		// Reject if the unquoted form contains any quote characters,
		// since that means the client mis-quoted.
		if strings.ContainsAny(s, `'"`) {
			return "", errors.New("invalid path: unbalanced quote")
		}
		return s, nil
	}
	last := s[len(s)-1]
	if last != first {
		return "", errors.New("invalid path: unbalanced or mixed quotes")
	}
	if len(s) < 2 {
		return "", errors.New("invalid path: empty quoted argument")
	}
	inner := s[1 : len(s)-1]
	if strings.ContainsAny(inner, `'"`) {
		return "", errors.New("invalid path: nested quotes")
	}
	return inner, nil
}

// splitRepoPath validates "tenant/repo.git" → (tenant, repo). Rejects
// paths with traversal sequences, control characters, percent-encoding,
// or anything other than exactly one slash separator. Requires a literal
// ".git" suffix.
func splitRepoPath(arg string) (tenant, repo string, err error) {
	// Reject control chars, NUL, percent-encoding, and backslashes.
	for i := 0; i < len(arg); i++ {
		c := arg[i]
		if c < 0x20 || c == 0x7f || c == '%' || c == '\\' {
			return "", "", errors.New("invalid path")
		}
	}
	// Must contain exactly one slash, between tenant and repo.git.
	parts := strings.Split(arg, "/")
	if len(parts) != 2 {
		return "", "", errors.New("invalid path: expected tenant/repo.git")
	}
	tenant = parts[0]
	repoSeg := parts[1]
	if !strings.HasSuffix(repoSeg, ".git") || repoSeg == ".git" {
		return "", "", errors.New("missing .git suffix")
	}
	repo = strings.TrimSuffix(repoSeg, ".git")
	if tenant == "" || repo == "" {
		return "", "", errors.New("invalid path: empty tenant or repo")
	}
	if tenant == "." || tenant == ".." || repo == "." || repo == ".." {
		return "", "", errors.New("invalid path")
	}
	if !routenames.ValidateName(tenant) || !routenames.ValidateName(repo) {
		return "", "", errors.New("invalid path: name not allowed")
	}
	return tenant, repo, nil
}
