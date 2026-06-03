package web

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	gw "github.com/bucketvcs/bucketvcs/internal/gateway"
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
	if r.Method != http.MethodGet {
		s.renderError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
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

func (s *server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	ip := gw.ClientIP(r, s.trustProxy)
	reject := func(code int, reason, email string) {
		s.limiter.MarkFailure(ip, "") // nil-safe
		EmitOIDCRejected(r.Context(), s.logger, s.oidc.Issuer, reason, email)
		EmitLoginMetric(r.Context(), s.logger, "invalid", "oidc")
		s.renderError(w, r, code, "single sign-on failed")
	}

	// 1. temp cookie present?
	c, err := r.Cookie(oidcCookieName)
	if err != nil || c.Value == "" {
		reject(http.StatusBadRequest, "state_mismatch", "")
		return
	}
	st, decErr := decodeOIDCState(s.oidc.HMACKey, c.Value)
	// always clear the temp cookie
	http.SetCookie(w, &http.Cookie{Name: oidcCookieName, Value: "", Path: "/login/oidc", MaxAge: -1, HttpOnly: true})
	if decErr != nil {
		reject(http.StatusBadRequest, "state_mismatch", "")
		return
	}

	// 2. IdP-returned error
	if e := r.URL.Query().Get("error"); e != "" {
		reject(http.StatusBadRequest, "idp_error", "")
		return
	}

	// 3. state double-submit
	if r.URL.Query().Get("state") != st.State {
		reject(http.StatusBadRequest, "state_mismatch", "")
		return
	}

	// 4. code -> token
	tok, err := s.oauthCfg.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(st.Verifier))
	if err != nil {
		reject(http.StatusUnauthorized, "token_invalid", "")
		return
	}
	rawID, _ := tok.Extra("id_token").(string)
	if rawID == "" {
		reject(http.StatusUnauthorized, "token_invalid", "")
		return
	}

	// 5. verify signature + iss/exp
	claims, err := s.verifier.Verify(r.Context(), rawID, s.oidc.Issuer)
	if err != nil {
		reject(http.StatusUnauthorized, "token_invalid", "")
		return
	}

	// 6. RP checks: aud + nonce
	if !audienceContains(claims, s.oidc.ClientID) {
		reject(http.StatusUnauthorized, "token_invalid", "")
		return
	}
	if claims.String("nonce") != st.Nonce {
		reject(http.StatusUnauthorized, "token_invalid", "")
		return
	}

	// 7. verified email
	email := claims.String("email")
	if email == "" || !claimBool(claims, "email_verified") {
		reject(http.StatusUnauthorized, "email_unverified", email)
		return
	}
	subject := claims.String("sub")

	// 8. resolve user
	actor, err := s.store.FindIdentity(r.Context(), s.oidc.Issuer, subject)
	if errors.Is(err, auth.ErrNoSuchUser) {
		// TOFU: match by verified email, then pin (issuer, subject)
		actor, err = s.store.FindUserByEmail(r.Context(), email)
		if errors.Is(err, auth.ErrNoSuchUser) {
			reject(http.StatusUnauthorized, "no_user", email)
			return
		}
		if errors.Is(err, auth.ErrUserDisabled) {
			reject(http.StatusUnauthorized, "disabled", email)
			return
		}
		if err != nil {
			reject(http.StatusInternalServerError, "server_error", email)
			return
		}
		if lerr := s.store.LinkIdentity(r.Context(), actor.UserID, s.oidc.Issuer, subject, email); lerr != nil {
			if errors.Is(lerr, auth.ErrConflict) {
				// race: another request linked it; re-resolve by identity
				actor, err = s.store.FindIdentity(r.Context(), s.oidc.Issuer, subject)
				if err != nil {
					reject(http.StatusUnauthorized, "no_user", email)
					return
				}
			} else {
				reject(http.StatusInternalServerError, "server_error", email)
				return
			}
		} else {
			EmitOIDCIdentityLinked(r.Context(), s.logger, actor.UserID, actor.Name, s.oidc.Issuer, subject, email)
		}
	} else if errors.Is(err, auth.ErrUserDisabled) {
		reject(http.StatusUnauthorized, "disabled", email)
		return
	} else if err != nil {
		reject(http.StatusInternalServerError, "server_error", email)
		return
	}

	// 9. session
	s.limiter.MarkSuccess(ip, "") // nil-safe
	raw, err := s.store.CreateSession(r.Context(), actor.UserID, "oidc", s.ttl)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "could not create session")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: raw, Path: "/",
		HttpOnly: true, Secure: requestIsTLS(r, s.trustProxy), SameSite: http.SameSiteLaxMode,
	})
	EmitLoginMetric(r.Context(), s.logger, "success", "oidc")
	EmitOIDCLogin(r.Context(), s.logger, actor.UserID, actor.Name, s.oidc.Issuer, subject)
	EmitSessionCreated(r.Context(), s.logger, actor.UserID, actor.Name, "oidc")
	http.Redirect(w, r, safeNext(st.Next), http.StatusSeeOther)
}
