// internal/web/adminhelpers.go
package web

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/policy"
)

// flashableErr reports whether err is a user-actionable validation error whose
// text is safe to surface in a flash (vs an internal failure to mask as 500).
//
// Predicates are derived conservatively from the service code so an unknown
// (wrapped DB) error always falls through to the 500 path:
//
//   - policy.AddPathRule wraps policy.ErrInvalidInput for bad patterns; its DB
//     failure is fmt.Errorf("policy: add path rule: %w", err) — caught by NONE
//     of the predicates below, so it masks as 500.
//   - policy.Add returns fmt.Errorf("policy: refname_pattern must not be empty")
//     and fmt.Errorf("policy: invalid refname_pattern %q: %w", ...) for
//     validation; its DB failure is fmt.Errorf("policy add %q/...", ...) (no
//     colon after "policy", so the prefixes below do not match it).
//   - hooks.Store.Add returns validateRow errors prefixed "hooks: "; its DB
//     failure is fmt.Errorf("hooks.Add: %w", err) (prefix "hooks.Add: ", not
//     "hooks: ").
//
// policy.ErrConflict is matched for interface conformance (reserved sentinel).
func flashableErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, policy.ErrInvalidInput) || errors.Is(err, policy.ErrConflict) {
		return true
	}
	msg := err.Error()
	// policy.Add validation messages (NOT its DB wrap "policy add ...").
	if strings.HasPrefix(msg, "policy: refname_pattern ") ||
		strings.HasPrefix(msg, "policy: invalid refname_pattern ") {
		return true
	}
	// hooks.Store.Add validateRow messages (NOT its DB wrap "hooks.Add: ...").
	if strings.HasPrefix(msg, "hooks: ") {
		return true
	}
	return false
}

// postGuard enforces POST + parseable form + valid CSRF, writing the error
// response itself. Callers bail out when it returns false.
func (s *server) postGuard(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		s.renderError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return false
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "bad form")
		return false
	}
	if !checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "invalid CSRF token")
		return false
	}
	return true
}

// redirectFlash sets a one-shot notice and 303s to dest.
func (s *server) redirectFlash(w http.ResponseWriter, r *http.Request, dest, msg string) {
	setFlash(w, msg, requestIsTLS(r, s.trustProxy))
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// emitAdmin emits an audit event for a web-originated admin action.
// Callers append target attrs; actor/source are added here.
func (s *server) emitAdmin(ctx context.Context, event string, attrs ...slog.Attr) {
	sess := SessionFromContext(ctx)
	actor := ""
	if sess != nil {
		actor = sess.Name
	}
	all := append([]slog.Attr{slog.String("actor", actor), slog.String("source", "web")}, attrs...)
	logger := s.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, event, all...)
}

// renderBuffered renders a page to a buffer first so template errors yield a
// clean 500 instead of a half-written 200.
func (s *server) renderBuffered(w http.ResponseWriter, page string, data any) error {
	var buf bytes.Buffer
	if err := s.render.render(&buf, page, data); err != nil {
		logger := s.logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Error("web: render", "page", page, "err", err)
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
	return nil
}

type secretData struct {
	base
	Title  string
	Secret string // plaintext credential; rendered exactly once, never logged
	Back   string // link target after the user copies it
}

// renderSecretOnce renders the one-time credential page directly (no redirect,
// so the secret never transits a header/cookie). Refresh re-submits the form;
// the page says so.
func (s *server) renderSecretOnce(w http.ResponseWriter, r *http.Request, title, secret, back string) {
	// The page carries a plaintext credential; keep it out of browser and
	// proxy caches so it cannot be retrieved after the user navigates away.
	w.Header().Set("Cache-Control", "no-store, private")
	d := secretData{
		base: base{
			Session: SessionFromContext(r.Context()),
			// base.html renders the logout form with {{.CSRF}}; without a token
			// the hidden field is empty and logout from this page would 403.
			CSRF: issueCSRF(w, requestIsTLS(r, s.trustProxy)),
		},
		Title:  title,
		Secret: secret,
		Back:   back,
	}
	_ = s.renderBuffered(w, "secret.html", d)
}
