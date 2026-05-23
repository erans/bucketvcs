package auth

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// TokenScope is a bitmask of capabilities granted to a token. Spec §4
// defines 7 named scopes plus the ScopeLegacy zero value.
//
// Naming: this type is `TokenScope`, not `Scope`, because types.go already
// has a `Scope` struct for deploy-key narrowing (M6). The constants below
// keep the Scope* prefix per the spec.
type TokenScope uint64

const (
	ScopeRepoRead     TokenScope = 1 << 0
	ScopeRepoWrite    TokenScope = 1 << 1
	ScopeRepoAdmin    TokenScope = 1 << 2
	ScopeLFSRead      TokenScope = 1 << 3
	ScopeLFSWrite     TokenScope = 1 << 4
	ScopeWebhookAdmin TokenScope = 1 << 5
	ScopeStorageAdmin TokenScope = 1 << 6
)

const ScopeMaskAll TokenScope = ScopeRepoRead | ScopeRepoWrite | ScopeRepoAdmin |
	ScopeLFSRead | ScopeLFSWrite | ScopeWebhookAdmin | ScopeStorageAdmin

// ScopeLegacy is the zero-value sentinel. Pre-M17 tokens and SSH key
// subjects have Scopes = ScopeLegacy and bypass the scope check.
const ScopeLegacy TokenScope = 0

var scopeNames = []struct {
	s    TokenScope
	name string
}{
	{ScopeRepoRead, "repo:read"},
	{ScopeRepoWrite, "repo:write"},
	{ScopeRepoAdmin, "repo:admin"},
	{ScopeLFSRead, "lfs:read"},
	{ScopeLFSWrite, "lfs:write"},
	{ScopeWebhookAdmin, "webhook:admin"},
	{ScopeStorageAdmin, "storage:admin"},
}

var nameToScope = func() map[string]TokenScope {
	m := make(map[string]TokenScope, len(scopeNames))
	for _, p := range scopeNames {
		m[p.name] = p.s
	}
	return m
}()

// String returns the canonical wire name of a single-bit TokenScope, or
// "legacy" for the zero value, or "scopes(0xN)" for multi-bit values.
func (s TokenScope) String() string {
	for _, p := range scopeNames {
		if p.s == s {
			return p.name
		}
	}
	if s == ScopeLegacy {
		return "legacy"
	}
	return fmt.Sprintf("scopes(0x%x)", uint64(s))
}

// Has reports whether mask s includes at least one bit from required —
// "any of" semantics. Pass a single-bit required mask for unambiguous
// checks; this is how every caller in the tree uses it today.
//
// IMPORTANT: do NOT pass a composite mask like
// (ScopeRepoRead | ScopeLFSRead) expecting "all of" semantics — Has
// returns true if the actor has either bit, which is almost never what
// a composite-required call site wants. For "all of" requirements call
// Has (or CheckScope) once per capability and AND the results.
func (s TokenScope) Has(required TokenScope) bool {
	return s&required != 0
}

// EffectiveScopes applies the implicit hierarchy. Idempotent.
func EffectiveScopes(s TokenScope) TokenScope {
	if s&ScopeRepoAdmin != 0 {
		s |= ScopeRepoWrite | ScopeRepoRead
	}
	if s&ScopeRepoWrite != 0 {
		s |= ScopeRepoRead
	}
	if s&ScopeLFSWrite != 0 {
		s |= ScopeLFSRead
	}
	return s
}

// FormatScopes returns "legacy" for 0, "all" for ScopeMaskAll, csv otherwise.
func FormatScopes(s TokenScope) string {
	if s == ScopeLegacy {
		return "legacy"
	}
	if s&ScopeMaskAll == ScopeMaskAll {
		return "all"
	}
	var names []string
	for _, p := range scopeNames {
		if s&p.s != 0 {
			names = append(names, p.name)
		}
	}
	sort.SliceStable(names, func(i, j int) bool {
		return indexOfScopeName(names[i]) < indexOfScopeName(names[j])
	})
	return strings.Join(names, ",")
}

func indexOfScopeName(n string) int {
	for i, p := range scopeNames {
		if p.name == n {
			return i
		}
	}
	return -1
}

// ParseScopes accepts "all", "legacy", "repo:*", "lfs:*", or CSV.
func ParseScopes(s string) (TokenScope, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("%w: empty scope list", ErrInvalidScope)
	}
	if s == "all" {
		return ScopeMaskAll, nil
	}
	if s == "legacy" {
		return ScopeLegacy, nil
	}
	var out TokenScope
	for _, raw := range strings.Split(s, ",") {
		tok := strings.TrimSpace(raw)
		switch tok {
		case "repo:*":
			out |= ScopeRepoRead | ScopeRepoWrite | ScopeRepoAdmin
		case "lfs:*":
			out |= ScopeLFSRead | ScopeLFSWrite
		default:
			sc, ok := nameToScope[tok]
			if !ok {
				return 0, fmt.Errorf("%w: unknown scope %q", ErrInvalidScope, tok)
			}
			out |= sc
		}
	}
	if out == 0 {
		return 0, fmt.Errorf("%w: empty after parsing", ErrInvalidScope)
	}
	return out, nil
}

// ValidateScopes returns ErrInvalidScope if any bit outside ScopeMaskAll is set.
func ValidateScopes(s TokenScope) error {
	if s == ScopeLegacy {
		return nil
	}
	if s&^ScopeMaskAll != 0 {
		return fmt.Errorf("%w: invalid bits 0x%x", ErrInvalidScope, uint64(s&^ScopeMaskAll))
	}
	return nil
}

// CheckScope returns ErrInsufficientScope when the actor's scopes don't
// satisfy required. The required mask SHOULD be a single bit (one
// capability per call). Multi-bit required is treated as "any of" via
// Has's OR semantics — callers MUST NOT pass composite masks for "all
// of" requirements; issue one CheckScope call per capability instead.
//
// Returns nil for:
//   - required == 0
//   - actor.Scopes == ScopeLegacy (pre-M17 token; SSH key path)
//   - actor non-nil with EffectiveScopes covering required
//
// A nil actor (anonymous) fails the check unless required == 0.
func CheckScope(actor *Actor, required TokenScope) error {
	if required == 0 {
		return nil
	}
	if actor == nil {
		return ErrInsufficientScope
	}
	if actor.Scopes == ScopeLegacy {
		return nil
	}
	if EffectiveScopes(actor.Scopes).Has(required) {
		return nil
	}
	return ErrInsufficientScope
}

// ErrInvalidScope is returned by ParseScopes/ValidateScopes for malformed
// scope strings or out-of-range bits.
var ErrInvalidScope = errors.New("auth: invalid scope")
