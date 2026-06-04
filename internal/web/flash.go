package web

import (
	"encoding/base64"
	"net/http"
)

const flashCookieName = "bvcs_flash"

// setFlash stores a short one-shot notice shown on the next page render.
// Base64url-encoded so arbitrary text survives the cookie value charset.
func setFlash(w http.ResponseWriter, msg string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookieName,
		Value:    base64.RawURLEncoding.EncodeToString([]byte(msg)),
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   60,
	})
}

// takeFlash returns the pending notice ("" if none) and clears the cookie.
func takeFlash(w http.ResponseWriter, r *http.Request) string {
	c, err := r.Cookie(flashCookieName)
	if err != nil || c.Value == "" {
		return ""
	}
	http.SetCookie(w, &http.Cookie{
		Name: flashCookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
	})
	b, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return ""
	}
	return string(b)
}
