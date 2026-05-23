package auth_test

import (
	"errors"
	"math/bits"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func TestTokenScope_String(t *testing.T) {
	cases := []struct {
		s    auth.TokenScope
		want string
	}{
		{auth.ScopeRepoRead, "repo:read"},
		{auth.ScopeRepoWrite, "repo:write"},
		{auth.ScopeRepoAdmin, "repo:admin"},
		{auth.ScopeLFSRead, "lfs:read"},
		{auth.ScopeLFSWrite, "lfs:write"},
		{auth.ScopeWebhookAdmin, "webhook:admin"},
		{auth.ScopeStorageAdmin, "storage:admin"},
	}
	for _, c := range cases {
		if got := c.s.String(); got != c.want {
			t.Errorf("TokenScope(%d).String() = %q, want %q", c.s, got, c.want)
		}
	}
}

func TestParseScopes(t *testing.T) {
	cases := []struct {
		in   string
		want auth.TokenScope
		ok   bool
	}{
		{"repo:read", auth.ScopeRepoRead, true},
		{"repo:read,lfs:read", auth.ScopeRepoRead | auth.ScopeLFSRead, true},
		{"all", auth.ScopeMaskAll, true},
		{"repo:*", auth.ScopeRepoRead | auth.ScopeRepoWrite | auth.ScopeRepoAdmin, true},
		{"lfs:*", auth.ScopeLFSRead | auth.ScopeLFSWrite, true},
		{"repo:read, lfs:read ", auth.ScopeRepoRead | auth.ScopeLFSRead, true},
		{"legacy", auth.ScopeLegacy, true},
		{"", 0, false},
		{"bogus", 0, false},
		{"repo:read,bogus", 0, false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := auth.ParseScopes(c.in)
			if c.ok && err != nil {
				t.Fatalf("ParseScopes(%q): %v", c.in, err)
			}
			if !c.ok && err == nil {
				t.Fatalf("ParseScopes(%q): nil err, want failure", c.in)
			}
			if got != c.want {
				t.Errorf("ParseScopes(%q) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

func TestFormatScopes(t *testing.T) {
	cases := []struct {
		s    auth.TokenScope
		want string
	}{
		{auth.ScopeLegacy, "legacy"},
		{auth.ScopeMaskAll, "all"},
		{auth.ScopeRepoRead, "repo:read"},
		{auth.ScopeRepoRead | auth.ScopeLFSRead, "repo:read,lfs:read"},
		{auth.ScopeRepoWrite | auth.ScopeWebhookAdmin, "repo:write,webhook:admin"},
	}
	for _, c := range cases {
		if got := auth.FormatScopes(c.s); got != c.want {
			t.Errorf("FormatScopes(%d) = %q, want %q", c.s, got, c.want)
		}
	}
}

func TestEffectiveScopes(t *testing.T) {
	cases := []struct {
		in   auth.TokenScope
		want auth.TokenScope
	}{
		{auth.ScopeRepoAdmin, auth.ScopeRepoRead | auth.ScopeRepoWrite | auth.ScopeRepoAdmin},
		{auth.ScopeRepoWrite, auth.ScopeRepoRead | auth.ScopeRepoWrite},
		{auth.ScopeRepoRead, auth.ScopeRepoRead},
		{auth.ScopeLFSWrite, auth.ScopeLFSRead | auth.ScopeLFSWrite},
		{auth.ScopeLFSRead, auth.ScopeLFSRead},
		{auth.ScopeWebhookAdmin, auth.ScopeWebhookAdmin},
		{auth.ScopeStorageAdmin, auth.ScopeStorageAdmin},
		{auth.ScopeMaskAll, auth.ScopeMaskAll},
		{auth.ScopeLegacy, auth.ScopeLegacy},
	}
	for _, c := range cases {
		got := auth.EffectiveScopes(c.in)
		if got != c.want {
			t.Errorf("EffectiveScopes(%d) = %d, want %d", c.in, got, c.want)
		}
		if doubled := auth.EffectiveScopes(got); doubled != got {
			t.Errorf("EffectiveScopes not idempotent for %d: %d vs %d", c.in, doubled, got)
		}
	}
}

func TestValidateScopes(t *testing.T) {
	if err := auth.ValidateScopes(auth.ScopeMaskAll); err != nil {
		t.Errorf("ValidateScopes(MaskAll): %v", err)
	}
	if err := auth.ValidateScopes(auth.ScopeLegacy); err != nil {
		t.Errorf("ValidateScopes(Legacy): %v", err)
	}
	bogus := auth.TokenScope(1 << 20)
	if err := auth.ValidateScopes(bogus); err == nil {
		t.Errorf("ValidateScopes(%d): nil err, want failure", bogus)
	}
}

func TestTokenScope_Has(t *testing.T) {
	mask := auth.ScopeRepoRead | auth.ScopeLFSRead
	if !mask.Has(auth.ScopeRepoRead) {
		t.Errorf("Has(ScopeRepoRead) = false, want true")
	}
	if mask.Has(auth.ScopeRepoWrite) {
		t.Errorf("Has(ScopeRepoWrite) = true, want false")
	}
}

func TestScopeMaskAll_CountsAllScopes(t *testing.T) {
	if got := bits.OnesCount64(uint64(auth.ScopeMaskAll)); got != 7 {
		t.Errorf("ScopeMaskAll has %d bits set, want 7", got)
	}
}

func TestCheckScope_LegacyBypass(t *testing.T) {
	actor := auth.Actor{Name: "alice", Scopes: auth.ScopeLegacy}
	if err := auth.CheckScope(&actor, auth.ScopeRepoWrite); err != nil {
		t.Errorf("CheckScope(legacy actor) = %v, want nil (bypass)", err)
	}
}

func TestCheckScope_Sufficient(t *testing.T) {
	actor := auth.Actor{Name: "alice", Scopes: auth.ScopeRepoWrite}
	if err := auth.CheckScope(&actor, auth.ScopeRepoRead); err != nil {
		t.Errorf("CheckScope(repo:write, want repo:read) = %v, want nil", err)
	}
}

func TestCheckScope_Insufficient(t *testing.T) {
	actor := auth.Actor{Name: "alice", Scopes: auth.ScopeRepoRead}
	err := auth.CheckScope(&actor, auth.ScopeRepoWrite)
	if !errors.Is(err, auth.ErrInsufficientScope) {
		t.Errorf("CheckScope(repo:read, want repo:write) = %v, want ErrInsufficientScope", err)
	}
}

func TestCheckScope_NilActor(t *testing.T) {
	if err := auth.CheckScope(nil, auth.ScopeRepoRead); !errors.Is(err, auth.ErrInsufficientScope) {
		t.Errorf("CheckScope(nil) = %v, want ErrInsufficientScope", err)
	}
}
