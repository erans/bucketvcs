package web

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"time"

	"golang.org/x/oauth2"
)

const oidcCookieName = "bvcs_oidc"

func randToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("web: oidc: crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *server) handleOIDCAuthorize(w http.ResponseWriter, r *http.Request) {
	secure := requestIsTLS(r, s.trustProxy)
	state := randToken()
	nonce := randToken()
	verifier := oauth2.GenerateVerifier()

	st := oidcState{
		State:    state,
		Nonce:    nonce,
		Verifier: verifier,
		Next:     safeNext(r.URL.Query().Get("next")),
		Exp:      time.Now().Add(10 * time.Minute).Unix(),
	}
	http.SetCookie(w, &http.Cookie{
		Name:     oidcCookieName,
		Value:    encodeOIDCState(s.oidc.HMACKey, st),
		Path:     "/login/oidc",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   600,
	})

	authURL := s.oauthCfg.AuthCodeURL(state,
		oauth2.SetAuthURLParam("nonce", nonce),
		oauth2.S256ChallengeOption(verifier),
	)
	http.Redirect(w, r, authURL, http.StatusFound)
}
