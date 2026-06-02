package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
)

const (
	csrfCookieName = "bvcs_csrf"
	csrfFormField  = "csrf_token"
)

// issueCSRF generates a token, sets it as a cookie, and returns it for embedding
// in a hidden form field (double-submit pattern). secure marks the cookie Secure.
func issueCSRF(w http.ResponseWriter, secure bool) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// A broken OS CSPRNG must fail loudly, not emit a predictable token.
		panic("web: csrf: crypto/rand failed: " + err.Error())
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
	return tok
}

// checkCSRF returns true iff the form field matches the cookie (constant-time).
func checkCSRF(r *http.Request) bool {
	c, err := r.Cookie(csrfCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	form := r.PostFormValue(csrfFormField)
	if form == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(form)) == 1
}
