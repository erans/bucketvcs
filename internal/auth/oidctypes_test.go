package auth

import "testing"

func TestMatchRule(t *testing.T) {
	rules := []OIDCTrustRule{
		{ID: "r2", Audience: "aud", Tenant: "org", Repo: "app",
			Claims: map[string]string{"repository": "org/app", "ref": "refs/heads/main"}},
		{ID: "r1", Audience: "aud", Tenant: "org", Repo: "app",
			Claims: map[string]string{"repository": "org/app"}},
		{ID: "r3", Audience: "other", Tenant: "org", Repo: "app",
			Claims: map[string]string{"repository": "org/app"}},
	}

	t.Run("all claims equal matches; first by (tenant,repo,id)", func(t *testing.T) {
		claims := map[string]any{"aud": "aud", "repository": "org/app", "ref": "refs/heads/main"}
		got := MatchRule(rules, claims)
		if got == nil || got.ID != "r1" {
			t.Fatalf("want r1 (lowest id among matches), got %+v", got)
		}
	})
	t.Run("missing required claim does not match", func(t *testing.T) {
		claims := map[string]any{"aud": "aud", "repository": "org/other"}
		if got := MatchRule(rules, claims); got != nil {
			t.Fatalf("want nil, got %+v", got)
		}
	})
	t.Run("audience must match", func(t *testing.T) {
		claims := map[string]any{"aud": "nope", "repository": "org/app"}
		if got := MatchRule(rules, claims); got != nil {
			t.Fatalf("want nil for wrong aud, got %+v", got)
		}
	})
	t.Run("zero-claim rule is wildcard", func(t *testing.T) {
		wc := []OIDCTrustRule{{ID: "w", Audience: "aud", Tenant: "org", Repo: "app", Claims: map[string]string{}}}
		claims := map[string]any{"aud": "aud", "anything": "x"}
		if got := MatchRule(wc, claims); got == nil {
			t.Fatal("zero-claim rule should match any token from issuer")
		}
	})
}
