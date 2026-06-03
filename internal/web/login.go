package web

import (
	"net/http"
	"strconv"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/gateway"
)

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	secure := requestIsTLS(r, s.trustProxy)
	switch r.Method {
	case http.MethodGet:
		tok := issueCSRF(w, secure)
		ld := loginData{
			base: base{Session: SessionFromContext(r.Context()), CSRF: tok},
			Next: safeNext(r.URL.Query().Get("next")),
		}
		if s.oidc != nil {
			ld.OIDC = true
			ld.OIDCLabel = s.oidc.Label
		}
		_ = s.render.render(w, "login.html", ld)
		EmitRequestMetric(r.Context(), s.logger, "login", 200)

	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			s.renderError(w, r, http.StatusBadRequest, "bad form")
			return
		}
		if !checkCSRF(r) {
			s.renderError(w, r, http.StatusForbidden, "invalid CSRF token")
			return
		}
		ip := gateway.ClientIP(r, s.trustProxy)
		username := r.PostFormValue("username")
		if s.limiter != nil {
			if ok, retry, _ := s.limiter.CheckDetailed(ip, username); !ok {
				sec := int(retry.Seconds())
				if sec < 1 {
					sec = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(sec))
				EmitLoginMetric(r.Context(), s.logger, "ratelimited", "password")
				s.renderError(w, r, http.StatusTooManyRequests, "too many attempts; try again later")
				return
			}
		}

		actor, err := s.store.VerifyPassword(r.Context(), username, r.PostFormValue("password"))
		if err != nil {
			if auth.IsCredentialError(err) {
				s.limiter.MarkFailure(ip, username) // nil-safe
			}
			EmitLoginMetric(r.Context(), s.logger, "invalid", "password")
			tok := issueCSRF(w, secure)
			w.WriteHeader(http.StatusUnauthorized)
			ld401 := loginData{
				base:  base{CSRF: tok},
				Error: "invalid username or password",
				Next:  safeNext(r.PostFormValue("next")),
			}
			if s.oidc != nil {
				ld401.OIDC = true
				ld401.OIDCLabel = s.oidc.Label
			}
			_ = s.render.render(w, "login.html", ld401)
			return
		}
		s.limiter.MarkSuccess(ip, username) // nil-safe

		raw, err := s.store.CreateSession(r.Context(), actor.UserID, "password", s.ttl)
		if err != nil {
			s.renderError(w, r, http.StatusInternalServerError, "could not create session")
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookieName,
			Value:    raw,
			Path:     "/",
			HttpOnly: true,
			Secure:   secure,
			SameSite: http.SameSiteLaxMode,
		})
		EmitLoginMetric(r.Context(), s.logger, "success", "password")
		EmitSessionCreated(r.Context(), s.logger, actor.UserID, actor.Name, "password")
		http.Redirect(w, r, safeNext(r.PostFormValue("next")), http.StatusSeeOther)

	default:
		s.renderError(w, r, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.renderError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "bad form")
		return
	}
	if !checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "invalid CSRF token")
		return
	}
	if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
		if sess := SessionFromContext(r.Context()); sess != nil {
			EmitSessionDestroyed(r.Context(), s.logger, sess.UserID, sess.Name)
		}
		_ = s.store.DeleteSession(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
