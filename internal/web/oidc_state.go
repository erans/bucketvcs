package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// oidcState is the data carried (HMAC-authenticated) in the bvcs_oidc cookie
// between the authorize redirect and the callback.
type oidcState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"` // PKCE code_verifier
	Next     string `json:"x"`
	Exp      int64  `json:"e"` // unix seconds
}

var errBadOIDCState = errors.New("web: bad oidc state cookie")

// encodeOIDCState returns base64url(payload) + "." + base64url(HMAC-SHA256(payload)).
func encodeOIDCState(key []byte, st oidcState) string {
	payload, _ := json.Marshal(st)
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	sig := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// decodeOIDCState verifies the HMAC and expiry and returns the state.
func decodeOIDCState(key []byte, enc string) (oidcState, error) {
	dot := strings.IndexByte(enc, '.')
	if dot <= 0 {
		return oidcState{}, errBadOIDCState
	}
	payload, err := base64.RawURLEncoding.DecodeString(enc[:dot])
	if err != nil {
		return oidcState{}, errBadOIDCState
	}
	sig, err := base64.RawURLEncoding.DecodeString(enc[dot+1:])
	if err != nil {
		return oidcState{}, errBadOIDCState
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	if subtle.ConstantTimeCompare(sig, mac.Sum(nil)) != 1 {
		return oidcState{}, errBadOIDCState
	}
	var st oidcState
	if err := json.Unmarshal(payload, &st); err != nil {
		return oidcState{}, errBadOIDCState
	}
	if st.Exp == 0 || time.Now().Unix() > st.Exp {
		return oidcState{}, errBadOIDCState
	}
	return st, nil
}
