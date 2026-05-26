package auth

import "sort"

// OIDCIssuer is a registered trusted OIDC issuer.
type OIDCIssuer struct {
	Alias     string
	IssuerURL string
	CreatedAt int64
}

// OIDCTrustRule maps validated token claims to a repo-scoped grant. A token
// matches when its `aud` equals Audience and every entry in Claims is present
// and string-equal in the token. An empty Claims map matches any token from
// the issuer.
type OIDCTrustRule struct {
	ID          string
	IssuerAlias string
	Audience    string
	Tenant      string
	Repo        string
	Scopes      TokenScope
	TTLSeconds  int64
	Claims      map[string]string
	CreatedAt   int64
}

// MatchRule returns the first rule (ordered by Tenant, Repo, ID) whose
// audience and claim constraints all match the token claims, or nil. The
// caller is responsible for passing only rules belonging to the token's
// issuer.
func MatchRule(rules []OIDCTrustRule, claims map[string]any) *OIDCTrustRule {
	sorted := make([]OIDCTrustRule, len(rules))
	copy(sorted, rules)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Tenant != sorted[j].Tenant {
			return sorted[i].Tenant < sorted[j].Tenant
		}
		if sorted[i].Repo != sorted[j].Repo {
			return sorted[i].Repo < sorted[j].Repo
		}
		return sorted[i].ID < sorted[j].ID
	})
	aud, _ := claims["aud"].(string)
	for i := range sorted {
		r := &sorted[i]
		if r.Audience != aud {
			continue
		}
		if claimsSatisfy(r.Claims, claims) {
			out := *r
			return &out
		}
	}
	return nil
}

func claimsSatisfy(required map[string]string, claims map[string]any) bool {
	for name, want := range required {
		got, ok := claims[name].(string)
		if !ok || got != want {
			return false
		}
	}
	return true
}
