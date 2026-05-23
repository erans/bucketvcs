# M17: Auth — Token Scopes + Rotation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add fine-grained bitmask scopes to HTTPS Basic tokens (`repo:read|write|admin`, `lfs:read|write`, `webhook:admin`, `storage:admin`) with implicit hierarchy. Add atomic `bucketvcs token rotate`. Backward-compatible via legacy `scopes=0` bypass for pre-M17 tokens.

**Architecture:** New `internal/auth/scopes.go` defines a `TokenScope` bitmask (named to avoid collision with existing `auth.Scope` deploy-key struct from M6). Migration 0008 adds `tokens.scopes INTEGER DEFAULT 0`. The existing `auth.Actor` struct gains a `Scopes TokenScope` field; HTTPS Basic auth in `internal/gateway/auth.go` populates it from the resolved token. New `CheckScope(actor, required) error` helper is called at upload-pack, receive-pack, LFS download, LFS verify, and LFS locks enforcement sites. SSH key auth leaves `Actor.Scopes` at zero (= `ScopeLegacy`), which short-circuits the check.

**Tech Stack:** Go (stdlib `math/bits` for the defense-in-depth bit-count test), modernc.org/sqlite (the M4 authdb driver), slog for the audit emitters. No new dependencies.

**Reference:** `docs/superpowers/specs/2026-05-23-m17-auth-scopes-design.md`. Section numbers like "spec §4" refer to that file.

**Naming note vs spec:** spec §4 defined the type as `Scope`; plan uses `TokenScope` because `internal/auth/types.go:60` already has a `Scope` struct (deploy-key narrowing). Constants (`ScopeRepoRead`, etc.) keep the `Scope*` prefix per the spec's scope-naming convention.

---

## File layout

**New:**
- `internal/auth/scopes.go` — `TokenScope` bitmask + constants + `ParseScopes`/`FormatScopes`/`EffectiveScopes`/`ValidateScopes`/`Has`/`CheckScope` + `ErrInsufficientScope`
- `internal/auth/scopes_test.go`
- `internal/auth/sqlitestore/migrations/0008_token_scopes.sql`
- `scripts/m17-auth-scopes-smoke.sh`

**Modified:**
- `internal/auth/types.go` — `Actor.Scopes TokenScope` field
- `internal/auth/errors.go` — declare `ErrInsufficientScope` (cross-package use)
- `internal/auth/sqlitestore/store.go` — `Token.Scopes`; `CreateToken` signature gains scopes; `RotateToken` new method; `Get/List` plumb scopes
- `internal/gateway/auth.go` — populate `Actor.Scopes` after token resolution
- `internal/gateway/receive_pack.go` — call `CheckScope(actor, ScopeRepoWrite)` before existing perm check
- `internal/gateway/upload_pack.go` (or wherever upload-pack route is wired) — `CheckScope(actor, ScopeRepoRead)`
- `internal/lfs/handler.go` — `CheckScope(actor, ScopeLFSRead)` on download
- `internal/lfs/proxied.go` — `CheckScope(actor, ScopeLFSWrite)` on verify
- `internal/lfs/locks_handler.go` — `CheckScope(actor, ScopeLFSWrite)` on create + release
- `cmd/bucketvcs/token.go` — `--scopes` flag on create; new `rotate` subcommand; list shows scopes column
- Audit emission lives where M4's existing token audits live (locate during Task 6)

**No new packages.** All extensions to existing `internal/auth` package.

---

## Tasks

### Task 1: TokenScope taxonomy

**Files:**
- Create: `internal/auth/scopes.go`
- Create: `internal/auth/scopes_test.go`
- Modify: `internal/auth/errors.go` (add ErrInsufficientScope)

- [ ] **Step 1: Write the failing test**

Create `internal/auth/scopes_test.go`:

```go
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
		// repo hierarchy.
		{auth.ScopeRepoAdmin, auth.ScopeRepoRead | auth.ScopeRepoWrite | auth.ScopeRepoAdmin},
		{auth.ScopeRepoWrite, auth.ScopeRepoRead | auth.ScopeRepoWrite},
		{auth.ScopeRepoRead, auth.ScopeRepoRead},
		// lfs hierarchy.
		{auth.ScopeLFSWrite, auth.ScopeLFSRead | auth.ScopeLFSWrite},
		{auth.ScopeLFSRead, auth.ScopeLFSRead},
		// admin scopes stand alone.
		{auth.ScopeWebhookAdmin, auth.ScopeWebhookAdmin},
		{auth.ScopeStorageAdmin, auth.ScopeStorageAdmin},
		// idempotent.
		{auth.ScopeMaskAll, auth.ScopeMaskAll},
		{auth.ScopeLegacy, auth.ScopeLegacy},
	}
	for _, c := range cases {
		if got := auth.EffectiveScopes(c.in); got != c.want {
			t.Errorf("EffectiveScopes(%d) = %d, want %d", c.in, got, c.want)
		}
		// Idempotent.
		if doubled := auth.EffectiveScopes(auth.EffectiveScopes(c.in)); doubled != auth.EffectiveScopes(c.in) {
			t.Errorf("EffectiveScopes not idempotent for %d: %d vs %d",
				c.in, doubled, auth.EffectiveScopes(c.in))
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
	// Bit outside the known set.
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
	// Defense-in-depth: if a future bit is added without updating
	// ScopeMaskAll, this test fails. 7 scopes per spec §4.
	if got := bits.OnesCount64(uint64(auth.ScopeMaskAll)); got != 7 {
		t.Errorf("ScopeMaskAll has %d bits set, want 7", got)
	}
}

func TestCheckScope_LegacyBypass(t *testing.T) {
	// Actor with ScopeLegacy (zero value) bypasses the scope check.
	actor := auth.Actor{Name: "alice", Scopes: auth.ScopeLegacy}
	if err := auth.CheckScope(&actor, auth.ScopeRepoWrite); err != nil {
		t.Errorf("CheckScope(legacy actor) = %v, want nil (bypass)", err)
	}
}

func TestCheckScope_Sufficient(t *testing.T) {
	actor := auth.Actor{Name: "alice", Scopes: auth.ScopeRepoWrite}
	// repo:write implies repo:read via EffectiveScopes.
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
	// Nil actor (anonymous) — not a legacy bypass, scope check fails.
	if err := auth.CheckScope(nil, auth.ScopeRepoRead); !errors.Is(err, auth.ErrInsufficientScope) {
		t.Errorf("CheckScope(nil) = %v, want ErrInsufficientScope", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/auth/... -run "TestTokenScope|TestParseScopes|TestFormatScopes|TestEffectiveScopes|TestValidateScopes|TestCheckScope|TestScopeMaskAll" -count=1
```
Expected: FAIL with "undefined: auth.TokenScope" / "undefined: auth.ParseScopes" etc.

- [ ] **Step 3: Implement scopes.go**

Create `internal/auth/scopes.go`:

```go
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
// Naming note: this type is named TokenScope (not "Scope") because
// internal/auth/types.go already has a Scope struct for deploy-key
// narrowing (M6). The constants below keep the Scope* prefix per the
// spec's scope-naming convention.
type TokenScope uint64

const (
	ScopeRepoRead     TokenScope = 1 << 0 // "repo:read"
	ScopeRepoWrite    TokenScope = 1 << 1 // "repo:write"
	ScopeRepoAdmin    TokenScope = 1 << 2 // "repo:admin"
	ScopeLFSRead      TokenScope = 1 << 3 // "lfs:read"
	ScopeLFSWrite     TokenScope = 1 << 4 // "lfs:write"
	ScopeWebhookAdmin TokenScope = 1 << 5 // "webhook:admin"
	ScopeStorageAdmin TokenScope = 1 << 6 // "storage:admin"
)

// ScopeMaskAll is the union of all known scope bits.
const ScopeMaskAll TokenScope = ScopeRepoRead | ScopeRepoWrite | ScopeRepoAdmin |
	ScopeLFSRead | ScopeLFSWrite | ScopeWebhookAdmin | ScopeStorageAdmin

// ScopeLegacy is the zero-value sentinel. Pre-M17 tokens and SSH key
// subjects have Scopes = ScopeLegacy and bypass the scope check.
const ScopeLegacy TokenScope = 0

// scopeNames is the authoritative list of (scope, canonical name) pairs.
// Ordering controls FormatScopes output.
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

// String returns the canonical wire name of a single-bit TokenScope.
// Returns "scopes(0xN)" for zero/multi-bit values.
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

// Has reports whether mask s includes at least one bit from required.
// Used at enforcement sites: actor.Scopes.Has(ScopeRepoWrite).
func (s TokenScope) Has(required TokenScope) bool {
	return s&required != 0
}

// EffectiveScopes applies the implicit hierarchy from spec §4:
//   ScopeRepoAdmin → adds ScopeRepoWrite + ScopeRepoRead
//   ScopeRepoWrite → adds ScopeRepoRead
//   ScopeLFSWrite  → adds ScopeLFSRead
// Idempotent. Returns the input unioned with everything it implies.
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

// FormatScopes returns:
//   "legacy" for ScopeLegacy (0)
//   "all"    when mask == ScopeMaskAll
//   csv      otherwise, in canonical order
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

// ParseScopes accepts:
//   "all"     → ScopeMaskAll
//   "legacy"  → ScopeLegacy
//   "repo:*"  → ScopeRepoRead | ScopeRepoWrite | ScopeRepoAdmin
//   "lfs:*"   → ScopeLFSRead | ScopeLFSWrite
//   comma-separated canonical names, whitespace tolerant
// Empty string and unknown names return ErrInvalidScope.
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

// ValidateScopes returns ErrInvalidScope if any bit outside ScopeMaskAll
// is set. Called at CreateToken time.
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
// satisfy required. Returns nil for:
//   - actor.Scopes == ScopeLegacy (pre-M17 token; SSH key path)
//   - actor non-nil with effective scopes covering required
//
// A nil actor is anonymous and fails the check unless required == 0.
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
// scope strings or bit positions outside ScopeMaskAll.
var ErrInvalidScope = errors.New("auth: invalid scope")
```

- [ ] **Step 4: Add ErrInsufficientScope sentinel**

Edit `internal/auth/errors.go`. Find the existing error declarations (e.g. `ErrTokenExpired`):

```bash
grep -n "ErrTokenExpired\|var Err" internal/auth/errors.go
```

Add:

```go
// ErrInsufficientScope is returned by CheckScope when the actor's token
// scopes don't satisfy the required scope. M17.
var ErrInsufficientScope = errors.New("auth: insufficient scope")
```

- [ ] **Step 5: Add Scopes field to Actor**

Edit `internal/auth/types.go`. Find the existing Actor struct:

```bash
grep -nA5 "^type Actor" internal/auth/types.go
```

Add `Scopes TokenScope` field:

```go
type Actor struct {
    UserID  string
    Name    string
    IsAdmin bool
    Scopes  TokenScope // M17: populated by HTTPS Basic path; zero on SSH (= ScopeLegacy)
}
```

- [ ] **Step 6: Run tests to verify pass**

```bash
go test ./internal/auth/... -count=1
```
Expected: PASS for new tests plus existing M4 tests still green.

- [ ] **Step 7: Build + vet**

```bash
go vet ./... && go build ./...
```
Expected: clean.

- [ ] **Step 8: Commit**

```bash
git add internal/auth/scopes.go internal/auth/scopes_test.go \
        internal/auth/errors.go internal/auth/types.go
git commit -m "auth: TokenScope bitmask + CheckScope + ErrInsufficientScope (M17 Task 1)"
```

---

### Task 2: Migration + Store CRUD plumbing

**Files:**
- Create: `internal/auth/sqlitestore/migrations/0008_token_scopes.sql`
- Modify: `internal/auth/sqlitestore/store.go` — Token.Scopes, CreateToken sig, Get/List, new RotateToken

- [ ] **Step 1: Write the migration**

Create `internal/auth/sqlitestore/migrations/0008_token_scopes.sql`:

```sql
ALTER TABLE tokens ADD COLUMN scopes INTEGER NOT NULL DEFAULT 0;

INSERT INTO schema_version (version, applied_at) VALUES (8, strftime('%s','now'));
```

- [ ] **Step 2: Write the failing test**

Append to `internal/auth/sqlitestore/store_test.go` (or wherever existing token tests live — confirm via `grep -n "TestCreateToken\|func TestToken" internal/auth/sqlitestore/*_test.go`):

```go
func TestCreateTokenWithScopes(t *testing.T) {
	store := openTestStore(t)
	defer store.Close()
	ctx := context.Background()

	if err := store.CreateUser(ctx, "u1", "alice", false); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	scopes := auth.ScopeRepoRead | auth.ScopeLFSRead
	if err := store.CreateToken(ctx, "tok1", "u1", "hash1", "label", nil, scopes); err != nil {
		t.Fatalf("CreateToken with scopes: %v", err)
	}
	got, err := store.GetTokenByID(ctx, "tok1")
	if err != nil {
		t.Fatalf("GetTokenByID: %v", err)
	}
	if got.Scopes != scopes {
		t.Errorf("Token.Scopes = %d, want %d", got.Scopes, scopes)
	}
}

func TestCreateTokenLegacyScopes(t *testing.T) {
	store := openTestStore(t)
	defer store.Close()
	ctx := context.Background()
	if err := store.CreateUser(ctx, "u1", "alice", false); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	// Scopes=0 means legacy/all-permissions.
	if err := store.CreateToken(ctx, "tok1", "u1", "hash1", "label", nil, auth.ScopeLegacy); err != nil {
		t.Fatalf("CreateToken with legacy scopes: %v", err)
	}
	got, _ := store.GetTokenByID(ctx, "tok1")
	if got.Scopes != auth.ScopeLegacy {
		t.Errorf("Token.Scopes = %d, want ScopeLegacy (0)", got.Scopes)
	}
}

func TestRotateToken(t *testing.T) {
	store := openTestStore(t)
	defer store.Close()
	ctx := context.Background()
	if err := store.CreateUser(ctx, "u1", "alice", false); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	exp := time.Now().Add(24 * time.Hour).Unix()
	if err := store.CreateToken(ctx, "tok1", "u1", "origHash", "label",
		&exp, auth.ScopeRepoWrite); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	if err := store.RotateToken(ctx, "tok1", "newHash"); err != nil {
		t.Fatalf("RotateToken: %v", err)
	}
	got, _ := store.GetTokenByID(ctx, "tok1")
	if got.SecretHash != "newHash" {
		t.Errorf("after rotate: SecretHash = %q, want newHash", got.SecretHash)
	}
	// Scopes + ExpiresAt preserved.
	if got.Scopes != auth.ScopeRepoWrite {
		t.Errorf("after rotate: Scopes = %d, want ScopeRepoWrite (preserved)", got.Scopes)
	}
	if got.ExpiresAt == nil || *got.ExpiresAt != exp {
		t.Errorf("after rotate: ExpiresAt changed; got %v, want %d", got.ExpiresAt, exp)
	}
}

func TestRotateTokenNotFound(t *testing.T) {
	store := openTestStore(t)
	defer store.Close()
	err := store.RotateToken(context.Background(), "nonexistent", "newHash")
	if !errors.Is(err, auth.ErrNoSuchToken) {
		t.Errorf("RotateToken nonexistent: err=%v, want ErrNoSuchToken", err)
	}
}

func TestListTokensIncludesScopes(t *testing.T) {
	store := openTestStore(t)
	defer store.Close()
	ctx := context.Background()
	if err := store.CreateUser(ctx, "u1", "alice", false); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := store.CreateToken(ctx, "tok1", "u1", "h1", "a", nil, auth.ScopeRepoRead); err != nil {
		t.Fatalf("CreateToken 1: %v", err)
	}
	if err := store.CreateToken(ctx, "tok2", "u1", "h2", "b", nil, auth.ScopeRepoAdmin); err != nil {
		t.Fatalf("CreateToken 2: %v", err)
	}
	list, err := store.ListTokensForUser(ctx, "alice")
	if err != nil {
		t.Fatalf("ListTokensForUser: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListTokensForUser len=%d, want 2", len(list))
	}
	byID := map[string]auth.TokenScope{}
	for _, tk := range list {
		byID[tk.ID] = tk.Scopes
	}
	if byID["tok1"] != auth.ScopeRepoRead {
		t.Errorf("tok1 scopes = %d, want ScopeRepoRead", byID["tok1"])
	}
	if byID["tok2"] != auth.ScopeRepoAdmin {
		t.Errorf("tok2 scopes = %d, want ScopeRepoAdmin", byID["tok2"])
	}
}
```

Confirm imports: `"context"`, `"errors"`, `"time"`, `"testing"`, `"github.com/bucketvcs/bucketvcs/internal/auth"`. Adapt if `openTestStore` is named differently in the existing test file.

Also: if `auth.ErrNoSuchToken` doesn't already exist, locate the existing not-found token sentinel:
```bash
grep -n "ErrNoSuchToken\|no.*such.*token" internal/auth/errors.go
```
If missing, add it.

- [ ] **Step 3: Run tests to verify they fail**

```bash
go test ./internal/auth/sqlitestore/... -run "TestCreateTokenWithScopes|TestRotateToken|TestListTokensIncludesScopes" -count=1
```
Expected: FAIL with "too many arguments" / "undefined: store.RotateToken" / "tok.Scopes undefined".

- [ ] **Step 4: Extend Token struct**

Edit `internal/auth/sqlitestore/store.go`. Find `type Token struct`:

```bash
grep -nA10 "^type Token struct" internal/auth/sqlitestore/store.go
```

Add the Scopes field:

```go
type Token struct {
    ID         string
    UserID     string
    SecretHash string
    Label      string
    CreatedAt  int64
    ExpiresAt  *int64
    LastUsedAt *int64
    RevokedAt  *int64
    Scopes     auth.TokenScope // M17
}
```

- [ ] **Step 5: Extend CreateToken signature**

Find `func (s *Store) CreateToken`:

```bash
grep -nA15 "^func (s \*Store) CreateToken" internal/auth/sqlitestore/store.go
```

Add `scopes auth.TokenScope` as the last parameter:

```go
func (s *Store) CreateToken(ctx context.Context, id, userID, secretHash, label string,
    expiresAt *int64, scopes auth.TokenScope) error {
    if !isSafeTokenIDPrefix(id) {
        return fmt.Errorf("auth: unsafe token id prefix: %q", id)
    }
    var expVal sql.NullInt64
    if expiresAt != nil {
        expVal.Valid = true
        expVal.Int64 = *expiresAt
    }
    _, err := s.db.ExecContext(ctx,
        `INSERT INTO tokens (id, user_id, secret_hash, label, created_at, expires_at, scopes)
         VALUES (?, ?, ?, ?, ?, ?, ?)`,
        id, userID, secretHash, label, time.Now().Unix(), expVal, int64(scopes),
    )
    if err != nil {
        return fmt.Errorf("auth: create token: %w", err)
    }
    return nil
}
```

(Preserve the rest of the existing function body. The only changes are the new parameter and the new column in the INSERT.)

- [ ] **Step 6: Update GetTokenByID + ListTokensForUser to read Scopes**

Find the existing SELECT statements:

```bash
grep -nA5 "func.*GetTokenByID\|func.*ListTokensForUser" internal/auth/sqlitestore/store.go
```

Update the SELECT column list to include `scopes` and the Scan call to receive it.

In `GetTokenByID`:

```go
row := s.db.QueryRowContext(ctx,
    `SELECT id, user_id, secret_hash, label, created_at,
            expires_at, last_used_at, revoked_at, scopes
     FROM tokens WHERE id=?`, id)
var t Token
var exp, lu, rv sql.NullInt64
var scopesRaw int64
if err := row.Scan(&t.ID, &t.UserID, &t.SecretHash, &t.Label, &t.CreatedAt,
    &exp, &lu, &rv, &scopesRaw); err != nil {
    // ... existing error handling ...
}
t.Scopes = auth.TokenScope(scopesRaw)
// ... existing nullable handling for exp/lu/rv ...
```

Match the existing pattern for `exp.Valid`/`lu.Valid` etc. Same shape in ListTokensForUser.

- [ ] **Step 7: Implement RotateToken**

Append (or insert near RevokeToken) to `internal/auth/sqlitestore/store.go`:

```go
// RotateToken atomically replaces secret_hash. Old hash is overwritten.
// Does NOT change expires_at or scopes. Returns ErrNoSuchToken on missing id
// (or already-revoked: revoked tokens fail to rotate — they must be
// recreated with a fresh ID).
func (s *Store) RotateToken(ctx context.Context, id, newSecretHash string) error {
    res, err := s.db.ExecContext(ctx,
        `UPDATE tokens SET secret_hash=? WHERE id=? AND revoked_at IS NULL`,
        newSecretHash, id,
    )
    if err != nil {
        return fmt.Errorf("auth: rotate token: %w", err)
    }
    n, err := res.RowsAffected()
    if err != nil {
        return fmt.Errorf("auth: rotate token rows affected: %w", err)
    }
    if n == 0 {
        return auth.ErrNoSuchToken
    }
    return nil
}
```

- [ ] **Step 8: Update existing CreateToken callers**

`go vet ./...` will list every call site that breaks on the new signature. The most common are:

```bash
grep -rn "CreateToken(" cmd/ internal/ --include="*.go" | grep -v "_test\|CreateToken(ctx" | head -10
```

For each call site that doesn't already pass scopes, add `auth.ScopeLegacy` as the new arg (preserves M4 behavior pre-M17):

```go
// Before:
store.CreateToken(ctx, id, userID, hash, label, expPtr)
// After:
store.CreateToken(ctx, id, userID, hash, label, expPtr, auth.ScopeLegacy)
```

Tests that already exist in the codebase (other than the new ones from Step 2) get the same treatment: pass `auth.ScopeLegacy` to preserve pre-M17 behavior.

- [ ] **Step 9: Run tests**

```bash
go test ./internal/auth/... -count=1
```
Expected: PASS for M17 + existing M4 tests.

- [ ] **Step 10: Build + vet**

```bash
go vet ./... && go build ./...
```
Expected: clean. Any remaining `CreateToken(...)` call-site errors get the same `auth.ScopeLegacy` fix.

- [ ] **Step 11: Commit**

```bash
git add internal/auth/sqlitestore/migrations/0008_token_scopes.sql \
        internal/auth/sqlitestore/store.go \
        internal/auth/sqlitestore/store_test.go
# Plus any callers updated in Step 8:
git add -u
git commit -m "auth: tokens.scopes column + RotateToken; CreateToken signature gains scopes (M17 Task 2)"
```

---

### Task 3: HTTPS Basic auth populates Actor.Scopes; gateway enforcement

**Files:**
- Modify: `internal/gateway/auth.go` — populate `Actor.Scopes` after token resolution
- Modify: `internal/gateway/receive_pack.go` — call `auth.CheckScope(actor, ScopeRepoWrite)` before existing perm check
- Modify: `internal/gateway/upload_pack.go` (or wherever upload-pack is routed) — `auth.CheckScope(actor, ScopeRepoRead)`

- [ ] **Step 1: Survey existing gateway auth flow**

```bash
grep -nE "RunAuth|VerifyCredential|Actor{" internal/gateway/auth.go | head -10
```

Read enough of `internal/gateway/auth.go` to find where the token is resolved into an `Actor`. The point where `Actor` is constructed is where we set `actor.Scopes = token.Scopes`.

- [ ] **Step 2: No unit test at this layer — smoke covers it**

The behavioral guarantee for Task 3 (gateway calls CheckScope; Actor.Scopes is populated from the token row) is covered by:
- Task 1's CheckScope unit tests (the helper itself, with all hierarchy and bypass cases)
- Task 7's smoke test (end-to-end via real HTTPS Basic + git clone/push against a scoped token)

A unit-level test that asserts `actor.Scopes == tok.Scopes` requires substantial setup of the existing gateway test harness (`internal/gateway/auth_test.go`) and would duplicate what the smoke validates. If the existing `TestRunAuth` harness makes a single-line assertion trivial, the implementer should add it; otherwise rely on the smoke.

- [ ] **Step 3: Wire Actor.Scopes in gateway/auth.go**

Edit `internal/gateway/auth.go`. Find the point where the token has been resolved (call to something like `store.GetTokenByID` or `auth.Store.VerifyCredential`). After successful resolution, populate the Actor:

```go
// (existing) after token resolution succeeds:
actor := &auth.Actor{
    UserID:  user.ID,
    Name:    user.Name,
    IsAdmin: user.IsAdmin,
    Scopes:  tok.Scopes,  // M17: NEW
}
```

If the existing code already has `actor := &auth.Actor{...}`, just add the Scopes field. If the construction is split across helpers, set `actor.Scopes = tok.Scopes` immediately after.

For the SSH key auth path (M6), Actor.Scopes is left at the zero value (`ScopeLegacy`), which CheckScope short-circuits as a bypass per spec §1.

- [ ] **Step 4: Enforce ScopeRepoRead on upload-pack**

Find the upload-pack handler:

```bash
grep -rn "upload-pack\|UploadPack\|handleUploadPack\|ServeUploadPack" internal/gateway/ | head -10
```

In the handler (likely `internal/gateway/upload_pack.go` or named similar), after the existing auth resolution but before serving the protocol, add:

```go
if err := auth.CheckScope(actor, auth.ScopeRepoRead); err != nil {
    http.Error(w, "insufficient scope: token lacks repo:read", http.StatusForbidden)
    return
}
```

If the existing perm check already runs, place the CheckScope call right next to it (before or after — both must pass).

- [ ] **Step 5: Enforce ScopeRepoWrite on receive-pack**

Find the receive-pack handler:

```bash
grep -n "ReceivePack\|receive-pack" internal/gateway/receive_pack.go
```

Add the parallel check:

```go
if err := auth.CheckScope(actor, auth.ScopeRepoWrite); err != nil {
    http.Error(w, "insufficient scope: token lacks repo:write", http.StatusForbidden)
    return
}
```

- [ ] **Step 6: Run gateway tests**

```bash
go test ./internal/gateway/... -count=1
```
Expected: PASS. Existing tests should continue to work because all paths use legacy tokens (scopes=0).

- [ ] **Step 7: Build + vet**

```bash
go vet ./... && go build ./...
```
Expected: clean.

- [ ] **Step 8: Commit**

```bash
git add internal/gateway/auth.go internal/gateway/upload_pack.go \
        internal/gateway/receive_pack.go
git commit -m "gateway: populate Actor.Scopes + enforce repo scopes on upload/receive-pack (M17 Task 3)"
```

---

### Task 4: LFS enforcement (download, verify, locks)

**Files:**
- Modify: `internal/lfs/handler.go` — `CheckScope(actor, ScopeLFSRead)` on the download path
- Modify: `internal/lfs/proxied.go` — `CheckScope(actor, ScopeLFSWrite)` on verify
- Modify: `internal/lfs/locks_handler.go` — `CheckScope(actor, ScopeLFSWrite)` on create + release

- [ ] **Step 1: Survey existing LFS auth checks**

```bash
grep -nE "actor|Actor|RunAuth" internal/lfs/handler.go internal/lfs/proxied.go internal/lfs/locks_handler.go | head -20
```

Identify where each route already validates the user's permission (e.g. checks `repo_permissions` for read/write). The CheckScope call goes adjacent to that check.

- [ ] **Step 2: Enforce ScopeLFSRead on LFS download**

Edit `internal/lfs/handler.go`. Find the download/GET-object handler. After actor resolution, add:

```go
if err := auth.CheckScope(actor, auth.ScopeLFSRead); err != nil {
    http.Error(w, "insufficient scope: token lacks lfs:read", http.StatusForbidden)
    return
}
```

- [ ] **Step 3: Enforce ScopeLFSWrite on verify**

Edit `internal/lfs/proxied.go`. Find `serveVerify` (or whatever the verify handler is named):

```bash
grep -n "serveVerify\|/verify\|HandleVerify" internal/lfs/proxied.go
```

Add the check before the existing quota/audit logic:

```go
if err := auth.CheckScope(actor, auth.ScopeLFSWrite); err != nil {
    http.Error(w, "insufficient scope: token lacks lfs:write", http.StatusForbidden)
    return
}
```

- [ ] **Step 4: Enforce ScopeLFSWrite on lock create + release**

Edit `internal/lfs/locks_handler.go`. Find both `handleLocksCreate` and `handleLocksUnlock`:

```bash
grep -n "handleLocksCreate\|handleLocksUnlock\|handleLocksVerify" internal/lfs/locks_handler.go
```

For each, add `auth.CheckScope(actor, auth.ScopeLFSWrite)` after the existing user-perm check. The verify endpoint (POST /locks/verify) is a READ operation — gate it on `auth.ScopeLFSRead`.

```go
// handleLocksCreate:
if err := auth.CheckScope(actor, auth.ScopeLFSWrite); err != nil {
    http.Error(w, "insufficient scope: token lacks lfs:write", http.StatusForbidden)
    return
}

// handleLocksUnlock:
if err := auth.CheckScope(actor, auth.ScopeLFSWrite); err != nil {
    http.Error(w, "insufficient scope: token lacks lfs:write", http.StatusForbidden)
    return
}

// handleLocksList / handleLocksVerify:
if err := auth.CheckScope(actor, auth.ScopeLFSRead); err != nil {
    http.Error(w, "insufficient scope: token lacks lfs:read", http.StatusForbidden)
    return
}
```

- [ ] **Step 5: Run LFS tests**

```bash
go test ./internal/lfs/... -count=1
```
Expected: PASS (existing tests use legacy tokens which bypass).

- [ ] **Step 6: Build + vet**

```bash
go vet ./... && go build ./...
```
Expected: clean.

- [ ] **Step 7: Commit**

```bash
git add internal/lfs/handler.go internal/lfs/proxied.go internal/lfs/locks_handler.go
git commit -m "lfs: enforce lfs:read/write scopes at download/verify/locks (M17 Task 4)"
```

---

### Task 5: CLI — `--scopes` flag + `rotate` subcommand

**Files:**
- Modify: `cmd/bucketvcs/token.go` — `--scopes` flag on create; new `rotate` subcommand; list shows scopes column
- Modify: `cmd/bucketvcs/token_test.go` (create if needed)

- [ ] **Step 1: Write failing tests**

Append to `cmd/bucketvcs/token_test.go` (or create with the package declaration if missing):

```go
package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

func setupTokenTestDB(t *testing.T, user string) string {
	t.Helper()
	tmp := t.TempDir()
	authDB := filepath.Join(tmp, "auth.db")
	store, err := sqlitestore.Open(authDB)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	if err := store.CreateUser(context.Background(), "u1", user, false); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return authDB
}

func TestTokenCreate_WithScopes(t *testing.T) {
	authDB := setupTokenTestDB(t, "alice")
	var stdout, stderr bytes.Buffer
	rc := runToken(context.Background(), []string{
		"create", "--auth-db=" + authDB, "--scopes=repo:read,lfs:read", "alice",
	}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("create with scopes: rc=%d, stderr=%s", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "scopes=repo:read,lfs:read") {
		t.Errorf("output missing scopes=repo:read,lfs:read: %s", stdout.String())
	}
}

func TestTokenCreate_NoScopesWarns(t *testing.T) {
	authDB := setupTokenTestDB(t, "alice")
	var stdout, stderr bytes.Buffer
	rc := runToken(context.Background(), []string{
		"create", "--auth-db=" + authDB, "alice",
	}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("create no scopes: rc=%d, stderr=%s", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "warning:") || !strings.Contains(stderr.String(), "scopes") {
		t.Errorf("expected scopes warning on stderr, got %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "scopes=legacy") {
		t.Errorf("output should show scopes=legacy: %s", stdout.String())
	}
}

func TestTokenCreate_BogusScopes(t *testing.T) {
	authDB := setupTokenTestDB(t, "alice")
	var stdout, stderr bytes.Buffer
	rc := runToken(context.Background(), []string{
		"create", "--auth-db=" + authDB, "--scopes=bogus", "alice",
	}, &stdout, &stderr)
	if rc != 2 {
		t.Errorf("bogus scopes: rc=%d, want 2", rc)
	}
}

func TestTokenList_ShowsScopes(t *testing.T) {
	authDB := setupTokenTestDB(t, "alice")
	var stdout, stderr bytes.Buffer
	_ = runToken(context.Background(), []string{
		"create", "--auth-db=" + authDB, "--scopes=repo:write", "alice",
	}, &stdout, &stderr)
	stdout.Reset()
	stderr.Reset()
	rc := runToken(context.Background(), []string{
		"list", "--auth-db=" + authDB, "alice",
	}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("list: rc=%d, stderr=%s", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "scopes") {
		t.Errorf("list header should mention 'scopes': %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "repo:write") {
		t.Errorf("list should show repo:write: %s", stdout.String())
	}
}

func TestTokenRotate(t *testing.T) {
	authDB := setupTokenTestDB(t, "alice")
	var stdout, stderr bytes.Buffer
	// Create.
	rc := runToken(context.Background(), []string{
		"create", "--auth-db=" + authDB, "--scopes=repo:read", "alice",
	}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("create: rc=%d, stderr=%s", rc, stderr.String())
	}
	// Extract token id from the create output. The line is like:
	// "id=bvtk_xxxxxxxxxxxx  token=..."
	createOut := stdout.String()
	idx := strings.Index(createOut, "id=")
	if idx < 0 {
		t.Fatalf("create output missing id=: %s", createOut)
	}
	tail := createOut[idx+len("id="):]
	end := strings.IndexAny(tail, " \t\n\r")
	tokenID := tail[:end]

	stdout.Reset()
	stderr.Reset()
	rc = runToken(context.Background(), []string{
		"rotate", "--auth-db=" + authDB, "--id=" + tokenID,
	}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rotate: rc=%d, stderr=%s", rc, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "rotated") {
		t.Errorf("rotate output missing 'rotated': %s", out)
	}
	if !strings.Contains(out, "token=") {
		t.Errorf("rotate output missing new token: %s", out)
	}
}

func TestTokenRotate_NotFound(t *testing.T) {
	authDB := setupTokenTestDB(t, "alice")
	var stdout, stderr bytes.Buffer
	rc := runToken(context.Background(), []string{
		"rotate", "--auth-db=" + authDB, "--id=nonexistent",
	}, &stdout, &stderr)
	if rc != 2 {
		t.Errorf("rotate nonexistent: rc=%d, want 2", rc)
	}
}
```

Note: the exact `id=` output format may differ; adjust the extraction based on the existing M4 `token create` output. If M4 prints `token=<plaintext>` only without an `id=` line, the rotate test needs to query the authdb directly to find the id.

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test ./cmd/bucketvcs/... -run "TestTokenCreate|TestTokenList|TestTokenRotate" -count=1
```
Expected: FAIL with "flag provided but not defined: -scopes" / "token: unknown subcommand \"rotate\"".

- [ ] **Step 3: Add --scopes to tokenCreate**

Edit `cmd/bucketvcs/token.go::tokenCreate`. Add a flag near the existing flag declarations:

```go
scopesFlag := fs.String("scopes", "",
    "Token scopes (csv|all|repo:*|lfs:*); empty means legacy (full user permissions)")
```

Update the `reorderFlagsFirst` map to mark `scopes` as taking a value (per the existing reorder helper's contract — check whether string flags need to be listed).

After parsing, resolve scopes:

```go
scopes := auth.ScopeLegacy
if *scopesFlag != "" {
    parsed, err := auth.ParseScopes(*scopesFlag)
    if err != nil {
        fmt.Fprintf(stderr, "invalid --scopes: %v\n", err)
        return 2
    }
    scopes = parsed
} else {
    fmt.Fprintln(stderr,
        "warning: no --scopes set; token has full user permissions (legacy mode)")
}
```

Pass `scopes` to `CreateToken` (signature was updated in Task 2):

```go
err := store.CreateToken(ctx, tokenID, user.ID, hash, *label, expPtr, scopes)
```

After successful create, extend the existing output to show scopes:

```go
fmt.Fprintf(stdout, "id=%s\ntoken=%s\nscopes=%s\n",
    tokenID, plaintext, auth.FormatScopes(scopes))
```

(Preserve whatever the existing output format is; just append `scopes=<formatted>`.)

Add `"github.com/bucketvcs/bucketvcs/internal/auth"` if not already in imports.

- [ ] **Step 4: Extend tokenList with scopes column**

Edit `cmd/bucketvcs/token.go::tokenList`. The current output is something like:

```go
fmt.Fprintln(stdout, "id\tlabel\tcreated\texpires\trevoked\tlast-used")
```

Add `scopes` as the last column:

```go
fmt.Fprintln(stdout, "id\tlabel\tcreated\texpires\trevoked\tlast-used\tscopes")
for _, t := range tokens {
    // ... existing row formatting ...
    fmt.Fprintf(stdout, "...%s\n", auth.FormatScopes(t.Scopes))
}
```

Match the existing row format — just append the scopes column at the end.

- [ ] **Step 5: Add the rotate subcommand**

In `cmd/bucketvcs/token.go::runToken` switch, add a case:

```go
case "rotate":
    return tokenRotate(ctx, rest, stdout, stderr)
```

Update the usage line:

```go
fmt.Fprintln(stderr, "usage: bucketvcs token <create|list|revoke|rotate>")
```

Add the handler at the bottom of the file:

```go
func tokenRotate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
    fs := flag.NewFlagSet("token rotate", flag.ContinueOnError)
    authDB := fs.String("auth-db", "", "path to auth.db")
    id := fs.String("id", "", "token id (required)")
    fs.SetOutput(stderr)
    if err := fs.Parse(reorderFlagsFirst(args, nil)); err != nil {
        return 2
    }
    if *authDB == "" || *id == "" {
        fmt.Fprintln(stderr, "usage: bucketvcs token rotate --auth-db <path> --id <token-id>")
        return 2
    }
    s, _, err := openAuthDB(*authDB)
    if err != nil {
        fmt.Fprintf(stderr, "auth-db: %v\n", err)
        return 1
    }
    defer s.Close()
    // Generate a new secret. Use the same helper as tokenCreate — find it
    // via grep ("rand.Read\|generateToken\|newToken"). Likely returns
    // (plaintext, hash) or similar.
    plaintext, hash, err := generateTokenSecret()
    if err != nil {
        fmt.Fprintf(stderr, "generate secret: %v\n", err)
        return 1
    }
    if err := s.RotateToken(ctx, *id, hash); err != nil {
        if errors.Is(err, auth.ErrNoSuchToken) {
            fmt.Fprintf(stderr, "token rotate: %v\n", err)
            return 2
        }
        fmt.Fprintf(stderr, "token rotate: %v\n", err)
        return 1
    }
    fmt.Fprintf(stdout, "id=%s  rotated\ntoken=%s   # store this now — it will not be shown again\n",
        *id, plaintext)
    return 0
}
```

If there's no shared `generateTokenSecret` helper in `cmd/bucketvcs/token.go`, locate how `tokenCreate` generates its plaintext + hash and replicate that logic inline (or extract a helper as part of this task).

- [ ] **Step 6: Run tests**

```bash
go test ./cmd/bucketvcs/... -run "TestToken" -count=1
```
Expected: PASS.

- [ ] **Step 7: Build + vet**

```bash
go vet ./... && go build ./...
```
Expected: clean.

- [ ] **Step 8: Commit**

```bash
git add cmd/bucketvcs/token.go cmd/bucketvcs/token_test.go
git commit -m "cmd/token: --scopes flag + rotate subcommand + scopes column (M17 Task 5)"
```

---

### Task 6: Observability (audit emitters)

**Files:**
- Modify: `internal/auth/audit.go` (locate first — may not exist; may be in `internal/auth/sqlitestore/audit.go` or inlined in `store.go`)
- Modify: `cmd/bucketvcs/token.go::tokenRotate` to emit `auth.token.rotated`
- Modify: `internal/gateway/auth.go` (or wherever CheckScope is called) to emit `auth.scope.denied`

- [ ] **Step 1: Survey existing auth audit pattern**

```bash
grep -rn "auth\\.token\\.\\|auth\\.\\|EmitToken\\|slog.LogAttrs.*auth" internal/auth/ cmd/bucketvcs/ --include="*.go" | head -20
```

Identify whether M4 has an audit emitter file (`internal/auth/audit.go`) or inlines slog calls in store.go / token.go. Match the existing pattern.

- [ ] **Step 2: Add EmitTokenRotated + EmitScopeDenied**

If a `internal/auth/audit.go` file exists (matching the M14 `internal/policy/audit.go` pattern), append:

```go
// EmitTokenRotated logs the auth.token.rotated audit event.
func EmitTokenRotated(ctx context.Context, logger *slog.Logger,
	tokenID, userID, actor string) {
    if logger == nil {
        logger = slog.Default()
    }
    logger.LogAttrs(ctx, slog.LevelInfo, "auth.token.rotated",
        slog.String("token_id", tokenID),
        slog.String("user_id", userID),
        slog.String("actor", actor),
    )
}

// EmitScopeDenied logs the auth.scope.denied audit event when CheckScope
// rejects an operation. token_id_prefix is the first 8 chars of the token
// id (never the full id, never the secret).
func EmitScopeDenied(ctx context.Context, logger *slog.Logger,
	userID, tokenIDPrefix, tenant, repo, operation string,
	required, granted TokenScope) {
    if logger == nil {
        logger = slog.Default()
    }
    logger.LogAttrs(ctx, slog.LevelWarn, "auth.scope.denied",
        slog.String("user_id", userID),
        slog.String("token_id_prefix", tokenIDPrefix),
        slog.String("tenant", tenant),
        slog.String("repo", repo),
        slog.String("operation", operation),
        slog.String("required_scope", FormatScopes(required)),
        slog.String("granted_scopes", FormatScopes(granted)),
    )
}
```

If no audit.go exists, create one with the appropriate `package auth` declaration and imports.

- [ ] **Step 3: Wire EmitTokenRotated in tokenRotate**

Edit `cmd/bucketvcs/token.go::tokenRotate`. After the successful RotateToken call but before printing the new secret:

```go
// Look up the token to populate the audit fields.
tok, _ := s.GetTokenByID(ctx, *id)
userID := ""
if tok != nil {
    userID = tok.UserID
}
auth.EmitTokenRotated(ctx, nil, *id, userID, cliActor())
```

- [ ] **Step 4: Wire EmitScopeDenied at enforcement sites**

This is more invasive — each CheckScope call site needs to know the operation name, tenant, and repo. The pattern: when CheckScope returns ErrInsufficientScope, emit before returning 403.

Update the helper pattern in each site (gateway + lfs):

```go
if err := auth.CheckScope(actor, auth.ScopeRepoWrite); err != nil {
    auth.EmitScopeDenied(r.Context(), logger,
        actor.UserID, tokenIDPrefix(actor),
        tenant, repo, "receive-pack",
        auth.ScopeRepoWrite, actor.Scopes)
    http.Error(w, "insufficient scope: token lacks repo:write", http.StatusForbidden)
    return
}
```

Where `tokenIDPrefix(actor)` is a tiny helper that returns the first 8 chars of the token ID if known, else empty. The Actor struct doesn't currently carry the token ID — if it does after Task 3's wiring, use it; otherwise pass `""` and document this as a future-improvement gap.

Adding `tokenID string` to Actor is also reasonable if the gateway's existing auth flow has it in scope. Decide during implementation.

- [ ] **Step 5: Run tests**

```bash
go test ./... -count=1 -timeout 180s 2>&1 | grep -E "^FAIL" | head
```
Expected: no FAIL.

- [ ] **Step 6: Build + vet**

```bash
go vet ./... && go build ./...
```
Expected: clean.

- [ ] **Step 7: Commit**

```bash
git add internal/auth/ cmd/bucketvcs/token.go \
        internal/gateway/ internal/lfs/
git commit -m "auth: EmitTokenRotated + EmitScopeDenied audit emitters at enforcement sites (M17 Task 6)"
```

(Drop any path that wasn't modified.)

---

### Task 7: Smoke

**Files:**
- Create: `scripts/m17-auth-scopes-smoke.sh`

- [ ] **Step 1: Write the smoke**

Create `scripts/m17-auth-scopes-smoke.sh`:

```bash
#!/usr/bin/env bash
# M17 end-to-end smoke for token scopes + rotation.
# - Create a user + scoped token (repo:read only)
# - git clone succeeds, git push fails 403 with "insufficient scope"
# - Rotate the token, clone still works, push still rejected
# - Issue a repo:write token; push succeeds

set -euo pipefail
trap 'echo "M17_AUTH_SCOPES_SMOKE_FAILED" >&2' ERR

WORK=$(mktemp -d)
echo "smoke working dir: $WORK"

go build -o "$WORK/bucketvcs" ./cmd/bucketvcs

AUTHDB="$WORK/auth.db"
"$WORK/bucketvcs" user create --auth-db="$AUTHDB" --tenant=acme --user=alice --password=topsecret
"$WORK/bucketvcs" repo register --auth-db="$AUTHDB" --no-init acme/site
"$WORK/bucketvcs" repo grant --auth-db="$AUTHDB" alice acme/site write

mkdir -p "$WORK/storage"

"$WORK/bucketvcs" serve \
    --listen=":12345" \
    --auth-db="$AUTHDB" \
    --storage=file://"$WORK/storage" \
    --lfs=false >"$WORK/serve.log" 2>&1 &
SERVE_PID=$!
trap 'kill $SERVE_PID 2>/dev/null || true; trap - ERR; echo "M17_AUTH_SCOPES_SMOKE_FAILED" >&2; exit 1' ERR
sleep 1

# Create a repo:read scoped token.
READ_TOKEN_OUT=$("$WORK/bucketvcs" token create \
    --auth-db="$AUTHDB" --scopes=repo:read alice 2>&1)
READ_TOKEN=$(echo "$READ_TOKEN_OUT" | grep "^token=" | head -1 | sed 's/^token=//')
READ_ID=$(echo "$READ_TOKEN_OUT" | grep "^id=" | head -1 | sed 's/^id=//')

# Clone with repo:read should work.
git clone -q "http://alice:$READ_TOKEN@127.0.0.1:12345/acme/site" "$WORK/clone1" || {
    echo "clone with repo:read failed"; cat "$WORK/serve.log"; exit 1
}

# Push with repo:read should fail 403.
( cd "$WORK/clone1"
  git config user.email smoke@local
  git config user.name smoke
  echo "x" > x.txt
  git add x.txt
  git -c commit.gpgsign=false commit -qm "x"
  if git push -q origin master 2>"$WORK/push-read.err"; then
    echo "push with repo:read succeeded; expected reject"; exit 1
  fi
)
grep -q "insufficient scope\|403\|repo:write" "$WORK/push-read.err" || {
    echo "push reject message wrong:"; cat "$WORK/push-read.err"; exit 1
}

# Rotate the read token; clone with the new secret still works.
ROTATE_OUT=$("$WORK/bucketvcs" token rotate \
    --auth-db="$AUTHDB" --id="$READ_ID" 2>&1)
NEW_TOKEN=$(echo "$ROTATE_OUT" | grep "^token=" | head -1 | sed 's/^token=//')
if [ -z "$NEW_TOKEN" ] || [ "$NEW_TOKEN" = "$READ_TOKEN" ]; then
    echo "rotate did not produce a different secret"
    exit 1
fi

rm -rf "$WORK/clone2"
git clone -q "http://alice:$NEW_TOKEN@127.0.0.1:12345/acme/site" "$WORK/clone2" || {
    echo "clone after rotate failed"; cat "$WORK/serve.log"; exit 1
}

# Issue a repo:write scoped token; push should work.
WRITE_TOKEN_OUT=$("$WORK/bucketvcs" token create \
    --auth-db="$AUTHDB" --scopes=repo:write alice 2>&1)
WRITE_TOKEN=$(echo "$WRITE_TOKEN_OUT" | grep "^token=" | head -1 | sed 's/^token=//')

( cd "$WORK/clone2"
  git config user.email smoke@local
  git config user.name smoke
  git remote set-url origin "http://alice:$WRITE_TOKEN@127.0.0.1:12345/acme/site"
  echo "y" > y.txt
  git add y.txt
  git -c commit.gpgsign=false commit -qm "y"
  git push -q origin master || {
    echo "push with repo:write failed"; cat "$WORK/serve.log"; exit 1
  }
)

kill $SERVE_PID 2>/dev/null || true
wait 2>/dev/null || true

echo "M17_AUTH_SCOPES_SMOKE_OK"
```

Make it executable:

```bash
chmod +x scripts/m17-auth-scopes-smoke.sh
```

The exact CLI flag names for `user create` and `repo register` may differ — verify by checking past smokes (e.g., `scripts/m15-webhook-smoke.sh`) and matching.

- [ ] **Step 2: Run the smoke**

```bash
bash scripts/m17-auth-scopes-smoke.sh 2>&1 | tail -10
```

Expected: ends `M17_AUTH_SCOPES_SMOKE_OK`. If any scenario fails, the smoke preserves $WORK so logs ($WORK/serve.log, $WORK/push-read.err) are inspectable.

- [ ] **Step 3: Run all prior smokes to confirm no regression**

```bash
bash scripts/m14-policy-smoke.sh 2>&1 | tail -3
bash scripts/m15-webhook-smoke.sh 2>&1 | tail -3
bash scripts/m13.5-lfs-quota-smoke.sh 2>&1 | tail -3
```

Expected: each ends `*_OK`. These all use legacy tokens (scopes=0), so M17's bypass should keep them green.

- [ ] **Step 4: Commit**

```bash
git add scripts/m17-auth-scopes-smoke.sh
git commit -m "scripts: M17 auth scopes end-to-end smoke (M17 Task 7)"
```

---

## Acceptance criteria

- 7 tasks complete
- `go test ./...` clean; `go vet ./...` clean
- `scripts/m17-auth-scopes-smoke.sh` ends `M17_AUTH_SCOPES_SMOKE_OK`
- All prior smokes still pass (legacy bypass preserves pre-M17 behavior)
- `bucketvcs token create --scopes=...` accepted; without --scopes prints stderr warning + creates legacy token
- `bucketvcs token list` shows scopes column
- `bucketvcs token rotate --id=...` produces new secret, preserves scopes + expires_at
- 2 new audit events emitted: `auth.token.rotated`, `auth.scope.denied`
- Existing pre-M17 tokens (scopes=0) keep working without operator action
- Push from a repo:read-scoped token returns 403 with `insufficient scope` in the body

## Spec coverage check

| Spec section | Task |
|---|---|
| §1 Architecture overview | Tasks 1 (TokenScope) + 3 (gateway wiring) |
| §2 Schema (migration 0008) | Task 2 |
| §3 Scope taxonomy + API | Task 1 |
| §4 Operation → required scope (mapping) | Tasks 3 (gateway) + 4 (LFS) |
| §5 Service API (Token.Scopes, CreateToken, RotateToken) | Task 2 |
| §6 Subject (Actor) extension | Task 1 (field) + Task 3 (population) |
| §7 Operator CLI | Task 5 |
| §8 Observability (audit emitters) | Task 6 |
| §9 Failure modes | covered in each task's error handling |
| §10 Testing | unit/store/CLI tests in Tasks 1/2/5; smoke in Task 7 |
| §11 Deferred | documented in spec; nothing to implement |
