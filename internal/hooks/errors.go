package hooks

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Sentinel errors returned by the runner.
var (
	// ErrScriptNotFound — hooks-root/<script_name> doesn't exist or isn't readable.
	ErrScriptNotFound = errors.New("hooks: script not found")
	// ErrTimeout — subprocess exceeded --hooks-timeout-sec wall-clock.
	ErrTimeout = errors.New("hooks: timeout")
	// ErrSandboxMissing — bwrap binary required but absent.
	ErrSandboxMissing = errors.New("hooks: bwrap not found")
	// ErrInternal — generic non-rejection error (fork failed, sandbox argv invalid, etc.).
	ErrInternal = errors.New("hooks: internal error")
)

// HookRejection is returned by Runner.Run when a hook script exits non-zero
// (pre-receive only — post-receive doesn't propagate exits).
type HookRejection struct {
	ScriptName string
	ExitCode   int
	Stderr     []byte // already truncated to --hooks-output-max-kb
}

func (h *HookRejection) Error() string {
	return fmt.Sprintf("hook %q rejected push (exit %d)", h.ScriptName, h.ExitCode)
}

// statusLineReasonMaxLen caps the reason string for the per-ref `ng <ref>
// <reason>` report-status pkt-line. Git's report-status grammar requires a
// single-line reason, and pkt-line payloads are bounded by pktline.MaxPayload
// (65516 bytes). 256 bytes leaves ample room for the surrounding
// "ng <refname> " prefix even with long ref names, and matches the brevity
// of existing receivepack rejection reasons ("ref already exists", "stale
// info").
const statusLineReasonMaxLen = 256

// StatusLineReason returns a short, single-line summary suitable for the
// `ng <ref> <reason>` report-status pkt-line. Newlines are replaced with
// spaces; the result is capped at 256 bytes. The full multi-line stderr
// remains available via the policy.hook.rejected audit event in serve.log.
func (h *HookRejection) StatusLineReason() string {
	base := fmt.Sprintf("hook %q rejected (exit %d)", h.ScriptName, h.ExitCode)
	if len(h.Stderr) == 0 {
		return base
	}
	// First non-empty line of stderr, sanitized to a single line.
	first := ""
	for _, line := range strings.Split(string(h.Stderr), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			first = trimmed
			break
		}
	}
	if first == "" {
		return base
	}
	out := base + ": " + first
	// Replace any remaining CRs and embedded NULs (shouldn't be present but
	// defense-in-depth against weird scripts) and cap length.
	out = strings.NewReplacer("\r", " ", "\x00", " ").Replace(out)
	if len(out) > statusLineReasonMaxLen {
		// Truncate on a rune boundary so we don't emit invalid UTF-8.
		// Walk back from the cap to the start of the last complete rune.
		cut := statusLineReasonMaxLen - 3
		for cut > 0 && !utf8.RuneStart(out[cut]) {
			cut--
		}
		out = out[:cut] + "..."
	}
	return out
}

// ClientMessage returns the multi-line, untruncated rejection text suitable
// for sideband band-2 progress output (where chunking is handled) or for
// inclusion in audit/log records. NOT safe for the report-status pkt-line
// (use StatusLineReason for that).
func (h *HookRejection) ClientMessage() string {
	if len(h.Stderr) == 0 {
		return fmt.Sprintf("hook %q rejected push (exit %d) — no stderr", h.ScriptName, h.ExitCode)
	}
	return fmt.Sprintf("hook %q (exit %d):\n%s", h.ScriptName, h.ExitCode, string(h.Stderr))
}
