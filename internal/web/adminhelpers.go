// internal/web/adminhelpers.go
package web

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
)

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
	d := secretData{
		base:   base{Session: SessionFromContext(r.Context())},
		Title:  title,
		Secret: secret,
		Back:   back,
	}
	_ = s.renderBuffered(w, "secret.html", d)
}
