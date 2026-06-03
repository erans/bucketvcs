package web

import (
	"context"

	"github.com/bucketvcs/bucketvcs/internal/oidc"
	"golang.org/x/oauth2"
)

// idTokenVerifier is the subset of *oidc.Verifier the callback needs (so tests
// can inject a fake that returns canned claims without hitting a real IdP).
type idTokenVerifier interface {
	Verify(ctx context.Context, raw, issuer string) (oidc.Claims, error)
}

// OIDCProvider is the resolved browser-login configuration (built in serve.go).
type OIDCProvider struct {
	Issuer      string
	ClientID    string
	AuthURL     string
	TokenURL    string
	RedirectURL string
	Scopes      []string
	Label       string
	HMACKey     []byte          // temp-cookie integrity (per process)
	Verifier    idTokenVerifier // nil => built from oidc.NewVerifier() in NewHandler
	Secret      string          // optional client secret
}

// oauthConfig builds the x/oauth2 config for this provider.
func (p *OIDCProvider) oauthConfig() *oauth2.Config {
	return &oauth2.Config{
		ClientID:     p.ClientID,
		ClientSecret: p.Secret,
		Endpoint:     oauth2.Endpoint{AuthURL: p.AuthURL, TokenURL: p.TokenURL},
		RedirectURL:  p.RedirectURL,
		Scopes:       p.Scopes,
	}
}

// claimBool reads a bool claim (e.g. email_verified).
func claimBool(c oidc.Claims, name string) bool {
	b, _ := c[name].(bool)
	return b
}

// audienceContains reports whether the token's aud (string or array) includes clientID.
func audienceContains(c oidc.Claims, clientID string) bool {
	switch v := c["aud"].(type) {
	case string:
		return v == clientID
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok && s == clientID {
				return true
			}
		}
	}
	return false
}
