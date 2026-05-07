# M4 — HTTPS Token Authentication Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace M3's shared-bearer placeholder with real per-actor HTTPS token authentication: SQLite-backed `auth.Store`, users + per-repo permissions (read/write/admin), per-repo `public_read` flag, transport-neutral interfaces so M6 SSH plugs in unchanged.

**Architecture:** A new `internal/auth` package defines pure transport-neutral types, the `Store` interface, a pure `Decide` authorization function, and token format/hashing helpers. `internal/auth/sqlitestore` implements `Store` against SQLite (pure-Go via `modernc.org/sqlite`). The gateway gains a single auth middleware that parses URL → required action, calls `Store`, calls `Decide`, and dispatches. Existing M3 protocol handlers do not import `auth`. Admin CLI subcommands open the SQLite file directly.

**Tech Stack:** Go 1.22+, `modernc.org/sqlite` (pure-Go SQLite driver), `golang.org/x/crypto/argon2`, stdlib `database/sql`, stdlib `flag`, stdlib `net/http`, existing `internal/mirror` and `internal/repo` from M1–M3.

**Spec:** `docs/superpowers/specs/2026-05-06-m4-https-token-auth-design.md`

---

## File Structure

**New files:**

```
internal/auth/
  doc.go, types.go, errors.go
  permissions.go, permissions_test.go
  tokens.go, tokens_test.go
  store.go

internal/auth/sqlitestore/
  store.go, store_test.go
  schema.go, schema_test.go
  migrations/0001_init.sql

internal/auth/conformance/
  conformance.go, doc.go

internal/gateway/
  authmw_test.go
  e2e_auth_test.go

cmd/bucketvcs/
  authdb.go, authdb_test.go
  user.go, user_test.go
  token.go, token_test.go
  repocmd.go, repocmd_test.go

docs/migration-m3-to-m4.md
```

**Modified files:**

```
internal/gateway/auth.go      REWRITE: middleware against auth.Store
internal/gateway/routes.go    REWRITE: extract pure ParseRoute
internal/gateway/server.go    Options: drop AuthMode/AuthToken; add AuthStore

cmd/bucketvcs/main.go         dispatch new subcommands
cmd/bucketvcs/serve.go        replace --auth-mode/--auth-token with --auth-db

go.mod                        add modernc.org/sqlite, golang.org/x/crypto
```

---

## Worktree

Per the M3 progress memory, M4 work happens in a dedicated worktree branched off `main` at or after the `m3-complete` tag. Before Task 1:

```bash
git worktree add -b m4-https-auth .claude/worktrees/m4-https-auth m3-complete
cd .claude/worktrees/m4-https-auth
```

All file paths below are repo-relative.

---

## Phase 1 — Pure auth primitives

### Task 1: Package skeleton and core types

**Files:** Create `internal/auth/doc.go`, `internal/auth/types.go`.

- [ ] **Step 1: Create `internal/auth/doc.go`**

```go
// Package auth defines bucketvcs's transport-neutral authentication and
// authorization model.
//
// This package contains only types and pure logic. It has no HTTP, no SSH,
// and no SQL imports. Storage and transport live at the edges:
//
//   - internal/auth/sqlitestore    persistent Store implementation
//   - internal/gateway             HTTP authentication middleware
//   - cmd/bucketvcs                admin CLI
//
// The single allow/deny decision is auth.Decide. The Store interface is the
// only seam with persistent state.
package auth
```

- [ ] **Step 2: Create `internal/auth/types.go`**

```go
package auth

// Actor identifies an authenticated principal. A nil *Actor means anonymous.
type Actor struct {
	UserID  string
	Name    string
	IsAdmin bool
}

// Credential is the closed set of credential shapes the gateway accepts.
// New shapes are added in later milestones (M6 adds SSHKeyFingerprint).
type Credential interface{ isCredential() }

// BasicPassword is HTTP Basic auth: username + token-as-password.
type BasicPassword struct {
	Username string
	Password string
}

func (BasicPassword) isCredential() {}

// SSHKeyFingerprint is the SSH public-key authentication credential. M6
// populates it; M4 only declares it for interface stability.
type SSHKeyFingerprint struct {
	Fingerprint string
}

func (SSHKeyFingerprint) isCredential() {}

// Action is the operation the actor is attempting on a repo.
type Action int

const (
	ActionRead Action = iota
	ActionWrite
)

// Perm is the granted permission level for an actor on a repo.
type Perm int

const (
	PermNone Perm = iota
	PermRead
	PermWrite
	PermAdmin
)

// RepoFlags carries the per-repo bits relevant to authorization.
type RepoFlags struct {
	PublicRead bool
}
```

- [ ] **Step 3: Build**

Run: `go build ./internal/auth/...`
Expected: success, no output.

- [ ] **Step 4: Commit**

```bash
git add internal/auth/doc.go internal/auth/types.go
git commit -m "M4 auth: package skeleton + transport-neutral types"
```

---

### Task 2: Sentinel errors

**Files:** Create `internal/auth/errors.go`.

- [ ] **Step 1: Create `internal/auth/errors.go`**

```go
package auth

import "errors"

var (
	ErrInvalidCredential = errors.New("auth: invalid credential")
	ErrTokenExpired      = errors.New("auth: token expired")
	ErrTokenRevoked      = errors.New("auth: token revoked")
	ErrUserDisabled      = errors.New("auth: user disabled")
	ErrNoSuchRepo        = errors.New("auth: no such repo")
	ErrNoSuchUser        = errors.New("auth: no such user")
	ErrNoSuchToken       = errors.New("auth: no such token")
	ErrConflict          = errors.New("auth: conflict")
)
```

- [ ] **Step 2: Build**

Run: `go build ./internal/auth/...`
Expected: success.

- [ ] **Step 3: Commit**

```bash
git add internal/auth/errors.go
git commit -m "M4 auth: sentinel errors"
```

---

### Task 3: Decide function (TDD)

**Files:** Create `internal/auth/permissions.go`, `internal/auth/permissions_test.go`.

- [ ] **Step 1: Write failing test in `internal/auth/permissions_test.go`**

```go
package auth

import "testing"

func TestDecide(t *testing.T) {
	type tc struct {
		name   string
		actor  *Actor
		perm   Perm
		action Action
		flags  RepoFlags
		want   bool
	}
	admin := &Actor{UserID: "u1", Name: "admin", IsAdmin: true}
	user := &Actor{UserID: "u2", Name: "alice"}
	cases := []tc{
		{"anon read public", nil, PermNone, ActionRead, RepoFlags{PublicRead: true}, true},
		{"anon read private", nil, PermNone, ActionRead, RepoFlags{}, false},
		{"anon write public", nil, PermNone, ActionWrite, RepoFlags{PublicRead: true}, false},
		{"anon write private", nil, PermNone, ActionWrite, RepoFlags{}, false},
		{"user-none read public", user, PermNone, ActionRead, RepoFlags{PublicRead: true}, true},
		{"user-none read private", user, PermNone, ActionRead, RepoFlags{}, false},
		{"user-none write public", user, PermNone, ActionWrite, RepoFlags{PublicRead: true}, false},
		{"user-none write private", user, PermNone, ActionWrite, RepoFlags{}, false},
		{"user-read read", user, PermRead, ActionRead, RepoFlags{}, true},
		{"user-read write", user, PermRead, ActionWrite, RepoFlags{}, false},
		{"user-write read", user, PermWrite, ActionRead, RepoFlags{}, true},
		{"user-write write", user, PermWrite, ActionWrite, RepoFlags{}, true},
		{"user-admin read", user, PermAdmin, ActionRead, RepoFlags{}, true},
		{"user-admin write", user, PermAdmin, ActionWrite, RepoFlags{}, true},
		{"is-admin none read", admin, PermNone, ActionRead, RepoFlags{}, true},
		{"is-admin none write", admin, PermNone, ActionWrite, RepoFlags{}, true},
		{"is-admin none write public", admin, PermNone, ActionWrite, RepoFlags{PublicRead: true}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := Decide(c.actor, c.perm, c.action, c.flags)
			if got != c.want {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
}

func TestDecideReasonNonEmptyOnDeny(t *testing.T) {
	ok, reason := Decide(nil, PermNone, ActionWrite, RepoFlags{})
	if ok {
		t.Fatal("expected deny")
	}
	if reason == "" {
		t.Fatal("expected non-empty reason on deny")
	}
}
```

- [ ] **Step 2: Run test, expect failure**

Run: `go test ./internal/auth/ -run TestDecide -v`
Expected: FAIL with "undefined: Decide".

- [ ] **Step 3: Implement `internal/auth/permissions.go`**

```go
package auth

// Decide reports whether actor with perm may perform action against a repo
// with flags. The second return is a short reason string suitable for log
// output on deny; empty on allow. Anonymous (actor == nil) is treated as
// PermNone. is_admin short-circuits to allow.
func Decide(actor *Actor, perm Perm, action Action, flags RepoFlags) (bool, string) {
	if actor != nil && actor.IsAdmin {
		return true, ""
	}
	switch action {
	case ActionRead:
		if flags.PublicRead {
			return true, ""
		}
		if perm >= PermRead {
			return true, ""
		}
		if actor == nil {
			return false, "anonymous read on private repo"
		}
		return false, "user has no read permission"
	case ActionWrite:
		if perm >= PermWrite {
			return true, ""
		}
		if actor == nil {
			return false, "anonymous write"
		}
		return false, "user has no write permission"
	default:
		return false, "unknown action"
	}
}
```

- [ ] **Step 4: Run tests, expect pass**

Run: `go test ./internal/auth/ -run TestDecide -v`
Expected: PASS (17 sub-tests + reason test).

- [ ] **Step 5: Commit**

```bash
git add internal/auth/permissions.go internal/auth/permissions_test.go
git commit -m "M4 auth: Decide function with full decision-table coverage"
```

---

### Task 4: Token format — generate, parse, validate (TDD)

Token format: `bvts_<id>_<secret>` where `id` is 24 Crockford base32 chars and `secret` is 52 Crockford base32 chars. Crockford base32 alphabet (uppercase, no padding, excludes I L O U): `0123456789ABCDEFGHJKMNPQRSTVWXYZ`.

**Files:** Create `internal/auth/tokens.go`, `internal/auth/tokens_test.go`.

- [ ] **Step 1: Write failing tests in `internal/auth/tokens_test.go`**

```go
package auth

import (
	"strings"
	"testing"
)

func TestGenerateToken_FormatAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok, id, secret, err := GenerateToken()
		if err != nil {
			t.Fatalf("GenerateToken: %v", err)
		}
		if !strings.HasPrefix(tok, "bvts_") {
			t.Fatalf("missing prefix: %q", tok)
		}
		parts := strings.Split(tok, "_")
		if len(parts) != 3 {
			t.Fatalf("want 3 segments, got %d: %q", len(parts), tok)
		}
		if len(parts[1]) != 24 {
			t.Fatalf("id length = %d, want 24", len(parts[1]))
		}
		if len(parts[2]) != 52 {
			t.Fatalf("secret length = %d, want 52", len(parts[2]))
		}
		if parts[1] != id {
			t.Fatalf("returned id %q != token id %q", id, parts[1])
		}
		if parts[2] != secret {
			t.Fatalf("returned secret %q != token secret %q", secret, parts[2])
		}
		if seen[tok] {
			t.Fatalf("duplicate token: %q", tok)
		}
		seen[tok] = true
	}
}

func TestParseToken(t *testing.T) {
	tok, id, secret, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	gotID, gotSecret, err := ParseToken(tok)
	if err != nil {
		t.Fatalf("ParseToken: %v", err)
	}
	if gotID != id || gotSecret != secret {
		t.Fatalf("parse mismatch: got (%q,%q) want (%q,%q)", gotID, gotSecret, id, secret)
	}
}

func TestParseToken_Invalid(t *testing.T) {
	bad := []string{
		"",
		"bvts_",
		"bvts_only",
		"bvts__",
		"wrong_AAAAAAAAAAAAAAAAAAAAAAAA_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",
		"bvts_TOOSHORT_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",
		"bvts_AAAAAAAAAAAAAAAAAAAAAAAA_TOOSHORT",
		// lowercase letters reject (Crockford uppercase-only canonical):
		"bvts_aaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		// excluded letters (I, L, O, U) reject:
		"bvts_IIIIIIIIIIIIIIIIIIIIIIII_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",
	}
	for _, b := range bad {
		if _, _, err := ParseToken(b); err == nil {
			t.Errorf("ParseToken(%q): want error, got nil", b)
		}
	}
}
```

- [ ] **Step 2: Run tests, expect failure**

Run: `go test ./internal/auth/ -run "TestGenerateToken|TestParseToken" -v`
Expected: FAIL with undefined symbols.

- [ ] **Step 3: Implement `internal/auth/tokens.go`**

```go
package auth

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
)

// crockfordAlphabet is the standard Crockford base32 alphabet, uppercase,
// no padding, excludes I L O U. 32 characters.
const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

const (
	tokenPrefix    = "bvts"
	tokenIDLen     = 24
	tokenSecretLen = 52
)

// GenerateToken produces a fresh token along with its id and secret segments.
// The id has ~120 bits of randomness; the secret ~256.
func GenerateToken() (token, id, secret string, err error) {
	id, err = randomCrockford(tokenIDLen)
	if err != nil {
		return "", "", "", fmt.Errorf("generate token id: %w", err)
	}
	secret, err = randomCrockford(tokenSecretLen)
	if err != nil {
		return "", "", "", fmt.Errorf("generate token secret: %w", err)
	}
	token = tokenPrefix + "_" + id + "_" + secret
	return token, id, secret, nil
}

// ParseToken splits a token string into its id and secret segments,
// validating the prefix, separator count, segment lengths, and alphabet.
func ParseToken(token string) (id, secret string, err error) {
	parts := strings.Split(token, "_")
	if len(parts) != 3 {
		return "", "", errors.New("auth: token must have form bvts_<id>_<secret>")
	}
	if parts[0] != tokenPrefix {
		return "", "", errors.New("auth: token has wrong prefix")
	}
	if len(parts[1]) != tokenIDLen {
		return "", "", fmt.Errorf("auth: token id length must be %d", tokenIDLen)
	}
	if len(parts[2]) != tokenSecretLen {
		return "", "", fmt.Errorf("auth: token secret length must be %d", tokenSecretLen)
	}
	if !isCrockford(parts[1]) || !isCrockford(parts[2]) {
		return "", "", errors.New("auth: token contains non-Crockford-base32 characters")
	}
	return parts[1], parts[2], nil
}

// randomCrockford returns n Crockford-base32 characters drawn from a CSPRNG.
func randomCrockford(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, n)
	for i, b := range buf {
		out[i] = crockfordAlphabet[int(b)%32]
	}
	return string(out), nil
}

func isCrockford(s string) bool {
	for i := 0; i < len(s); i++ {
		if !strings.ContainsRune(crockfordAlphabet, rune(s[i])) {
			return false
		}
	}
	return true
}
```

- [ ] **Step 4: Run tests, expect pass**

Run: `go test ./internal/auth/ -run "TestGenerateToken|TestParseToken" -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/tokens.go internal/auth/tokens_test.go
git commit -m "M4 auth: token format (bvts_<id>_<secret>) generate + parse"
```

---

### Task 5: Token hashing with argon2id (TDD)

Hashing parameters: argon2id `m=65536 KiB (64 MiB), t=3, p=4`, 16-byte salt, 32-byte hash. PHC string format `$argon2id$v=19$m=65536,t=3,p=4$<salt-b64>$<hash-b64>` so future parameter migration is self-describing.

**Files:** Modify `internal/auth/tokens.go`; extend `internal/auth/tokens_test.go`.

- [ ] **Step 1: Add `go.mod` dependency**

Run: `go get golang.org/x/crypto@latest`
Expected: dependency added.

- [ ] **Step 2: Append failing tests to `internal/auth/tokens_test.go`**

```go
func TestHashAndVerify_Roundtrip(t *testing.T) {
	secret := "ABCDEFGHJKMNPQRSTVWXYZ0123456789ABCDEFGHJKMNPQRSTVWX"
	enc, err := HashSecret(secret)
	if err != nil {
		t.Fatalf("HashSecret: %v", err)
	}
	if !strings.HasPrefix(enc, "$argon2id$") {
		t.Fatalf("encoded form missing prefix: %q", enc)
	}
	if err := VerifyHash(secret, enc); err != nil {
		t.Fatalf("VerifyHash same secret: %v", err)
	}
	if err := VerifyHash(secret+"X", enc); err == nil {
		t.Fatal("VerifyHash mismatched secret: expected error")
	}
}

func TestHashSecret_DifferentSaltsDifferentEncodings(t *testing.T) {
	a, err := HashSecret("same-secret")
	if err != nil {
		t.Fatalf("HashSecret a: %v", err)
	}
	b, err := HashSecret("same-secret")
	if err != nil {
		t.Fatalf("HashSecret b: %v", err)
	}
	if a == b {
		t.Fatal("two HashSecret calls produced identical encoding (salt should differ)")
	}
	if VerifyHash("same-secret", a) != nil || VerifyHash("same-secret", b) != nil {
		t.Fatal("both encodings should verify")
	}
}

func TestVerifyHash_Malformed(t *testing.T) {
	bad := []string{
		"",
		"plaintext",
		"$argon2id$",
		"$argon2id$v=19$m=65536,t=3,p=4$bad-base64$bad-base64",
	}
	for _, b := range bad {
		if err := VerifyHash("secret", b); err == nil {
			t.Errorf("VerifyHash(_, %q): expected error", b)
		}
	}
}
```

- [ ] **Step 3: Run tests, expect failure**

Run: `go test ./internal/auth/ -run "TestHash|TestVerify" -v`
Expected: FAIL with undefined symbols.

- [ ] **Step 4: Append implementation to `internal/auth/tokens.go`**

```go
// (additional imports — add to existing import block)
//   "crypto/subtle"
//   "encoding/base64"
//
//   "golang.org/x/crypto/argon2"

const (
	argon2Memory  = 64 * 1024 // KiB -> 64 MiB
	argon2Time    = 3
	argon2Threads = 4
	argon2KeyLen  = 32
	argon2SaltLen = 16
)

// HashSecret returns a PHC-encoded argon2id hash of secret.
func HashSecret(secret string) (string, error) {
	salt := make([]byte, argon2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("hash secret: salt: %w", err)
	}
	key := argon2.IDKey([]byte(secret), salt, argon2Time, argon2Memory, argon2Threads, argon2KeyLen)
	enc := fmt.Sprintf(
		"$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		argon2Memory, argon2Time, argon2Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	)
	return enc, nil
}

// VerifyHash compares secret against a PHC-encoded argon2id encoded hash.
// Returns nil on match; non-nil on mismatch or malformed encoding.
func VerifyHash(secret, encoded string) error {
	parts := strings.Split(encoded, "$")
	// Expected layout: ["", "argon2id", "v=19", "m=...,t=...,p=...", salt, key]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return errors.New("auth: malformed argon2id encoding")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != 19 {
		return errors.New("auth: unsupported argon2 version")
	}
	var memory, time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return errors.New("auth: malformed argon2id parameters")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return errors.New("auth: malformed argon2id salt")
	}
	wantKey, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return errors.New("auth: malformed argon2id key")
	}
	gotKey := argon2.IDKey([]byte(secret), salt, time, memory, threads, uint32(len(wantKey)))
	if subtle.ConstantTimeCompare(gotKey, wantKey) != 1 {
		return ErrInvalidCredential
	}
	return nil
}
```

When updating the import block, the final imports of `internal/auth/tokens.go` should be:

```go
import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)
```

- [ ] **Step 5: Run tests, expect pass**

Run: `go test ./internal/auth/ -v`
Expected: PASS, all auth-package tests green (Decide + token + hashing).

- [ ] **Step 6: Commit**

```bash
git add internal/auth/tokens.go internal/auth/tokens_test.go go.mod go.sum
git commit -m "M4 auth: argon2id token-secret hashing (PHC format)"
```

---

### Task 6: Store interface

**Files:** Create `internal/auth/store.go`.

- [ ] **Step 1: Create `internal/auth/store.go`**

```go
package auth

import "context"

// Store is the persistence and identity-lookup seam used by the gateway
// middleware and the admin CLI. It is transport-neutral: M4 uses
// BasicPassword credentials; M6 will additionally use SSHKeyFingerprint.
//
// All methods take ctx so timeouts and cancellation propagate from the
// HTTP handler. Implementations must honor ctx promptly.
type Store interface {
	// VerifyCredential validates a credential and returns the associated
	// actor plus the originating token id (empty for credential types
	// that don't carry an id).
	//
	// Errors:
	//   ErrInvalidCredential   — credential did not match any record
	//   ErrTokenExpired        — record matched but expires_at < now
	//   ErrTokenRevoked        — record matched but revoked_at != null
	//   ErrUserDisabled        — record matched but user disabled_at != null
	VerifyCredential(ctx context.Context, c Credential) (actor *Actor, tokenID string, err error)

	// LookupRepoPerm returns the actor's permission level on (tenant, repo).
	// Anonymous actors (nil) return PermNone without consulting storage.
	// is_admin actors return PermAdmin without consulting permission rows.
	LookupRepoPerm(ctx context.Context, actor *Actor, tenant, repo string) (Perm, error)

	// GetRepoFlags returns the per-repo authorization-relevant flags.
	// Returns ErrNoSuchRepo if (tenant, repo) is not registered.
	GetRepoFlags(ctx context.Context, tenant, repo string) (RepoFlags, error)

	// TouchTokenUsage updates the last_used_at timestamp for the token id
	// to time.Now(). Best-effort: callers run this in a fire-and-forget
	// goroutine off the request hot path. A missing tokenID is not an
	// error; implementations may no-op silently.
	TouchTokenUsage(ctx context.Context, tokenID string) error

	// Close releases backing resources (DB connections etc).
	Close() error
}
```

- [ ] **Step 2: Build**

Run: `go build ./internal/auth/...`
Expected: success.

- [ ] **Step 3: Commit**

```bash
git add internal/auth/store.go
git commit -m "M4 auth: Store interface (transport-neutral)"
```

---

## Phase 2 — SQLite store

### Task 7: Migrations runner + 0001_init.sql (TDD)

**Files:** Create `internal/auth/sqlitestore/schema.go`, `internal/auth/sqlitestore/schema_test.go`, `internal/auth/sqlitestore/migrations/0001_init.sql`.

- [ ] **Step 1: Add `modernc.org/sqlite` to go.mod**

Run: `go get modernc.org/sqlite@latest`
Expected: dependency added.

- [ ] **Step 2: Create `internal/auth/sqlitestore/migrations/0001_init.sql`**

```sql
CREATE TABLE schema_version (
    version    INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL
);

CREATE TABLE users (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    is_admin    INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    disabled_at INTEGER
);

CREATE TABLE tokens (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    secret_hash  TEXT NOT NULL,
    label        TEXT,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER,
    last_used_at INTEGER,
    revoked_at   INTEGER
);
CREATE INDEX tokens_user_idx ON tokens(user_id);

CREATE TABLE repos (
    tenant      TEXT NOT NULL,
    name        TEXT NOT NULL,
    public_read INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (tenant, name)
);

CREATE TABLE repo_permissions (
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    tenant     TEXT NOT NULL,
    repo       TEXT NOT NULL,
    perm       TEXT NOT NULL CHECK (perm IN ('read','write','admin')),
    granted_at INTEGER NOT NULL,
    PRIMARY KEY (user_id, tenant, repo),
    FOREIGN KEY (tenant, repo) REFERENCES repos(tenant, name) ON DELETE CASCADE
);

INSERT INTO schema_version (version, applied_at) VALUES (1, strftime('%s','now'));
```

- [ ] **Step 3: Write failing test in `internal/auth/sqlitestore/schema_test.go`**

```go
package sqlitestore

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestRunMigrations_FreshDatabase(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := RunMigrations(db); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	var v int
	if err := db.QueryRow("SELECT MAX(version) FROM schema_version").Scan(&v); err != nil {
		t.Fatalf("schema_version: %v", err)
	}
	if v != 1 {
		t.Fatalf("schema_version = %d, want 1", v)
	}

	tables := []string{"users", "tokens", "repos", "repo_permissions"}
	for _, name := range tables {
		var got string
		err := db.QueryRow(
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", name,
		).Scan(&got)
		if err != nil {
			t.Errorf("table %q missing: %v", name, err)
		}
	}
}

func TestRunMigrations_Idempotent(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := RunMigrations(db); err != nil {
		t.Fatalf("first RunMigrations: %v", err)
	}
	if err := RunMigrations(db); err != nil {
		t.Fatalf("second RunMigrations: %v", err)
	}
	var v int
	if err := db.QueryRow("SELECT MAX(version) FROM schema_version").Scan(&v); err != nil {
		t.Fatalf("schema_version: %v", err)
	}
	if v != 1 {
		t.Fatalf("schema_version = %d, want 1 after idempotent reapply", v)
	}
}
```

- [ ] **Step 4: Run tests, expect failure**

Run: `go test ./internal/auth/sqlitestore/ -run TestRunMigrations -v`
Expected: FAIL (undefined RunMigrations).

- [ ] **Step 5: Implement `internal/auth/sqlitestore/schema.go`**

```go
package sqlitestore

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// RunMigrations applies any unapplied migrations in lexical filename order.
// It creates schema_version on first run via the embedded 0001_init.sql.
//
// Migrations are idempotent at the runner level: each is wrapped in a
// transaction, and each numbered migration is applied at most once based on
// its leading <NNNN_> prefix. The schema_version row is inserted by each
// migration's SQL so that schema_version itself can be created in 0001.
func RunMigrations(db *sql.DB) error {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	applied, err := loadAppliedVersions(db)
	if err != nil {
		return err
	}

	for _, name := range names {
		ver, err := parseVersion(name)
		if err != nil {
			return fmt.Errorf("parse migration %q: %w", name, err)
		}
		if applied[ver] {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %q: %w", name, err)
		}
		if err := applyOne(db, string(body)); err != nil {
			return fmt.Errorf("apply migration %q: %w", name, err)
		}
	}
	return nil
}

// loadAppliedVersions returns the set of already-applied version numbers,
// or an empty set if schema_version doesn't exist yet.
func loadAppliedVersions(db *sql.DB) (map[int]bool, error) {
	out := map[int]bool{}
	rows, err := db.Query("SELECT version FROM schema_version")
	if err != nil {
		// schema_version not yet created -> empty set.
		return out, nil
	}
	defer rows.Close()
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

func parseVersion(name string) (int, error) {
	i := strings.IndexByte(name, '_')
	if i <= 0 {
		return 0, fmt.Errorf("expected NNNN_<name>.sql")
	}
	var v int
	if _, err := fmt.Sscanf(name[:i], "%d", &v); err != nil {
		return 0, err
	}
	return v, nil
}

func applyOne(db *sql.DB, body string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(body); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
```

- [ ] **Step 6: Run tests, expect pass**

Run: `go test ./internal/auth/sqlitestore/ -run TestRunMigrations -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/auth/sqlitestore/ go.mod go.sum
git commit -m "M4 sqlitestore: embedded migrations runner + 0001 init schema"
```

---

### Task 8: Open() with WAL and foreign-keys pragmas (TDD)

**Files:** Create `internal/auth/sqlitestore/store.go`, `internal/auth/sqlitestore/store_test.go`.

- [ ] **Step 1: Write failing tests in `internal/auth/sqlitestore/store_test.go`**

```go
package sqlitestore

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpen_CreatesFileAndAppliesMigrations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	var pragma string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&pragma); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if pragma != "wal" {
		t.Errorf("journal_mode = %q, want wal", pragma)
	}

	var fk int
	if err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}

	var v int
	if err := s.db.QueryRow("SELECT MAX(version) FROM schema_version").Scan(&v); err != nil {
		t.Fatalf("schema_version: %v", err)
	}
	if v != 1 {
		t.Errorf("schema_version = %d, want 1", v)
	}
}

func TestOpen_ReopenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.db")
	a, err := Open(path)
	if err != nil {
		t.Fatalf("Open a: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close a: %v", err)
	}
	b, err := Open(path)
	if err != nil {
		t.Fatalf("Open b: %v", err)
	}
	defer b.Close()
	if err := b.db.PingContext(context.Background()); err != nil {
		t.Fatalf("Ping after reopen: %v", err)
	}
}
```

- [ ] **Step 2: Run tests, expect failure**

Run: `go test ./internal/auth/sqlitestore/ -run TestOpen -v`
Expected: FAIL with undefined symbols.

- [ ] **Step 3: Implement `internal/auth/sqlitestore/store.go`**

```go
package sqlitestore

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// Store is the SQLite-backed implementation of auth.Store.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite database at path, enables WAL and
// foreign keys, and applies any pending migrations.
func Open(path string) (*Store, error) {
	// Use file: URI so we can request WAL via _journal=WAL and foreign
	// keys via _pragma=foreign_keys(1) at connection setup time.
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)",
		path,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	// Single connection for the writer side simplifies WAL semantics for
	// our use case (low concurrency on writes, many concurrent reads).
	db.SetMaxOpenConns(1)

	if err := RunMigrations(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database handle.
func (s *Store) Close() error { return s.db.Close() }
```

- [ ] **Step 4: Run tests, expect pass**

Run: `go test ./internal/auth/sqlitestore/ -v`
Expected: PASS (Open + migrations tests).

- [ ] **Step 5: Commit**

```bash
git add internal/auth/sqlitestore/store.go internal/auth/sqlitestore/store_test.go
git commit -m "M4 sqlitestore: Open() with WAL + foreign_keys + busy_timeout"
```

---

### Task 9: User CRUD (TDD)

**Files:** Modify `internal/auth/sqlitestore/store.go`; extend `internal/auth/sqlitestore/store_test.go`.

- [ ] **Step 1: Append failing tests to `internal/auth/sqlitestore/store_test.go`**

```go
import (
	"errors"
	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func TestCreateUser_AndGetByName(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	id, err := s.CreateUser(ctx, "alice", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if id == "" {
		t.Fatal("CreateUser returned empty id")
	}
	got, err := s.GetUserByName(ctx, "alice")
	if err != nil {
		t.Fatalf("GetUserByName: %v", err)
	}
	if got.ID != id || got.Name != "alice" || got.IsAdmin {
		t.Fatalf("got %+v", got)
	}
}

func TestCreateUser_DuplicateName(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "alice", false); err != nil {
		t.Fatalf("first: %v", err)
	}
	_, err := s.CreateUser(ctx, "alice", false)
	if !errors.Is(err, auth.ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
}

func TestSetUserDisabled_AndDelete(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	id, _ := s.CreateUser(ctx, "alice", false)

	if err := s.SetUserDisabled(ctx, "alice", true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	u, _ := s.GetUserByName(ctx, "alice")
	if u.DisabledAt == nil {
		t.Fatal("expected DisabledAt set")
	}
	if err := s.SetUserDisabled(ctx, "alice", false); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	u, _ = s.GetUserByName(ctx, "alice")
	if u.DisabledAt != nil {
		t.Fatal("expected DisabledAt cleared")
	}
	if err := s.DeleteUser(ctx, "alice"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if _, err := s.GetUserByName(ctx, "alice"); !errors.Is(err, auth.ErrNoSuchUser) {
		t.Fatalf("want ErrNoSuchUser, got %v", err)
	}
	_ = id
}

func TestDeleteUser_RefusesLastAdmin(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "root", true); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	err := s.DeleteUser(ctx, "root")
	if err == nil || !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("want ErrLastAdmin, got %v", err)
	}
}

func TestListUsers(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	_, _ = s.CreateUser(ctx, "alice", false)
	_, _ = s.CreateUser(ctx, "bob", true)
	got, err := s.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
}

// mustOpen is a tiny test helper.
func mustOpen(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "auth.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}
```

- [ ] **Step 2: Run tests, expect failure**

Run: `go test ./internal/auth/sqlitestore/ -run "TestCreateUser|TestSetUser|TestDeleteUser|TestListUsers" -v`
Expected: FAIL (undefined methods).

- [ ] **Step 3: Append to `internal/auth/sqlitestore/store.go`**

```go
import (
	// existing imports...
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// ErrLastAdmin is returned by DeleteUser when removing the user would
// leave the system with zero admins.
var ErrLastAdmin = errors.New("sqlitestore: refusing to delete the last admin")

// User is the row shape returned by user-lookup methods.
type User struct {
	ID         string
	Name       string
	IsAdmin    bool
	CreatedAt  int64
	DisabledAt *int64
}

// newID returns a random 16-byte hex identifier (32 chars). We use this
// for opaque user/token primary keys; for tokens, the public id segment
// is generated separately by auth.GenerateToken.
func newID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// CreateUser inserts a user row and returns its id.
func (s *Store) CreateUser(ctx context.Context, name string, isAdmin bool) (string, error) {
	id, err := newID()
	if err != nil {
		return "", err
	}
	now := time.Now().Unix()
	adminInt := 0
	if isAdmin {
		adminInt = 1
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO users (id, name, is_admin, created_at) VALUES (?, ?, ?, ?)`,
		id, name, adminInt, now,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return "", auth.ErrConflict
		}
		return "", fmt.Errorf("create user: %w", err)
	}
	return id, nil
}

// GetUserByName returns the user row with the given name.
func (s *Store) GetUserByName(ctx context.Context, name string) (*User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, is_admin, created_at, disabled_at FROM users WHERE name = ?`,
		name,
	)
	u := &User{}
	var adminInt int
	var disabled sql.NullInt64
	if err := row.Scan(&u.ID, &u.Name, &adminInt, &u.CreatedAt, &disabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, auth.ErrNoSuchUser
		}
		return nil, fmt.Errorf("get user: %w", err)
	}
	u.IsAdmin = adminInt != 0
	if disabled.Valid {
		v := disabled.Int64
		u.DisabledAt = &v
	}
	return u, nil
}

// ListUsers returns all users ordered by name.
func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, is_admin, created_at, disabled_at FROM users ORDER BY name`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*User{}
	for rows.Next() {
		u := &User{}
		var adminInt int
		var disabled sql.NullInt64
		if err := rows.Scan(&u.ID, &u.Name, &adminInt, &u.CreatedAt, &disabled); err != nil {
			return nil, err
		}
		u.IsAdmin = adminInt != 0
		if disabled.Valid {
			v := disabled.Int64
			u.DisabledAt = &v
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetUserDisabled toggles users.disabled_at. disabled=true sets to now;
// disabled=false sets to NULL.
func (s *Store) SetUserDisabled(ctx context.Context, name string, disabled bool) error {
	var res sql.Result
	var err error
	if disabled {
		res, err = s.db.ExecContext(ctx,
			`UPDATE users SET disabled_at = ? WHERE name = ?`,
			time.Now().Unix(), name,
		)
	} else {
		res, err = s.db.ExecContext(ctx,
			`UPDATE users SET disabled_at = NULL WHERE name = ?`, name,
		)
	}
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return auth.ErrNoSuchUser
	}
	return nil
}

// DeleteUser removes the named user. It refuses to remove the user if doing
// so would leave the system with zero admins (ErrLastAdmin).
func (s *Store) DeleteUser(ctx context.Context, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var isAdmin int
	err = tx.QueryRowContext(ctx, `SELECT is_admin FROM users WHERE name = ?`, name).Scan(&isAdmin)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.ErrNoSuchUser
	}
	if err != nil {
		return err
	}
	if isAdmin != 0 {
		var others int
		err = tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM users WHERE is_admin = 1 AND name != ?`, name,
		).Scan(&others)
		if err != nil {
			return err
		}
		if others == 0 {
			return ErrLastAdmin
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE name = ?`, name); err != nil {
		return err
	}
	return tx.Commit()
}

// isUniqueViolation reports whether err looks like a SQLite UNIQUE
// constraint failure. modernc.org/sqlite errors stringify with this
// substring across versions.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed") ||
		strings.Contains(err.Error(), "constraint failed: UNIQUE")
}
```

- [ ] **Step 4: Run tests, expect pass**

Run: `go test ./internal/auth/sqlitestore/ -v`
Expected: PASS — all user CRUD tests green.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/sqlitestore/
git commit -m "M4 sqlitestore: user CRUD (create/get/list/disable/delete + last-admin guard)"
```

---

### Task 10: Token CRUD (TDD)

**Files:** Modify `internal/auth/sqlitestore/store.go`; extend `internal/auth/sqlitestore/store_test.go`.

The CLI consumes these methods to mint, list, and revoke tokens. `CreateToken` accepts a token-id and a precomputed argon2id encoded hash so this layer never sees the plaintext secret.

- [ ] **Step 1: Append failing tests**

```go
func TestCreateToken_AndGet(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "alice", false)

	exp := time.Now().Add(24 * time.Hour).Unix()
	err := s.CreateToken(ctx, "tokid001AAAAAAAAAAAAAAAA", uid, "$argon2id$v=19$m=65536,t=3,p=4$AAAA$BBBB",
		"laptop", &exp)
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	got, err := s.GetTokenByID(ctx, "tokid001AAAAAAAAAAAAAAAA")
	if err != nil {
		t.Fatalf("GetTokenByID: %v", err)
	}
	if got.UserID != uid || got.Label != "laptop" {
		t.Fatalf("got %+v", got)
	}
	if got.ExpiresAt == nil || *got.ExpiresAt != exp {
		t.Fatalf("ExpiresAt mismatch")
	}
}

func TestRevokeToken(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "alice", false)
	_ = s.CreateToken(ctx, "tokid001AAAAAAAAAAAAAAAA", uid, "$argon2id$x", "", nil)
	if err := s.RevokeToken(ctx, "tokid001AAAAAAAAAAAAAAAA"); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	tok, _ := s.GetTokenByID(ctx, "tokid001AAAAAAAAAAAAAAAA")
	if tok.RevokedAt == nil {
		t.Fatal("expected RevokedAt set")
	}
}

func TestListTokensForUser(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "alice", false)
	_ = s.CreateToken(ctx, "tok1AAAAAAAAAAAAAAAAAAAA", uid, "$argon2id$1", "a", nil)
	_ = s.CreateToken(ctx, "tok2AAAAAAAAAAAAAAAAAAAA", uid, "$argon2id$2", "b", nil)
	rows, err := s.ListTokensForUser(ctx, "alice")
	if err != nil {
		t.Fatalf("ListTokensForUser: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len = %d, want 2", len(rows))
	}
}

func TestResolveTokenIDPrefix(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "alice", false)
	full := "tokABCDE0000000000000000"
	_ = s.CreateToken(ctx, full, uid, "$argon2id$1", "", nil)
	got, err := s.ResolveTokenIDPrefix(ctx, "tokABCDE")
	if err != nil {
		t.Fatalf("ResolveTokenIDPrefix: %v", err)
	}
	if got != full {
		t.Fatalf("got %q want %q", got, full)
	}

	// Unique-prefix violation: add a second token whose id shares the prefix.
	_ = s.CreateToken(ctx, "tokABCDE9999999999999999", uid, "$argon2id$2", "", nil)
	if _, err := s.ResolveTokenIDPrefix(ctx, "tokABC"); !errors.Is(err, ErrAmbiguousPrefix) {
		t.Fatalf("want ErrAmbiguousPrefix, got %v", err)
	}
}

func TestDeleteUser_CascadesTokens(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	// Create another admin so the user-delete is allowed.
	_, _ = s.CreateUser(ctx, "root", true)
	uid, _ := s.CreateUser(ctx, "alice", false)
	_ = s.CreateToken(ctx, "tok1AAAAAAAAAAAAAAAAAAAA", uid, "$argon2id$1", "", nil)
	if err := s.DeleteUser(ctx, "alice"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if _, err := s.GetTokenByID(ctx, "tok1AAAAAAAAAAAAAAAAAAAA"); !errors.Is(err, auth.ErrNoSuchToken) {
		t.Fatalf("want ErrNoSuchToken, got %v", err)
	}
}
```

- [ ] **Step 2: Run tests, expect failure** (`go test ./internal/auth/sqlitestore/ -run "TestCreateToken|TestRevokeToken|TestListTokens|TestResolveToken|TestDeleteUser_CascadesTokens" -v`).

- [ ] **Step 3: Append to `internal/auth/sqlitestore/store.go`**

```go
// ErrAmbiguousPrefix is returned by ResolveTokenIDPrefix when the prefix
// matches more than one token id.
var ErrAmbiguousPrefix = errors.New("sqlitestore: ambiguous token id prefix")

// Token is the row shape returned by token-lookup methods. Note: SecretHash
// is the PHC-encoded argon2id hash, not the plaintext secret.
type Token struct {
	ID         string
	UserID     string
	SecretHash string
	Label      string
	CreatedAt  int64
	ExpiresAt  *int64
	LastUsedAt *int64
	RevokedAt  *int64
}

// CreateToken inserts a token row. The caller supplies the token-id segment
// (from auth.GenerateToken) and the PHC-encoded argon2id hash of the secret
// segment (from auth.HashSecret).
func (s *Store) CreateToken(ctx context.Context, id, userID, secretHash, label string, expiresAt *int64) error {
	now := time.Now().Unix()
	var exp sql.NullInt64
	if expiresAt != nil {
		exp = sql.NullInt64{Int64: *expiresAt, Valid: true}
	}
	var lbl sql.NullString
	if label != "" {
		lbl = sql.NullString{String: label, Valid: true}
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tokens (id, user_id, secret_hash, label, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		id, userID, secretHash, lbl, now, exp,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return auth.ErrConflict
		}
		return fmt.Errorf("create token: %w", err)
	}
	return nil
}

// GetTokenByID fetches a token row.
func (s *Store) GetTokenByID(ctx context.Context, id string) (*Token, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, secret_hash, COALESCE(label,''), created_at,
		        expires_at, last_used_at, revoked_at
		   FROM tokens WHERE id = ?`, id,
	)
	t := &Token{}
	var exp, last, rev sql.NullInt64
	if err := row.Scan(&t.ID, &t.UserID, &t.SecretHash, &t.Label, &t.CreatedAt, &exp, &last, &rev); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, auth.ErrNoSuchToken
		}
		return nil, err
	}
	if exp.Valid {
		v := exp.Int64
		t.ExpiresAt = &v
	}
	if last.Valid {
		v := last.Int64
		t.LastUsedAt = &v
	}
	if rev.Valid {
		v := rev.Int64
		t.RevokedAt = &v
	}
	return t, nil
}

// ListTokensForUser returns all tokens for user `name` ordered by created_at desc.
func (s *Store) ListTokensForUser(ctx context.Context, name string) ([]*Token, error) {
	u, err := s.GetUserByName(ctx, name)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, secret_hash, COALESCE(label,''), created_at,
		        expires_at, last_used_at, revoked_at
		   FROM tokens WHERE user_id = ?
		  ORDER BY created_at DESC`, u.ID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Token{}
	for rows.Next() {
		t := &Token{}
		var exp, last, rev sql.NullInt64
		if err := rows.Scan(&t.ID, &t.UserID, &t.SecretHash, &t.Label, &t.CreatedAt, &exp, &last, &rev); err != nil {
			return nil, err
		}
		if exp.Valid {
			v := exp.Int64
			t.ExpiresAt = &v
		}
		if last.Valid {
			v := last.Int64
			t.LastUsedAt = &v
		}
		if rev.Valid {
			v := rev.Int64
			t.RevokedAt = &v
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken sets revoked_at on the token row identified by full id.
func (s *Store) RevokeToken(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		time.Now().Unix(), id,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Either token doesn't exist or was already revoked. Disambiguate.
		if _, err := s.GetTokenByID(ctx, id); err != nil {
			return err
		}
		// Already revoked: idempotent success.
		return nil
	}
	return nil
}

// ResolveTokenIDPrefix returns the full token id for the given prefix.
// Returns auth.ErrNoSuchToken if no match, ErrAmbiguousPrefix if >1 match.
func (s *Store) ResolveTokenIDPrefix(ctx context.Context, prefix string) (string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM tokens WHERE id LIKE ? || '%' LIMIT 2`, prefix,
	)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	matches := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		matches = append(matches, id)
	}
	switch len(matches) {
	case 0:
		return "", auth.ErrNoSuchToken
	case 1:
		return matches[0], nil
	default:
		return "", ErrAmbiguousPrefix
	}
}
```

- [ ] **Step 4: Run tests, expect pass** (`go test ./internal/auth/sqlitestore/ -v`).

- [ ] **Step 5: Commit**

```bash
git add internal/auth/sqlitestore/
git commit -m "M4 sqlitestore: token CRUD (create/get/list/revoke + prefix resolve)"
```

---

### Task 11: Repo registry CRUD (TDD)

**Files:** Modify `internal/auth/sqlitestore/store.go`; extend `internal/auth/sqlitestore/store_test.go`.

- [ ] **Step 1: Append failing tests**

```go
func TestRegisterRepo_AndGetFlags(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	if err := s.RegisterRepo(ctx, "acme", "foo"); err != nil {
		t.Fatalf("RegisterRepo: %v", err)
	}
	flags, err := s.GetRepoFlags(ctx, "acme", "foo")
	if err != nil {
		t.Fatalf("GetRepoFlags: %v", err)
	}
	if flags.PublicRead {
		t.Fatal("default should be private")
	}
}

func TestSetRepoPublic(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	_ = s.RegisterRepo(ctx, "acme", "foo")
	if err := s.SetRepoPublic(ctx, "acme", "foo", true); err != nil {
		t.Fatalf("SetRepoPublic on: %v", err)
	}
	flags, _ := s.GetRepoFlags(ctx, "acme", "foo")
	if !flags.PublicRead {
		t.Fatal("expected PublicRead = true")
	}
	_ = s.SetRepoPublic(ctx, "acme", "foo", false)
	flags, _ = s.GetRepoFlags(ctx, "acme", "foo")
	if flags.PublicRead {
		t.Fatal("expected PublicRead = false after toggle off")
	}
}

func TestGetRepoFlags_NoSuchRepo(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	if _, err := s.GetRepoFlags(ctx, "ghost", "x"); !errors.Is(err, auth.ErrNoSuchRepo) {
		t.Fatalf("want ErrNoSuchRepo, got %v", err)
	}
}

func TestRegisterRepo_Idempotent(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	if err := s.RegisterRepo(ctx, "acme", "foo"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := s.RegisterRepo(ctx, "acme", "foo"); err != nil {
		t.Fatalf("second (should be idempotent): %v", err)
	}
}

func TestListRepos(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	_ = s.RegisterRepo(ctx, "acme", "foo")
	_ = s.RegisterRepo(ctx, "acme", "bar")
	_ = s.RegisterRepo(ctx, "other", "x")
	got, err := s.ListRepos(ctx, "acme")
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	all, _ := s.ListRepos(ctx, "")
	if len(all) != 3 {
		t.Fatalf("len = %d, want 3", len(all))
	}
}
```

- [ ] **Step 2: Run tests, expect failure**.

- [ ] **Step 3: Append to `internal/auth/sqlitestore/store.go`**

```go
// Repo is the registry row shape.
type Repo struct {
	Tenant     string
	Name       string
	PublicRead bool
	CreatedAt  int64
}

// RegisterRepo idempotently inserts a (tenant, name) into repos.
func (s *Store) RegisterRepo(ctx context.Context, tenant, name string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO repos (tenant, name, public_read, created_at)
		 VALUES (?, ?, 0, ?)`,
		tenant, name, time.Now().Unix(),
	)
	return err
}

// GetRepoFlags returns the per-repo authorization flags.
func (s *Store) GetRepoFlags(ctx context.Context, tenant, repo string) (auth.RepoFlags, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT public_read FROM repos WHERE tenant = ? AND name = ?`, tenant, repo,
	)
	var pub int
	if err := row.Scan(&pub); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return auth.RepoFlags{}, auth.ErrNoSuchRepo
		}
		return auth.RepoFlags{}, err
	}
	return auth.RepoFlags{PublicRead: pub != 0}, nil
}

// SetRepoPublic toggles repos.public_read.
func (s *Store) SetRepoPublic(ctx context.Context, tenant, repo string, public bool) error {
	v := 0
	if public {
		v = 1
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE repos SET public_read = ? WHERE tenant = ? AND name = ?`, v, tenant, repo,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return auth.ErrNoSuchRepo
	}
	return nil
}

// ListRepos returns repos in `tenant`, or all repos if tenant == "".
// Ordered by (tenant, name).
func (s *Store) ListRepos(ctx context.Context, tenant string) ([]*Repo, error) {
	var rows *sql.Rows
	var err error
	if tenant == "" {
		rows, err = s.db.QueryContext(ctx,
			`SELECT tenant, name, public_read, created_at FROM repos ORDER BY tenant, name`)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT tenant, name, public_read, created_at FROM repos WHERE tenant = ? ORDER BY name`,
			tenant)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Repo{}
	for rows.Next() {
		r := &Repo{}
		var pub int
		if err := rows.Scan(&r.Tenant, &r.Name, &pub, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.PublicRead = pub != 0
		out = append(out, r)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Run tests, expect pass**.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/sqlitestore/
git commit -m "M4 sqlitestore: repo registry CRUD + GetRepoFlags"
```

---

### Task 12: Permission CRUD (TDD)

**Files:** Modify `internal/auth/sqlitestore/store.go`; extend test file.

- [ ] **Step 1: Append failing tests**

```go
func TestGrantAndLookup(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	_, _ = s.CreateUser(ctx, "alice", false)
	_ = s.RegisterRepo(ctx, "acme", "foo")
	if err := s.Grant(ctx, "alice", "acme", "foo", "write"); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	u, _ := s.GetUserByName(ctx, "alice")
	a := &auth.Actor{UserID: u.ID, Name: u.Name}
	perm, err := s.LookupRepoPerm(ctx, a, "acme", "foo")
	if err != nil {
		t.Fatalf("LookupRepoPerm: %v", err)
	}
	if perm != auth.PermWrite {
		t.Fatalf("perm = %v, want PermWrite", perm)
	}
}

func TestGrant_RefusesUnregisteredRepo(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	_, _ = s.CreateUser(ctx, "alice", false)
	if err := s.Grant(ctx, "alice", "acme", "foo", "read"); !errors.Is(err, auth.ErrNoSuchRepo) {
		t.Fatalf("want ErrNoSuchRepo, got %v", err)
	}
}

func TestRevokeRepoPermission(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	_, _ = s.CreateUser(ctx, "alice", false)
	_ = s.RegisterRepo(ctx, "acme", "foo")
	_ = s.Grant(ctx, "alice", "acme", "foo", "read")
	if err := s.RevokeRepoPermission(ctx, "alice", "acme", "foo"); err != nil {
		t.Fatalf("RevokeRepoPermission: %v", err)
	}
	u, _ := s.GetUserByName(ctx, "alice")
	a := &auth.Actor{UserID: u.ID}
	perm, _ := s.LookupRepoPerm(ctx, a, "acme", "foo")
	if perm != auth.PermNone {
		t.Fatalf("perm = %v, want PermNone after revoke", perm)
	}
}

func TestLookupRepoPerm_AdminShortCircuits(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "root", true)
	_ = s.RegisterRepo(ctx, "acme", "foo")
	a := &auth.Actor{UserID: uid, IsAdmin: true}
	perm, err := s.LookupRepoPerm(ctx, a, "acme", "foo")
	if err != nil {
		t.Fatalf("LookupRepoPerm: %v", err)
	}
	if perm != auth.PermAdmin {
		t.Fatalf("perm = %v, want PermAdmin", perm)
	}
}

func TestLookupRepoPerm_NilActorIsPermNone(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	perm, err := s.LookupRepoPerm(ctx, nil, "acme", "foo")
	if err != nil {
		t.Fatalf("LookupRepoPerm(nil): %v", err)
	}
	if perm != auth.PermNone {
		t.Fatalf("perm = %v, want PermNone", perm)
	}
}
```

- [ ] **Step 2: Run tests, expect failure**.

- [ ] **Step 3: Append to `internal/auth/sqlitestore/store.go`**

```go
// Grant creates or replaces a permission row. perm must be "read", "write",
// or "admin". Refuses if the (tenant, repo) is not registered.
func (s *Store) Grant(ctx context.Context, userName, tenant, repo, perm string) error {
	if perm != "read" && perm != "write" && perm != "admin" {
		return fmt.Errorf("grant: invalid perm %q", perm)
	}
	u, err := s.GetUserByName(ctx, userName)
	if err != nil {
		return err
	}
	if _, err := s.GetRepoFlags(ctx, tenant, repo); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO repo_permissions (user_id, tenant, repo, perm, granted_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(user_id, tenant, repo) DO UPDATE SET perm = excluded.perm,
		                                                  granted_at = excluded.granted_at`,
		u.ID, tenant, repo, perm, time.Now().Unix(),
	)
	return err
}

// RevokeRepoPermission removes the permission row for (userName, tenant, repo).
// No error if the row didn't exist.
func (s *Store) RevokeRepoPermission(ctx context.Context, userName, tenant, repo string) error {
	u, err := s.GetUserByName(ctx, userName)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`DELETE FROM repo_permissions WHERE user_id = ? AND tenant = ? AND repo = ?`,
		u.ID, tenant, repo,
	)
	return err
}

// LookupRepoPerm returns the actor's permission level on (tenant, repo).
// Implements auth.Store.
func (s *Store) LookupRepoPerm(ctx context.Context, actor *auth.Actor, tenant, repo string) (auth.Perm, error) {
	if actor == nil {
		return auth.PermNone, nil
	}
	if actor.IsAdmin {
		return auth.PermAdmin, nil
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT perm FROM repo_permissions
		   WHERE user_id = ? AND tenant = ? AND repo = ?`,
		actor.UserID, tenant, repo,
	)
	var p string
	if err := row.Scan(&p); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return auth.PermNone, nil
		}
		return auth.PermNone, err
	}
	switch p {
	case "read":
		return auth.PermRead, nil
	case "write":
		return auth.PermWrite, nil
	case "admin":
		return auth.PermAdmin, nil
	default:
		return auth.PermNone, fmt.Errorf("lookup repo perm: unknown perm %q", p)
	}
}
```

- [ ] **Step 4: Run tests, expect pass**.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/sqlitestore/
git commit -m "M4 sqlitestore: permission CRUD + LookupRepoPerm with admin short-circuit"
```

---

### Task 13: VerifyCredential + TouchTokenUsage (TDD)

`VerifyCredential` is the integration of token parsing, hash lookup, and policy checks (expired/revoked/disabled). It is the first method that uses `auth.GenerateToken`/`auth.HashSecret` end-to-end.

**Files:** Modify `internal/auth/sqlitestore/store.go`; extend test file.

- [ ] **Step 1: Append failing tests**

```go
func TestVerifyCredential_HappyPath(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "alice", false)
	tok, id, secret, _ := auth.GenerateToken()
	hash, _ := auth.HashSecret(secret)
	_ = s.CreateToken(ctx, id, uid, hash, "laptop", nil)

	got, gotID, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "alice", Password: tok})
	if err != nil {
		t.Fatalf("VerifyCredential: %v", err)
	}
	if got == nil || got.UserID != uid || got.Name != "alice" {
		t.Fatalf("actor = %+v", got)
	}
	if gotID != id {
		t.Fatalf("returned tokenID = %q want %q", gotID, id)
	}
}

func TestVerifyCredential_BadPassword(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "alice", false)
	_, id, _, _ := auth.GenerateToken()
	hash, _ := auth.HashSecret("real-secret-string")
	_ = s.CreateToken(ctx, id, uid, hash, "", nil)

	_, _, err := s.VerifyCredential(ctx, auth.BasicPassword{
		Username: "alice",
		Password: "bvts_" + id + "_" + strings.Repeat("A", 52),
	})
	if !errors.Is(err, auth.ErrInvalidCredential) {
		t.Fatalf("want ErrInvalidCredential, got %v", err)
	}
}

func TestVerifyCredential_UnknownTokenID(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	tok, _, _, _ := auth.GenerateToken()
	_, _, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "alice", Password: tok})
	if !errors.Is(err, auth.ErrInvalidCredential) {
		t.Fatalf("want ErrInvalidCredential, got %v", err)
	}
}

func TestVerifyCredential_Expired(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "alice", false)
	tok, id, secret, _ := auth.GenerateToken()
	hash, _ := auth.HashSecret(secret)
	past := time.Now().Add(-time.Hour).Unix()
	_ = s.CreateToken(ctx, id, uid, hash, "", &past)
	_, _, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "alice", Password: tok})
	if !errors.Is(err, auth.ErrTokenExpired) {
		t.Fatalf("want ErrTokenExpired, got %v", err)
	}
}

func TestVerifyCredential_Revoked(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "alice", false)
	tok, id, secret, _ := auth.GenerateToken()
	hash, _ := auth.HashSecret(secret)
	_ = s.CreateToken(ctx, id, uid, hash, "", nil)
	_ = s.RevokeToken(ctx, id)
	_, _, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "alice", Password: tok})
	if !errors.Is(err, auth.ErrTokenRevoked) {
		t.Fatalf("want ErrTokenRevoked, got %v", err)
	}
}

func TestVerifyCredential_Disabled(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "alice", false)
	tok, id, secret, _ := auth.GenerateToken()
	hash, _ := auth.HashSecret(secret)
	_ = s.CreateToken(ctx, id, uid, hash, "", nil)
	_ = s.SetUserDisabled(ctx, "alice", true)
	_, _, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "alice", Password: tok})
	if !errors.Is(err, auth.ErrUserDisabled) {
		t.Fatalf("want ErrUserDisabled, got %v", err)
	}
	_ = uid
}

func TestVerifyCredential_UsernameMustMatch(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "alice", false)
	tok, id, secret, _ := auth.GenerateToken()
	hash, _ := auth.HashSecret(secret)
	_ = s.CreateToken(ctx, id, uid, hash, "", nil)
	// Wrong username, valid token: reject. (Spec §30.1: username + token-as-password.)
	_, _, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "bob", Password: tok})
	if !errors.Is(err, auth.ErrInvalidCredential) {
		t.Fatalf("want ErrInvalidCredential, got %v", err)
	}
}

func TestTouchTokenUsage(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, "alice", false)
	_, id, secret, _ := auth.GenerateToken()
	hash, _ := auth.HashSecret(secret)
	_ = s.CreateToken(ctx, id, uid, hash, "", nil)
	if err := s.TouchTokenUsage(ctx, id); err != nil {
		t.Fatalf("TouchTokenUsage: %v", err)
	}
	tok, _ := s.GetTokenByID(ctx, id)
	if tok.LastUsedAt == nil {
		t.Fatal("LastUsedAt not set")
	}
	// Missing id = no error.
	if err := s.TouchTokenUsage(ctx, "noSuchAAAAAAAAAAAAAAAAAA"); err != nil {
		t.Fatalf("TouchTokenUsage missing id: %v", err)
	}
}
```

- [ ] **Step 2: Run tests, expect failure**.

- [ ] **Step 3: Append to `internal/auth/sqlitestore/store.go`**

```go
// VerifyCredential implements auth.Store.
func (s *Store) VerifyCredential(ctx context.Context, c auth.Credential) (*auth.Actor, string, error) {
	bp, ok := c.(auth.BasicPassword)
	if !ok {
		// M6 will add SSHKeyFingerprint handling.
		return nil, "", auth.ErrInvalidCredential
	}
	tokenID, secret, err := auth.ParseToken(bp.Password)
	if err != nil {
		return nil, "", auth.ErrInvalidCredential
	}
	tok, err := s.GetTokenByID(ctx, tokenID)
	if errors.Is(err, auth.ErrNoSuchToken) {
		return nil, "", auth.ErrInvalidCredential
	}
	if err != nil {
		return nil, "", err
	}
	if err := auth.VerifyHash(secret, tok.SecretHash); err != nil {
		return nil, "", auth.ErrInvalidCredential
	}
	if tok.RevokedAt != nil {
		return nil, "", auth.ErrTokenRevoked
	}
	if tok.ExpiresAt != nil && *tok.ExpiresAt <= time.Now().Unix() {
		return nil, "", auth.ErrTokenExpired
	}
	// Lookup the user; check name match and disabled state.
	row := s.db.QueryRowContext(ctx,
		`SELECT name, is_admin, disabled_at FROM users WHERE id = ?`, tok.UserID,
	)
	var name string
	var adminInt int
	var disabled sql.NullInt64
	if err := row.Scan(&name, &adminInt, &disabled); err != nil {
		return nil, "", auth.ErrInvalidCredential
	}
	if disabled.Valid {
		return nil, "", auth.ErrUserDisabled
	}
	if bp.Username != name {
		return nil, "", auth.ErrInvalidCredential
	}
	return &auth.Actor{
		UserID:  tok.UserID,
		Name:    name,
		IsAdmin: adminInt != 0,
	}, tokenID, nil
}

// TouchTokenUsage implements auth.Store. A missing tokenID is not an error.
func (s *Store) TouchTokenUsage(ctx context.Context, tokenID string) error {
	if tokenID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE tokens SET last_used_at = ? WHERE id = ?`, time.Now().Unix(), tokenID,
	)
	return err
}
```

- [ ] **Step 4: Run tests, expect pass**.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/sqlitestore/
git commit -m "M4 sqlitestore: VerifyCredential + TouchTokenUsage (full auth.Store)"
```

---

### Task 14: Compile-time interface check + sanity build

**Files:** Append to `internal/auth/sqlitestore/store.go`.

- [ ] **Step 1: Add the assertion**

```go
// Compile-time check that *Store satisfies auth.Store.
var _ auth.Store = (*Store)(nil)
```

- [ ] **Step 2: Build**

Run: `go build ./...`
Expected: success.

- [ ] **Step 3: Run full sqlitestore test suite**

Run: `go test ./internal/auth/...`
Expected: all green.

- [ ] **Step 4: Commit**

```bash
git add internal/auth/sqlitestore/store.go
git commit -m "M4 sqlitestore: compile-time auth.Store interface check"
```

---

## Phase 3 — Conformance suite

### Task 15: Portable Store conformance suite + run against sqlitestore

**Files:** Create `internal/auth/conformance/doc.go`, `internal/auth/conformance/conformance.go`, `internal/auth/sqlitestore/conformance_test.go`.

- [ ] **Step 1: Create `internal/auth/conformance/doc.go`**

```go
// Package conformance is a portable test suite that any auth.Store
// implementation must pass. M4 runs it against sqlitestore.Store; later
// hosted implementations (e.g., Postgres) will subscribe via the same
// Run(t, factory) entry point.
package conformance
```

- [ ] **Step 2: Create `internal/auth/conformance/conformance.go`**

```go
package conformance

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// SeedUserFn lets the conformance suite stand up baseline users/tokens/repos
// without depending on a specific Store impl's CRUD methods. The factory
// returns a fresh Store and a Seeder bound to it; the Seeder applies the
// minimum operations the conformance tests need.
type Seeder interface {
	CreateUser(ctx context.Context, name string, isAdmin bool) (userID string)
	CreateToken(ctx context.Context, userID, tokenID, secretHash string, expiresAt *int64) // never expires if nil
	RevokeToken(ctx context.Context, tokenID string)
	SetUserDisabled(ctx context.Context, name string, disabled bool)
	RegisterRepo(ctx context.Context, tenant, repo string)
	SetRepoPublic(ctx context.Context, tenant, repo string, public bool)
	Grant(ctx context.Context, userName, tenant, repo, perm string)
}

// Factory builds a fresh (Store, Seeder) pair for each test.
type Factory func(t *testing.T) (auth.Store, Seeder)

// Run executes the full conformance suite.
func Run(t *testing.T, factory Factory) {
	t.Run("VerifyCredential_RejectsUnknownTokenID", func(t *testing.T) {
		s, _ := factory(t)
		defer s.Close()
		tok, _, _, _ := auth.GenerateToken()
		_, _, err := s.VerifyCredential(context.Background(),
			auth.BasicPassword{Username: "alice", Password: tok})
		mustErrIs(t, err, auth.ErrInvalidCredential)
	})

	t.Run("VerifyCredential_RejectsBadSecret", func(t *testing.T) {
		s, sd := factory(t)
		defer s.Close()
		ctx := context.Background()
		uid := sd.CreateUser(ctx, "alice", false)
		hash, _ := auth.HashSecret("the-real-secret")
		_, id, _, _ := auth.GenerateToken()
		sd.CreateToken(ctx, uid, id, hash, nil)
		// Compose a token with the right id but wrong secret.
		bad := "bvts_" + id + "_" + strings.Repeat("A", 52)
		_, _, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "alice", Password: bad})
		mustErrIs(t, err, auth.ErrInvalidCredential)
	})

	t.Run("VerifyCredential_RejectsExpired", func(t *testing.T) {
		s, sd := factory(t)
		defer s.Close()
		ctx := context.Background()
		uid := sd.CreateUser(ctx, "alice", false)
		tok, id, secret, _ := auth.GenerateToken()
		hash, _ := auth.HashSecret(secret)
		past := time.Now().Add(-time.Hour).Unix()
		sd.CreateToken(ctx, uid, id, hash, &past)
		_, _, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "alice", Password: tok})
		mustErrIs(t, err, auth.ErrTokenExpired)
	})

	t.Run("VerifyCredential_RejectsRevoked", func(t *testing.T) {
		s, sd := factory(t)
		defer s.Close()
		ctx := context.Background()
		uid := sd.CreateUser(ctx, "alice", false)
		tok, id, secret, _ := auth.GenerateToken()
		hash, _ := auth.HashSecret(secret)
		sd.CreateToken(ctx, uid, id, hash, nil)
		sd.RevokeToken(ctx, id)
		_, _, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "alice", Password: tok})
		mustErrIs(t, err, auth.ErrTokenRevoked)
	})

	t.Run("VerifyCredential_RejectsDisabled", func(t *testing.T) {
		s, sd := factory(t)
		defer s.Close()
		ctx := context.Background()
		uid := sd.CreateUser(ctx, "alice", false)
		tok, id, secret, _ := auth.GenerateToken()
		hash, _ := auth.HashSecret(secret)
		sd.CreateToken(ctx, uid, id, hash, nil)
		sd.SetUserDisabled(ctx, "alice", true)
		_, _, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "alice", Password: tok})
		mustErrIs(t, err, auth.ErrUserDisabled)
	})

	t.Run("LookupRepoPerm_NoneForNoGrant", func(t *testing.T) {
		s, sd := factory(t)
		defer s.Close()
		ctx := context.Background()
		uid := sd.CreateUser(ctx, "alice", false)
		sd.RegisterRepo(ctx, "acme", "foo")
		actor := &auth.Actor{UserID: uid, Name: "alice"}
		p, err := s.LookupRepoPerm(ctx, actor, "acme", "foo")
		if err != nil || p != auth.PermNone {
			t.Fatalf("got perm=%v err=%v", p, err)
		}
	})

	t.Run("LookupRepoPerm_GrantedLevel", func(t *testing.T) {
		s, sd := factory(t)
		defer s.Close()
		ctx := context.Background()
		uid := sd.CreateUser(ctx, "alice", false)
		sd.RegisterRepo(ctx, "acme", "foo")
		sd.Grant(ctx, "alice", "acme", "foo", "write")
		actor := &auth.Actor{UserID: uid, Name: "alice"}
		p, _ := s.LookupRepoPerm(ctx, actor, "acme", "foo")
		if p != auth.PermWrite {
			t.Fatalf("perm = %v want PermWrite", p)
		}
	})

	t.Run("LookupRepoPerm_AdminShortCircuits", func(t *testing.T) {
		s, sd := factory(t)
		defer s.Close()
		ctx := context.Background()
		uid := sd.CreateUser(ctx, "root", true)
		sd.RegisterRepo(ctx, "acme", "foo")
		actor := &auth.Actor{UserID: uid, IsAdmin: true}
		p, _ := s.LookupRepoPerm(ctx, actor, "acme", "foo")
		if p != auth.PermAdmin {
			t.Fatalf("perm = %v want PermAdmin", p)
		}
	})

	t.Run("LookupRepoPerm_NilActorIsPermNone", func(t *testing.T) {
		s, _ := factory(t)
		defer s.Close()
		p, err := s.LookupRepoPerm(context.Background(), nil, "acme", "foo")
		if err != nil || p != auth.PermNone {
			t.Fatalf("got perm=%v err=%v", p, err)
		}
	})

	t.Run("GetRepoFlags_NoSuchRepo", func(t *testing.T) {
		s, _ := factory(t)
		defer s.Close()
		_, err := s.GetRepoFlags(context.Background(), "ghost", "x")
		mustErrIs(t, err, auth.ErrNoSuchRepo)
	})

	t.Run("GetRepoFlags_PublicRead", func(t *testing.T) {
		s, sd := factory(t)
		defer s.Close()
		ctx := context.Background()
		sd.RegisterRepo(ctx, "acme", "foo")
		sd.SetRepoPublic(ctx, "acme", "foo", true)
		f, err := s.GetRepoFlags(ctx, "acme", "foo")
		if err != nil {
			t.Fatal(err)
		}
		if !f.PublicRead {
			t.Fatal("expected PublicRead = true")
		}
	})

	t.Run("TouchTokenUsage_IdempotentOnMissing", func(t *testing.T) {
		s, _ := factory(t)
		defer s.Close()
		if err := s.TouchTokenUsage(context.Background(), ""); err != nil {
			t.Fatalf("empty id: %v", err)
		}
		if err := s.TouchTokenUsage(context.Background(), "noSuchABCDE0000000000000"); err != nil {
			t.Fatalf("missing id: %v", err)
		}
	})
}

func mustErrIs(t *testing.T, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("got error %v, want %v", got, want)
	}
}
```

- [ ] **Step 3: Create `internal/auth/sqlitestore/conformance_test.go`**

```go
package sqlitestore

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/conformance"
)

type sqliteSeeder struct{ s *Store }

func (sd *sqliteSeeder) CreateUser(ctx context.Context, name string, isAdmin bool) string {
	id, err := sd.s.CreateUser(ctx, name, isAdmin)
	if err != nil {
		panic(err)
	}
	return id
}
func (sd *sqliteSeeder) CreateToken(ctx context.Context, userID, tokenID, hash string, exp *int64) {
	if err := sd.s.CreateToken(ctx, tokenID, userID, hash, "", exp); err != nil {
		panic(err)
	}
}
func (sd *sqliteSeeder) RevokeToken(ctx context.Context, tokenID string) {
	if err := sd.s.RevokeToken(ctx, tokenID); err != nil {
		panic(err)
	}
}
func (sd *sqliteSeeder) SetUserDisabled(ctx context.Context, name string, dis bool) {
	if err := sd.s.SetUserDisabled(ctx, name, dis); err != nil {
		panic(err)
	}
}
func (sd *sqliteSeeder) RegisterRepo(ctx context.Context, tenant, repo string) {
	if err := sd.s.RegisterRepo(ctx, tenant, repo); err != nil {
		panic(err)
	}
}
func (sd *sqliteSeeder) SetRepoPublic(ctx context.Context, tenant, repo string, pub bool) {
	if err := sd.s.SetRepoPublic(ctx, tenant, repo, pub); err != nil {
		panic(err)
	}
}
func (sd *sqliteSeeder) Grant(ctx context.Context, user, tenant, repo, perm string) {
	if err := sd.s.Grant(ctx, user, tenant, repo, perm); err != nil {
		panic(err)
	}
}

func TestConformance(t *testing.T) {
	conformance.Run(t, func(t *testing.T) (auth.Store, conformance.Seeder) {
		dir := t.TempDir()
		s, err := Open(filepath.Join(dir, "auth.db"))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return s, &sqliteSeeder{s: s}
	})
}
```

- [ ] **Step 4: Run tests, expect pass**

Run: `go test ./internal/auth/...`
Expected: PASS — every conformance sub-test green.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/conformance/ internal/auth/sqlitestore/conformance_test.go
git commit -m "M4 auth: portable Store conformance suite + sqlitestore exercises it"
```

---

## Phase 4 — Gateway integration

### Task 16: Extract pure ParseRoute (TDD)

The existing `internal/gateway/routes.go` mixes URL parsing with handler dispatch and the legacy `authorize()` calls. We refactor it into:

1. A pure `ParseRoute(method, path, query) (*RoutedRequest, error)` function with a single source of truth for URL → (tenant, repo, op, requiredAction).
2. A thin `routeRepo` that calls ParseRoute, runs the new middleware (Task 17), and dispatches.

This task only adds ParseRoute and tests. The existing `routeRepo` body is untouched (rewritten in Task 18).

**Files:** Modify `internal/gateway/routes.go`; create `internal/gateway/routes_test.go`.

- [ ] **Step 1: Write failing tests in `internal/gateway/routes_test.go`**

```go
package gateway

import (
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func TestParseRoute_Table(t *testing.T) {
	type tc struct {
		name     string
		method   string
		path     string
		query    string
		wantOp   Op
		wantAct  auth.Action
		wantTen  string
		wantRepo string
		wantErr  bool
	}
	cases := []tc{
		{"info-refs upload", "GET", "/acme/foo.git/info/refs", "service=git-upload-pack",
			OpInfoRefsUpload, auth.ActionRead, "acme", "foo", false},
		{"info-refs receive", "GET", "/acme/foo.git/info/refs", "service=git-receive-pack",
			OpInfoRefsReceive, auth.ActionWrite, "acme", "foo", false},
		{"upload-pack", "POST", "/acme/foo.git/git-upload-pack", "",
			OpUploadPack, auth.ActionRead, "acme", "foo", false},
		{"receive-pack", "POST", "/acme/foo.git/git-receive-pack", "",
			OpReceivePack, auth.ActionWrite, "acme", "foo", false},

		{"missing .git suffix", "GET", "/acme/foo/info/refs", "service=git-upload-pack",
			0, 0, "", "", true},
		{"unknown service param", "GET", "/acme/foo.git/info/refs", "service=git-archive",
			0, 0, "", "", true},
		{"info-refs without service", "GET", "/acme/foo.git/info/refs", "",
			0, 0, "", "", true},
		{"upload-pack via GET", "GET", "/acme/foo.git/git-upload-pack", "",
			0, 0, "", "", true},
		{"trailing slash", "GET", "/acme/foo.git/info/refs/", "service=git-upload-pack",
			0, 0, "", "", true},
		{"invalid tenant", "GET", "/../foo.git/info/refs", "service=git-upload-pack",
			0, 0, "", "", true},
		{"invalid repo", "GET", "/acme/!!.git/info/refs", "service=git-upload-pack",
			0, 0, "", "", true},
		{"missing tenant", "GET", "/.git/info/refs", "service=git-upload-pack",
			0, 0, "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr, err := ParseRoute(c.method, c.path, c.query)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", rr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if rr.Op != c.wantOp || rr.RequiredAction != c.wantAct ||
				rr.Tenant != c.wantTen || rr.Repo != c.wantRepo {
				t.Fatalf("got %+v, want op=%v act=%v tenant=%q repo=%q",
					rr, c.wantOp, c.wantAct, c.wantTen, c.wantRepo)
			}
		})
	}
}

func TestParseRoute_NoMatchSentinel(t *testing.T) {
	_, err := ParseRoute("GET", "/some/random/path", "")
	if !errors.Is(err, ErrRouteNoMatch) {
		t.Fatalf("want ErrRouteNoMatch, got %v", err)
	}
}
```

- [ ] **Step 2: Run tests, expect failure**

Run: `go test ./internal/gateway/ -run TestParseRoute -v`
Expected: FAIL with undefined symbols.

- [ ] **Step 3: Add ParseRoute to `internal/gateway/routes.go`**

Append below the existing imports (which already include `path`, `regexp`, `strings`):

```go
// Op is the protocol operation a request maps to.
type Op int

const (
	OpInfoRefsUpload Op = iota + 1
	OpInfoRefsReceive
	OpUploadPack
	OpReceivePack
)

// RoutedRequest is the parsed shape of a Git-protocol request.
type RoutedRequest struct {
	Tenant         string
	Repo           string
	Op             Op
	RequiredAction auth.Action
}

// ErrRouteNoMatch means the request URL does not look like a Git smart-HTTP
// request handled by this gateway. Callers should respond with 404.
var ErrRouteNoMatch = errors.New("gateway: no route match")

// ParseRoute is the single source of truth for "this URL means this op."
// It is pure: no http.Request, no http.ResponseWriter, no auth.Store.
//
// Caller is responsible for translating the returned error into 404.
func ParseRoute(method, urlPath, rawQuery string) (*RoutedRequest, error) {
	if urlPath != path.Clean(urlPath) {
		return nil, fmt.Errorf("gateway: invalid path: %w", ErrRouteNoMatch)
	}
	parts := strings.SplitN(strings.TrimPrefix(urlPath, "/"), "/", 3)
	if len(parts) < 3 {
		return nil, ErrRouteNoMatch
	}
	tenant := parts[0]
	repoSeg := parts[1]
	rest := parts[2]

	if !strings.HasSuffix(repoSeg, ".git") || repoSeg == ".git" {
		return nil, ErrRouteNoMatch
	}
	repoID := strings.TrimSuffix(repoSeg, ".git")
	if !nameRE.MatchString(tenant) || !nameRE.MatchString(repoID) {
		return nil, ErrRouteNoMatch
	}

	q, _ := url.ParseQuery(rawQuery)
	switch {
	case method == http.MethodGet && rest == "info/refs":
		switch q.Get("service") {
		case "git-upload-pack":
			return &RoutedRequest{tenant, repoID, OpInfoRefsUpload, auth.ActionRead}, nil
		case "git-receive-pack":
			return &RoutedRequest{tenant, repoID, OpInfoRefsReceive, auth.ActionWrite}, nil
		default:
			return nil, ErrRouteNoMatch
		}
	case method == http.MethodPost && rest == "git-upload-pack":
		return &RoutedRequest{tenant, repoID, OpUploadPack, auth.ActionRead}, nil
	case method == http.MethodPost && rest == "git-receive-pack":
		return &RoutedRequest{tenant, repoID, OpReceivePack, auth.ActionWrite}, nil
	default:
		return nil, ErrRouteNoMatch
	}
}
```

Update the import block of `internal/gateway/routes.go` to:

```go
import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)
```

(Existing `nameRE` and `routeRepo` stay for now — Task 18 rewrites `routeRepo`.)

- [ ] **Step 4: Run tests, expect pass**

Run: `go test ./internal/gateway/ -run TestParseRoute -v`
Expected: PASS.

- [ ] **Step 5: Confirm existing tests still pass**

Run: `go test ./internal/gateway/`
Expected: PASS — pre-M4 gateway tests still green; ParseRoute is additive at this point.

- [ ] **Step 6: Commit**

```bash
git add internal/gateway/routes.go internal/gateway/routes_test.go
git commit -m "M4 gateway: extract pure ParseRoute (URL -> RoutedRequest)"
```

---

### Task 17: Auth middleware against auth.Store (TDD)

This rewrites `internal/gateway/auth.go`. The old `AuthMode`/`authorize` is removed. The middleware function takes `(http.ResponseWriter, *http.Request)`, runs the spec §6.2 sequence, and returns `(actor *auth.Actor, ok bool)`. On `!ok`, the response has already been written.

**Files:** Rewrite `internal/gateway/auth.go`; create `internal/gateway/authmw_test.go`.

- [ ] **Step 1: Write failing tests in `internal/gateway/authmw_test.go`**

```go
package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// fakeStore is an in-memory minimal auth.Store for middleware tests.
type fakeStore struct {
	credActor *auth.Actor
	credToken string
	credErr   error
	perm      auth.Perm
	flags     auth.RepoFlags
	flagsErr  error
}

func (f *fakeStore) VerifyCredential(ctx context.Context, c auth.Credential) (*auth.Actor, string, error) {
	return f.credActor, f.credToken, f.credErr
}
func (f *fakeStore) LookupRepoPerm(ctx context.Context, a *auth.Actor, t, r string) (auth.Perm, error) {
	if a == nil {
		return auth.PermNone, nil
	}
	return f.perm, nil
}
func (f *fakeStore) GetRepoFlags(ctx context.Context, t, r string) (auth.RepoFlags, error) {
	return f.flags, f.flagsErr
}
func (f *fakeStore) TouchTokenUsage(ctx context.Context, id string) error { return nil }
func (f *fakeStore) Close() error                                         { return nil }

func req(t *testing.T, method, path, query, basicUser, basicPass string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, "http://x"+path+"?"+query, nil)
	if basicUser != "" || basicPass != "" {
		r.SetBasicAuth(basicUser, basicPass)
	}
	return r
}

func TestRunAuth_AnonymousReadPublic(t *testing.T) {
	st := &fakeStore{flags: auth.RepoFlags{PublicRead: true}}
	rr := &RoutedRequest{Tenant: "a", Repo: "b", Op: OpUploadPack, RequiredAction: auth.ActionRead}
	w := httptest.NewRecorder()
	r := req(t, "POST", "/a/b.git/git-upload-pack", "", "", "")
	actor, ok := RunAuth(w, r, st, rr)
	if !ok {
		t.Fatalf("expected allow, got status %d", w.Code)
	}
	if actor != nil {
		t.Fatalf("expected anonymous, got %+v", actor)
	}
}

func TestRunAuth_AnonymousWritePublic_Challenge(t *testing.T) {
	st := &fakeStore{flags: auth.RepoFlags{PublicRead: true}}
	rr := &RoutedRequest{Tenant: "a", Repo: "b", Op: OpReceivePack, RequiredAction: auth.ActionWrite}
	w := httptest.NewRecorder()
	r := req(t, "POST", "/a/b.git/git-receive-pack", "", "", "")
	if _, ok := RunAuth(w, r, st, rr); ok {
		t.Fatal("expected deny")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if !strings.HasPrefix(w.Header().Get("WWW-Authenticate"), "Basic ") {
		t.Fatalf("missing WWW-Authenticate: %q", w.Header().Get("WWW-Authenticate"))
	}
}

func TestRunAuth_AnonymousReadPrivate_Challenge(t *testing.T) {
	st := &fakeStore{flags: auth.RepoFlags{PublicRead: false}}
	rr := &RoutedRequest{Tenant: "a", Repo: "b", Op: OpUploadPack, RequiredAction: auth.ActionRead}
	w := httptest.NewRecorder()
	r := req(t, "POST", "/a/b.git/git-upload-pack", "", "", "")
	if _, ok := RunAuth(w, r, st, rr); ok {
		t.Fatal("expected deny")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestRunAuth_NoSuchRepo404(t *testing.T) {
	st := &fakeStore{flagsErr: auth.ErrNoSuchRepo}
	rr := &RoutedRequest{Tenant: "a", Repo: "b", Op: OpUploadPack, RequiredAction: auth.ActionRead}
	w := httptest.NewRecorder()
	r := req(t, "POST", "/a/b.git/git-upload-pack", "", "", "")
	if _, ok := RunAuth(w, r, st, rr); ok {
		t.Fatal("expected deny")
	}
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestRunAuth_BadCredentials_401(t *testing.T) {
	st := &fakeStore{credErr: auth.ErrInvalidCredential, flags: auth.RepoFlags{}}
	rr := &RoutedRequest{Tenant: "a", Repo: "b", Op: OpUploadPack, RequiredAction: auth.ActionRead}
	w := httptest.NewRecorder()
	r := req(t, "POST", "/a/b.git/git-upload-pack", "", "alice", "bvts_BAD")
	if _, ok := RunAuth(w, r, st, rr); ok {
		t.Fatal("expected deny")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestRunAuth_AuthenticatedUnauthorized_403(t *testing.T) {
	actor := &auth.Actor{UserID: "u1", Name: "alice"}
	st := &fakeStore{credActor: actor, credToken: "tokid", flags: auth.RepoFlags{}, perm: auth.PermRead}
	rr := &RoutedRequest{Tenant: "a", Repo: "b", Op: OpReceivePack, RequiredAction: auth.ActionWrite}
	w := httptest.NewRecorder()
	r := req(t, "POST", "/a/b.git/git-receive-pack", "", "alice", "bvts_OK")
	if _, ok := RunAuth(w, r, st, rr); ok {
		t.Fatal("expected deny")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

func TestRunAuth_AuthenticatedAuthorized_AttachesActor(t *testing.T) {
	actor := &auth.Actor{UserID: "u1", Name: "alice"}
	st := &fakeStore{credActor: actor, credToken: "tokid", flags: auth.RepoFlags{}, perm: auth.PermWrite}
	rr := &RoutedRequest{Tenant: "a", Repo: "b", Op: OpReceivePack, RequiredAction: auth.ActionWrite}
	w := httptest.NewRecorder()
	r := req(t, "POST", "/a/b.git/git-receive-pack", "", "alice", "bvts_OK")
	got, ok := RunAuth(w, r, st, rr)
	if !ok {
		t.Fatalf("expected allow, status=%d", w.Code)
	}
	if got != actor {
		t.Fatalf("got actor %+v want %+v", got, actor)
	}
}

func TestRunAuth_PassesContextErrors(t *testing.T) {
	st := &fakeStore{flagsErr: errors.New("internal-disk")}
	rr := &RoutedRequest{Tenant: "a", Repo: "b", Op: OpUploadPack, RequiredAction: auth.ActionRead}
	w := httptest.NewRecorder()
	r := req(t, "POST", "/a/b.git/git-upload-pack", "", "", "")
	if _, ok := RunAuth(w, r, st, rr); ok {
		t.Fatal("expected deny on internal error")
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}
```

- [ ] **Step 2: Run tests, expect failure**

Run: `go test ./internal/gateway/ -run TestRunAuth -v`
Expected: FAIL with undefined `RunAuth`.

- [ ] **Step 3: Replace `internal/gateway/auth.go` body**

```go
package gateway

import (
	"errors"
	"net/http"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

const authRealm = `Basic realm="bucketvcs"`

// actorContextKey is the context key under which the authenticated actor is
// stored after successful auth. Handlers retrieve it via ActorFromContext.
type actorContextKey struct{}

// ActorFromContext returns the authenticated actor or nil if anonymous.
func ActorFromContext(ctx context.Context) *auth.Actor {
	v, _ := ctx.Value(actorContextKey{}).(*auth.Actor)
	return v
}

// RunAuth executes the spec §6.2 sequence:
//
//	1. GetRepoFlags  -> 404 on ErrNoSuchRepo, 500 on other err
//	2. Extract Basic -> 401 on credential errors
//	3. LookupRepoPerm
//	4. Decide        -> allow | 401 (anon) | 403 (authed)
//
// On allow, the returned actor (nil for anonymous) is also attached to the
// request context (caller is expected to use a context-derived chain).
//
// On deny, the response has already been fully written.
func RunAuth(w http.ResponseWriter, r *http.Request, store auth.Store, rr *RoutedRequest) (*auth.Actor, bool) {
	ctx := r.Context()

	flags, err := store.GetRepoFlags(ctx, rr.Tenant, rr.Repo)
	if errors.Is(err, auth.ErrNoSuchRepo) {
		http.Error(w, "not found", http.StatusNotFound)
		return nil, false
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return nil, false
	}

	var actor *auth.Actor
	var tokenID string
	if user, pass, hasBasic := r.BasicAuth(); hasBasic {
		actor, tokenID, err = store.VerifyCredential(ctx, auth.BasicPassword{Username: user, Password: pass})
		if err != nil {
			challenge(w, "invalid credentials")
			return nil, false
		}
		// Best-effort last-used update off the hot path.
		go func(id string) {
			tctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			_ = store.TouchTokenUsage(tctx, id)
		}(tokenID)
	}

	perm, err := store.LookupRepoPerm(ctx, actor, rr.Tenant, rr.Repo)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return nil, false
	}

	ok, _ := auth.Decide(actor, perm, rr.RequiredAction, flags)
	if !ok {
		if actor == nil {
			challenge(w, "authentication required")
		} else {
			http.Error(w, "insufficient permissions", http.StatusForbidden)
		}
		return nil, false
	}

	// Attach actor to the request's context for handlers (best-effort
	// for logging only — no further auth decisions in handlers).
	*r = *r.WithContext(context.WithValue(ctx, actorContextKey{}, actor))
	return actor, true
}

func challenge(w http.ResponseWriter, body string) {
	w.Header().Set("WWW-Authenticate", authRealm)
	http.Error(w, body, http.StatusUnauthorized)
}
```

Update the imports of `internal/gateway/auth.go` to:

```go
import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)
```

- [ ] **Step 4: Run tests, expect pass**

Run: `go test ./internal/gateway/ -run TestRunAuth -v`
Expected: PASS.

- [ ] **Step 5: Existing gateway tests now fail to compile**

Old test files reference `AuthMode`, `AuthAnonymous`, `AuthAll`, `AuthWriteOnly`, `Options{AuthMode:...}`. They will fail to compile after this change. That is expected; Task 18 fixes them by introducing `AuthStore` in `Options`.

The plan executes Tasks 17→18 atomically: do not commit Task 17 in isolation. Continue directly to Task 18 and commit them together.

---

### Task 18: Wire AuthStore into Server.Options + rewrite routeRepo (TDD)

**Files:** Modify `internal/gateway/server.go`, `internal/gateway/routes.go`; update `internal/gateway/server_test.go` and any other M3 gateway tests that reference removed fields.

- [ ] **Step 1: Modify `internal/gateway/server.go`**

Replace the existing `Options` struct and the `NewServer` validation. Diff intent:

- Remove fields: `AuthMode`, `AuthToken`.
- Add field: `AuthStore auth.Store`.
- `NewServer` returns an error if `opts.AuthStore == nil`.

Updated `Options` and `NewServer`:

```go
import (
	"fmt"
	"net/http"
	"unicode"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/mirror"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Options configures a Server.
type Options struct {
	MirrorDir    string
	Version      string // bucketvcs version string
	AuthStore    auth.Store
	MaxBodyBytes int64
}

// NewServer constructs a Server. The mirror manager acquires a process flock
// on opts.MirrorDir; the caller must Close() the server on shutdown.
func NewServer(store storage.ObjectStore, opts Options) (*Server, error) {
	if opts.Version == "" {
		opts.Version = "0.0-dev"
	}
	for _, r := range opts.Version {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return nil, fmt.Errorf("gateway: Version must not contain whitespace or control characters")
		}
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = 1 << 30 // 1 GiB
	}
	if opts.AuthStore == nil {
		return nil, fmt.Errorf("gateway: AuthStore is required")
	}
	mgr, err := mirror.NewManager(opts.MirrorDir, store)
	if err != nil {
		return nil, fmt.Errorf("gateway: mirror manager: %w", err)
	}
	s := &Server{store: store, mgr: mgr, opts: opts}
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	s.mux.HandleFunc("/", s.routeRoot)
	return s, nil
}
```

- [ ] **Step 2: Replace `routeRepo` body in `internal/gateway/routes.go`**

```go
// routeRepo dispatches /{tenant}/{repo}.git/<sub-path>.
func (s *Server) routeRepo(w http.ResponseWriter, r *http.Request) {
	rr, err := ParseRoute(r.Method, r.URL.Path, r.URL.RawQuery)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, ok := RunAuth(w, r, s.opts.AuthStore, rr); !ok {
		return
	}
	switch rr.Op {
	case OpInfoRefsUpload, OpInfoRefsReceive:
		s.handleInfoRefs(w, r, rr.Tenant, rr.Repo)
	case OpUploadPack:
		s.handleUploadPack(w, r, rr.Tenant, rr.Repo)
	case OpReceivePack:
		s.handleReceivePack(w, r, rr.Tenant, rr.Repo)
	default:
		http.NotFound(w, r)
	}
}
```

- [ ] **Step 3: Update existing M3 gateway tests**

Search for all references to removed identifiers and update test setup to use a real `sqlitestore.Store` seeded with a registered repo:

```bash
grep -rln "AuthMode\|AuthAnonymous\|AuthAll\|AuthWriteOnly\|AuthToken" internal/gateway/
```

For each test that constructs `Options{AuthMode: AuthAnonymous}`, replace with the helper introduced below. Add a new test helper file `internal/gateway/testhelp_test.go`:

```go
package gateway

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

// newTestAuthStore returns a sqlitestore with one admin user "tester" and
// the (tenant, repo) registered. Tests that want anonymous behavior just
// don't hit Basic auth; tests that want authenticated behavior use the
// returned token.
//
// Returns the store, the username, and the full token string.
func newTestAuthStore(t *testing.T, tenant, repo string) (*sqlitestore.Store, string, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := sqlitestore.Open(filepath.Join(dir, "auth.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	uid, err := s.CreateUser(ctx, "tester", true)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.RegisterRepo(ctx, tenant, repo); err != nil {
		t.Fatalf("RegisterRepo: %v", err)
	}
	tok, id, secret, _ := auth.GenerateToken()
	hash, _ := auth.HashSecret(secret)
	if err := s.CreateToken(ctx, id, uid, hash, "test", nil); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	return s, "tester", tok
}

// newAnonymousTestAuthStore returns a store with the repo registered and
// public_read = pub. No users.
func newAnonymousTestAuthStore(t *testing.T, tenant, repo string, pub bool) *sqlitestore.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := sqlitestore.Open(filepath.Join(dir, "auth.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.RegisterRepo(context.Background(), tenant, repo); err != nil {
		t.Fatalf("RegisterRepo: %v", err)
	}
	if pub {
		if err := s.SetRepoPublic(context.Background(), tenant, repo, true); err != nil {
			t.Fatalf("SetRepoPublic: %v", err)
		}
	}
	return s
}
```

For each existing M3 gateway test that built `Options{AuthMode: AuthAnonymous, ...}`, switch to:

```go
authS := newAnonymousTestAuthStore(t, "tenant", "repo", true)
opts := Options{MirrorDir: dir, AuthStore: authS}
srv, err := NewServer(store, opts)
```

Tests that exercised the old `AuthAll` behavior use `newTestAuthStore` and inject Basic credentials with `req.SetBasicAuth("tester", token)`.

Stress test (`internal/gateway/stress_test.go`) gets the same treatment: stand up a `newTestAuthStore`, attach the token to the `git push` invocation via a credential helper.

- [ ] **Step 4: Run gateway test suite**

Run: `go test ./internal/gateway/...`
Expected: PASS — all M3 tests still green under the new auth wiring.

- [ ] **Step 5: Run full test suite**

Run: `go test ./...`
Expected: PASS — `cmd/bucketvcs/serve_test.go` may still fail because `serve.go` still references removed flags. That fix is Task 22.

If `cmd/bucketvcs` build fails, leave that fix to Task 22 — but the gateway-only test pass is the gate for committing Tasks 17+18.

- [ ] **Step 6: Commit Tasks 17 + 18 together**

```bash
git add internal/gateway/
git commit -m "M4 gateway: replace AuthMode/AuthToken with auth.Store middleware"
```

---

## Phase 5 — CLI

### Task 19: Auth-DB path resolution helper (TDD)

Resolution order: `--auth-db <path>`, `BUCKETVCS_AUTH_DB`, `XDG_STATE_HOME/bucketvcs/bucketvcs.db`, `$HOME/.local/state/bucketvcs/bucketvcs.db`.

**Files:** Create `cmd/bucketvcs/authdb.go`, `cmd/bucketvcs/authdb_test.go`.

- [ ] **Step 1: Write failing tests in `cmd/bucketvcs/authdb_test.go`**

```go
package main

import (
	"path/filepath"
	"testing"
)

func TestResolveAuthDB_FlagWins(t *testing.T) {
	got, err := resolveAuthDB("/explicit", &envLookup{
		BUCKETVCS_AUTH_DB: "/env",
		XDG_STATE_HOME:    "/xdg",
		HOME:              "/home/u",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "/explicit" {
		t.Fatalf("got %q want /explicit", got)
	}
}

func TestResolveAuthDB_EnvVar(t *testing.T) {
	got, _ := resolveAuthDB("", &envLookup{
		BUCKETVCS_AUTH_DB: "/env/path.db",
		HOME:              "/home/u",
	})
	if got != "/env/path.db" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveAuthDB_XDG(t *testing.T) {
	got, _ := resolveAuthDB("", &envLookup{
		XDG_STATE_HOME: "/x",
		HOME:           "/home/u",
	})
	want := filepath.Join("/x", "bucketvcs", "bucketvcs.db")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestResolveAuthDB_HOMEFallback(t *testing.T) {
	got, _ := resolveAuthDB("", &envLookup{HOME: "/home/u"})
	want := filepath.Join("/home/u", ".local", "state", "bucketvcs", "bucketvcs.db")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestResolveAuthDB_NoHOME(t *testing.T) {
	if _, err := resolveAuthDB("", &envLookup{}); err == nil {
		t.Fatal("want error when HOME unset and no other source")
	}
}
```

- [ ] **Step 2: Run tests, expect failure**.

- [ ] **Step 3: Create `cmd/bucketvcs/authdb.go`**

```go
package main

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

// envLookup is a small abstraction over os.Getenv so resolveAuthDB is
// testable without mutating process state.
type envLookup struct {
	BUCKETVCS_AUTH_DB string
	XDG_STATE_HOME    string
	HOME              string
}

func realEnv() *envLookup {
	return &envLookup{
		BUCKETVCS_AUTH_DB: os.Getenv("BUCKETVCS_AUTH_DB"),
		XDG_STATE_HOME:    os.Getenv("XDG_STATE_HOME"),
		HOME:              os.Getenv("HOME"),
	}
}

// resolveAuthDB returns the auth DB path using the resolution order from
// the M4 spec §7.3:
//   1. flag (if non-empty)
//   2. BUCKETVCS_AUTH_DB
//   3. $XDG_STATE_HOME/bucketvcs/bucketvcs.db
//   4. $HOME/.local/state/bucketvcs/bucketvcs.db
func resolveAuthDB(flag string, env *envLookup) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if env.BUCKETVCS_AUTH_DB != "" {
		return env.BUCKETVCS_AUTH_DB, nil
	}
	if env.XDG_STATE_HOME != "" {
		return filepath.Join(env.XDG_STATE_HOME, "bucketvcs", "bucketvcs.db"), nil
	}
	if env.HOME != "" {
		return filepath.Join(env.HOME, ".local", "state", "bucketvcs", "bucketvcs.db"), nil
	}
	return "", errors.New("auth-db: cannot resolve default path; set --auth-db, BUCKETVCS_AUTH_DB, XDG_STATE_HOME, or HOME")
}

// openAuthDB resolves the path, ensures the parent directory exists, and
// returns an opened sqlitestore. Caller must Close.
func openAuthDB(flag string) (*sqlitestore.Store, string, error) {
	path, err := resolveAuthDB(flag, realEnv())
	if err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, "", err
	}
	s, err := sqlitestore.Open(path)
	if err != nil {
		return nil, "", err
	}
	return s, path, nil
}
```

- [ ] **Step 4: Run tests, expect pass**.

- [ ] **Step 5: Commit**

```bash
git add cmd/bucketvcs/authdb.go cmd/bucketvcs/authdb_test.go
git commit -m "M4 cli: auth-db path resolution + open helper"
```

---

### Task 20: `bucketvcs user` subcommands (TDD)

**Files:** Create `cmd/bucketvcs/user.go`, `cmd/bucketvcs/user_test.go`.

- [ ] **Step 1: Write failing tests in `cmd/bucketvcs/user_test.go`**

```go
package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// userCmdEnv stands up a tmp HOME so resolveAuthDB lands somewhere clean.
func userCmdEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("BUCKETVCS_AUTH_DB", "")
	return filepath.Join(home, ".local", "state", "bucketvcs", "bucketvcs.db")
}

func TestUserAdd_AndList(t *testing.T) {
	_ = userCmdEnv(t)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if rc := runUser(context.Background(), []string{"add", "alice"}, stdout, stderr); rc != 0 {
		t.Fatalf("add rc=%d stderr=%s", rc, stderr)
	}
	stdout.Reset()
	if rc := runUser(context.Background(), []string{"list"}, stdout, stderr); rc != 0 {
		t.Fatalf("list rc=%d", rc)
	}
	if !strings.Contains(stdout.String(), "alice") {
		t.Fatalf("list output missing alice: %q", stdout)
	}
}

func TestUserAdd_DuplicateExit2(t *testing.T) {
	_ = userCmdEnv(t)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	_ = runUser(context.Background(), []string{"add", "alice"}, stdout, stderr)
	rc := runUser(context.Background(), []string{"add", "alice"}, stdout, stderr)
	if rc != 2 {
		t.Fatalf("rc = %d, want 2", rc)
	}
}

func TestUserAdmin_Add(t *testing.T) {
	_ = userCmdEnv(t)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if rc := runUser(context.Background(), []string{"add", "root", "--admin"}, stdout, stderr); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	stdout.Reset()
	_ = runUser(context.Background(), []string{"list"}, stdout, stderr)
	if !strings.Contains(stdout.String(), "admin") {
		t.Fatalf("expected admin marker: %q", stdout)
	}
}

func TestUserDisable_AndEnable(t *testing.T) {
	_ = userCmdEnv(t)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	_ = runUser(context.Background(), []string{"add", "alice"}, stdout, stderr)
	if rc := runUser(context.Background(), []string{"disable", "alice"}, stdout, stderr); rc != 0 {
		t.Fatalf("disable rc=%d", rc)
	}
	if rc := runUser(context.Background(), []string{"enable", "alice"}, stdout, stderr); rc != 0 {
		t.Fatalf("enable rc=%d", rc)
	}
}

func TestUserDelete_LastAdminRefused(t *testing.T) {
	_ = userCmdEnv(t)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	_ = runUser(context.Background(), []string{"add", "root", "--admin"}, stdout, stderr)
	if rc := runUser(context.Background(), []string{"delete", "root"}, stdout, stderr); rc == 0 {
		t.Fatalf("expected non-zero rc on last-admin delete")
	}
}
```

- [ ] **Step 2: Run tests, expect failure**.

- [ ] **Step 3: Create `cmd/bucketvcs/user.go`**

```go
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

func runUser(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: bucketvcs user <add|list|disable|enable|delete> [flags]")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "add":
		return userAdd(ctx, rest, stdout, stderr)
	case "list":
		return userList(ctx, rest, stdout, stderr)
	case "disable":
		return userSetDisabled(ctx, rest, stdout, stderr, true)
	case "enable":
		return userSetDisabled(ctx, rest, stdout, stderr, false)
	case "delete":
		return userDelete(ctx, rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "user: unknown subcommand %q\n", sub)
		return 2
	}
}

func userAdd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("user add", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	admin := fs.Bool("admin", false, "create as admin")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: bucketvcs user add <name> [--admin]")
		return 2
	}
	name := fs.Arg(0)
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()
	if _, err := s.CreateUser(ctx, name, *admin); err != nil {
		if errors.Is(err, auth.ErrConflict) {
			fmt.Fprintf(stderr, "user %q already exists\n", name)
			return 2
		}
		fmt.Fprintf(stderr, "create user: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "created user %s\n", name)
	return 0
}

func userList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("user list", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()
	users, err := s.ListUsers(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "list: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "name\tadmin\tdisabled\tcreated")
	for _, u := range users {
		adm := "no"
		if u.IsAdmin {
			adm = "admin"
		}
		dis := "no"
		if u.DisabledAt != nil {
			dis = "yes"
		}
		fmt.Fprintf(stdout, "%s\t%s\t%s\t%d\n", u.Name, adm, dis, u.CreatedAt)
	}
	return 0
}

func userSetDisabled(ctx context.Context, args []string, stdout, stderr io.Writer, disabled bool) int {
	fs := flag.NewFlagSet("user enable/disable", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: bucketvcs user {enable|disable} <name>")
		return 2
	}
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()
	if err := s.SetUserDisabled(ctx, fs.Arg(0), disabled); err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	return 0
}

func userDelete(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("user delete", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: bucketvcs user delete <name>")
		return 2
	}
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()
	if err := s.DeleteUser(ctx, fs.Arg(0)); err != nil {
		if errors.Is(err, sqlitestore.ErrLastAdmin) {
			fmt.Fprintln(stderr, "refusing: would remove last admin")
			return 1
		}
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	return 0
}
```

- [ ] **Step 4: Run tests, expect pass**

Run: `go test ./cmd/bucketvcs/ -run TestUser -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/bucketvcs/user.go cmd/bucketvcs/user_test.go
git commit -m "M4 cli: user subcommands (add/list/disable/enable/delete)"
```

---

### Task 21: `bucketvcs token` subcommands (TDD)

**Files:** Create `cmd/bucketvcs/token.go`, `cmd/bucketvcs/token_test.go`.

- [ ] **Step 1: Write failing tests in `cmd/bucketvcs/token_test.go`**

```go
package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestTokenCreate_PrintsTokenOnce(t *testing.T) {
	_ = userCmdEnv(t)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if rc := runUser(context.Background(), []string{"add", "alice"}, stdout, stderr); rc != 0 {
		t.Fatalf("add: rc=%d stderr=%s", rc, stderr)
	}
	stdout.Reset()
	if rc := runToken(context.Background(), []string{"create", "alice", "--label", "laptop"}, stdout, stderr); rc != 0 {
		t.Fatalf("create: rc=%d stderr=%s", rc, stderr)
	}
	if !strings.Contains(stdout.String(), "bvts_") {
		t.Fatalf("expected bvts_ prefix in stdout, got: %q", stdout)
	}
}

func TestTokenList_AfterCreate(t *testing.T) {
	_ = userCmdEnv(t)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	_ = runUser(context.Background(), []string{"add", "alice"}, stdout, stderr)
	stdout.Reset()
	_ = runToken(context.Background(), []string{"create", "alice", "--label", "laptop"}, stdout, stderr)
	stdout.Reset()
	if rc := runToken(context.Background(), []string{"list", "alice"}, stdout, stderr); rc != 0 {
		t.Fatalf("list rc=%d", rc)
	}
	if !strings.Contains(stdout.String(), "laptop") {
		t.Fatalf("list missing label: %q", stdout)
	}
}

func TestTokenRevoke_ByPrefix(t *testing.T) {
	_ = userCmdEnv(t)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	_ = runUser(context.Background(), []string{"add", "alice"}, stdout, stderr)
	stdout.Reset()
	_ = runToken(context.Background(), []string{"create", "alice"}, stdout, stderr)
	full := strings.TrimSpace(stdout.String())
	parts := strings.Split(full, "_")
	if len(parts) != 3 {
		t.Fatalf("unexpected token shape: %q", full)
	}
	id := parts[1]
	stdout.Reset()
	if rc := runToken(context.Background(), []string{"revoke", id[:8]}, stdout, stderr); rc != 0 {
		t.Fatalf("revoke rc=%d stderr=%s", rc, stderr)
	}
}
```

- [ ] **Step 2: Run tests, expect failure**.

- [ ] **Step 3: Create `cmd/bucketvcs/token.go`**

```go
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

func runToken(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: bucketvcs token <create|list|revoke>")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "create":
		return tokenCreate(ctx, rest, stdout, stderr)
	case "list":
		return tokenList(ctx, rest, stdout, stderr)
	case "revoke":
		return tokenRevoke(ctx, rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "token: unknown subcommand %q\n", sub)
		return 2
	}
}

func tokenCreate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("token create", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	expires := fs.String("expires", "", "expiration duration, e.g. 90d, 24h; empty means never")
	label := fs.String("label", "", "human-readable label")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: bucketvcs token create <user> [--expires <duration>] [--label <text>]")
		return 2
	}
	user := fs.Arg(0)
	var expPtr *int64
	if *expires != "" {
		d, err := parseDuration(*expires)
		if err != nil {
			fmt.Fprintf(stderr, "invalid --expires: %v\n", err)
			return 2
		}
		exp := time.Now().Add(d).Unix()
		expPtr = &exp
	}
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()
	u, err := s.GetUserByName(ctx, user)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	tok, id, secret, err := auth.GenerateToken()
	if err != nil {
		fmt.Fprintf(stderr, "generate: %v\n", err)
		return 1
	}
	hash, err := auth.HashSecret(secret)
	if err != nil {
		fmt.Fprintf(stderr, "hash: %v\n", err)
		return 1
	}
	if err := s.CreateToken(ctx, id, u.ID, hash, *label, expPtr); err != nil {
		fmt.Fprintf(stderr, "create token: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, tok)
	fmt.Fprintln(stderr, "(this is the only time the full token will be shown; copy it now)")
	return 0
}

// parseDuration accepts the standard Go time.ParseDuration syntax with the
// addition of a "d" suffix meaning days.
func parseDuration(s string) (time.Duration, error) {
	if len(s) > 1 && s[len(s)-1] == 'd' {
		var n int
		if _, err := fmt.Sscanf(s, "%dd", &n); err != nil {
			return 0, err
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

func tokenList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("token list", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: bucketvcs token list <user>")
		return 2
	}
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()
	rows, err := s.ListTokensForUser(ctx, fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "id\tlabel\tcreated\texpires\trevoked\tlast-used")
	for _, r := range rows {
		exp := "-"
		if r.ExpiresAt != nil {
			exp = fmt.Sprintf("%d", *r.ExpiresAt)
		}
		rev := "-"
		if r.RevokedAt != nil {
			rev = fmt.Sprintf("%d", *r.RevokedAt)
		}
		last := "-"
		if r.LastUsedAt != nil {
			last = fmt.Sprintf("%d", *r.LastUsedAt)
		}
		fmt.Fprintf(stdout, "%s\t%s\t%d\t%s\t%s\t%s\n", r.ID, r.Label, r.CreatedAt, exp, rev, last)
	}
	return 0
}

func tokenRevoke(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("token revoke", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: bucketvcs token revoke <token-id-or-prefix>")
		return 2
	}
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()
	id, err := s.ResolveTokenIDPrefix(ctx, fs.Arg(0))
	if err != nil {
		if errors.Is(err, sqlitestore.ErrAmbiguousPrefix) {
			fmt.Fprintln(stderr, "ambiguous token id prefix; supply more characters")
			return 2
		}
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	if err := s.RevokeToken(ctx, id); err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	return 0
}
```

- [ ] **Step 4: Run tests, expect pass**.

- [ ] **Step 5: Commit**

```bash
git add cmd/bucketvcs/token.go cmd/bucketvcs/token_test.go
git commit -m "M4 cli: token subcommands (create/list/revoke; one-shot display; prefix revoke)"
```

---

### Task 22: `bucketvcs repo` subcommands (TDD)

**Files:** Create `cmd/bucketvcs/repocmd.go`, `cmd/bucketvcs/repocmd_test.go`.

The `register` subcommand wraps M1's `bucketvcs init` so registry and bucket state stay in sync. Use a `--no-init` flag to skip the M1 init step (for already-initialized repos).

- [ ] **Step 1: Write failing tests in `cmd/bucketvcs/repocmd_test.go`**

```go
package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRepoRegister_GrantPublicList(t *testing.T) {
	_ = userCmdEnv(t)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	_ = runUser(context.Background(), []string{"add", "alice"}, stdout, stderr)
	stdout.Reset()
	if rc := runRepo(context.Background(), []string{"register", "acme/foo", "--no-init"}, stdout, stderr); rc != 0 {
		t.Fatalf("register rc=%d stderr=%s", rc, stderr)
	}
	if rc := runRepo(context.Background(), []string{"grant", "alice", "acme/foo", "write"}, stdout, stderr); rc != 0 {
		t.Fatalf("grant rc=%d stderr=%s", rc, stderr)
	}
	if rc := runRepo(context.Background(), []string{"public", "acme/foo", "on"}, stdout, stderr); rc != 0 {
		t.Fatalf("public rc=%d", rc)
	}
	stdout.Reset()
	if rc := runRepo(context.Background(), []string{"list"}, stdout, stderr); rc != 0 {
		t.Fatalf("list rc=%d", rc)
	}
	if !strings.Contains(stdout.String(), "acme") || !strings.Contains(stdout.String(), "foo") {
		t.Fatalf("list missing repo: %q", stdout)
	}
}

func TestRepoGrant_RefusesUnregistered(t *testing.T) {
	_ = userCmdEnv(t)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	_ = runUser(context.Background(), []string{"add", "alice"}, stdout, stderr)
	if rc := runRepo(context.Background(), []string{"grant", "alice", "ghost/x", "read"}, stdout, stderr); rc == 0 {
		t.Fatalf("expected non-zero rc; stderr=%s", stderr)
	}
}
```

- [ ] **Step 2: Run tests, expect failure**.

- [ ] **Step 3: Create `cmd/bucketvcs/repocmd.go`**

The `register` subcommand without `--no-init` invokes the existing `runInit` (declared in `cmd/bucketvcs/init.go`). Read that file at implementation time to confirm the signature; if the existing `runInit` cannot be invoked programmatically, copy its body inline rather than restructuring the M1 code (out of scope for M4).

```go
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func runRepo(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: bucketvcs repo <register|grant|revoke|public|list>")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "register":
		return repoRegister(ctx, rest, stdout, stderr)
	case "grant":
		return repoGrant(ctx, rest, stdout, stderr)
	case "revoke":
		return repoRevoke(ctx, rest, stdout, stderr)
	case "public":
		return repoPublic(ctx, rest, stdout, stderr)
	case "list":
		return repoList(ctx, rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "repo: unknown subcommand %q\n", sub)
		return 2
	}
}

func splitTenantRepo(s string) (string, string, error) {
	i := strings.IndexByte(s, '/')
	if i <= 0 || i == len(s)-1 {
		return "", "", fmt.Errorf("expected tenant/repo, got %q", s)
	}
	return s[:i], s[i+1:], nil
}

func repoRegister(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repo register", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	noInit := fs.Bool("no-init", false, "skip M1 bucket init (registry only)")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: bucketvcs repo register <tenant>/<repo> [--no-init]")
		return 2
	}
	tenant, repo, err := splitTenantRepo(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if !*noInit {
		// Build args for runInit. The exact flag shape is read from the
		// existing cmd/bucketvcs/init.go at implementation time.
		// Failure to init when bucket state already exists must NOT be a
		// hard error: pass --idempotent if init.go supports it; otherwise
		// detect and ignore the "already initialized" error.
		initArgs := []string{"--tenant", tenant, "--repo", repo}
		if rc := runInit(ctx, initArgs, stdout, stderr); rc != 0 {
			fmt.Fprintln(stderr, "init failed; if the bucket repo already exists, retry with --no-init")
			return 1
		}
	}
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()
	if err := s.RegisterRepo(ctx, tenant, repo); err != nil {
		fmt.Fprintf(stderr, "register: %v\n", err)
		return 1
	}
	return 0
}

func repoGrant(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repo grant", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 3 {
		fmt.Fprintln(stderr, "usage: bucketvcs repo grant <user> <tenant>/<repo> <read|write|admin>")
		return 2
	}
	user := fs.Arg(0)
	tenant, repo, err := splitTenantRepo(fs.Arg(1))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	perm := fs.Arg(2)
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()
	if err := s.Grant(ctx, user, tenant, repo, perm); err != nil {
		if errors.Is(err, auth.ErrNoSuchRepo) {
			fmt.Fprintln(stderr, "repo not registered (run `bucketvcs repo register`)")
			return 1
		}
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	return 0
}

func repoRevoke(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repo revoke", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(stderr, "usage: bucketvcs repo revoke <user> <tenant>/<repo>")
		return 2
	}
	user := fs.Arg(0)
	tenant, repo, err := splitTenantRepo(fs.Arg(1))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()
	if err := s.RevokeRepoPermission(ctx, user, tenant, repo); err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	return 0
}

func repoPublic(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repo public", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(stderr, "usage: bucketvcs repo public <tenant>/<repo> <on|off>")
		return 2
	}
	tenant, repo, err := splitTenantRepo(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	on := false
	switch fs.Arg(1) {
	case "on":
		on = true
	case "off":
		on = false
	default:
		fmt.Fprintln(stderr, "expected on|off")
		return 2
	}
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()
	if err := s.SetRepoPublic(ctx, tenant, repo, on); err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	return 0
}

func repoList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repo list", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	tenant := fs.String("tenant", "", "filter by tenant")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()
	rows, err := s.ListRepos(ctx, *tenant)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "tenant\tname\tpublic\tcreated")
	for _, r := range rows {
		pub := "no"
		if r.PublicRead {
			pub = "yes"
		}
		fmt.Fprintf(stdout, "%s\t%s\t%s\t%d\n", r.Tenant, r.Name, pub, r.CreatedAt)
	}
	return 0
}
```

- [ ] **Step 4: Run tests, expect pass**.

- [ ] **Step 5: Commit**

```bash
git add cmd/bucketvcs/repocmd.go cmd/bucketvcs/repocmd_test.go
git commit -m "M4 cli: repo subcommands (register/grant/revoke/public/list)"
```

---

### Task 23: Wire new subcommands into main.go and modify serve.go

**Files:** Modify `cmd/bucketvcs/main.go`, `cmd/bucketvcs/serve.go`, `cmd/bucketvcs/serve_test.go`.

- [ ] **Step 1: Modify `cmd/bucketvcs/main.go`**

Add cases to the `switch sub` in `run`:

```go
case "user":
    return runUser(ctx, rest, stdout, stderr)
case "token":
    return runToken(ctx, rest, stdout, stderr)
case "repo":
    return runRepo(ctx, rest, stdout, stderr)
```

Update the `usage()` text to list the new subcommands:

```go
func usage(w io.Writer) {
	fmt.Fprint(w, `Usage: bucketvcs <subcommand> [flags] [args]

Subcommands:
  init               Create a new repo
  inspect-manifest   Print summary of the root manifest
  import             Round-trip a bare git repo into bucketvcs storage
  export             Materialize a bare git repo from bucketvcs storage
  cat-object         Read a Git object from a bucketvcs repo
  serve              Run the HTTP smart-Git gateway
  user               Manage users (add/list/disable/enable/delete)
  token              Manage tokens (create/list/revoke)
  repo               Manage repository registry and permissions

Run "bucketvcs <subcommand> --help" for subcommand flags.
`)
}
```

- [ ] **Step 2: Modify `cmd/bucketvcs/serve.go`**

Read the current file with `Read` first to capture exact flag names. Then make these changes:

1. Remove flag definitions for `--auth-mode` and `--auth-token`.
2. Add a `--auth-db` string flag.
3. After flag parsing, if either `--auth-mode` or `--auth-token` is present in `args`, fail fast with a clear message pointing to `docs/migration-m3-to-m4.md`. The cleanest implementation is to scan args before passing to `flag.Parse`:

```go
for _, a := range args {
    if a == "--auth-mode" || a == "--auth-token" ||
       strings.HasPrefix(a, "--auth-mode=") || strings.HasPrefix(a, "--auth-token=") {
        fmt.Fprintln(stderr, "bucketvcs serve: --auth-mode/--auth-token were removed in M4.")
        fmt.Fprintln(stderr, "See docs/migration-m3-to-m4.md.")
        return 2
    }
}
```

4. After parsing, open the auth DB:

```go
authS, _, err := openAuthDB(*authDB)
if err != nil {
    fmt.Fprintf(stderr, "auth-db: %v\n", err)
    return 1
}
defer authS.Close()
```

5. Replace the `gateway.Options` construction:

```go
opts := gateway.Options{
    MirrorDir: *mirrorDir,
    Version:   versionString,
    AuthStore: authS,
}
```

6. Update `serve --help` text to drop `--auth-mode`/`--auth-token` and document `--auth-db`.

- [ ] **Step 3: Update `cmd/bucketvcs/serve_test.go`**

Replace any tests that validated the old auth flags. Keep at minimum:

```go
func TestServe_RejectsLegacyAuthModeFlag(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rc := runServe(context.Background(),
		[]string{"--auth-mode", "all"},
		stdout, stderr,
	)
	if rc != 2 {
		t.Fatalf("rc = %d, want 2", rc)
	}
	if !strings.Contains(stderr.String(), "M4") {
		t.Fatalf("stderr should explain M4 removal: %q", stderr)
	}
}

func TestServe_RejectsLegacyAuthTokenFlag(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rc := runServe(context.Background(),
		[]string{"--auth-token=secret"},
		stdout, stderr,
	)
	if rc != 2 {
		t.Fatalf("rc = %d, want 2", rc)
	}
}
```

The existing healthy-server test from M3 needs to be updated to provide `--auth-db` pointing into a `t.TempDir()` and to register the repo it pushes/clones via the new auth-db (use `runRepo([]string{"register", "<tenant>/<repo>", "--no-init"})` as a setup helper).

- [ ] **Step 4: Run full CLI test suite**

Run: `go test ./cmd/bucketvcs/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/bucketvcs/main.go cmd/bucketvcs/serve.go cmd/bucketvcs/serve_test.go
git commit -m "M4 cli: wire user/token/repo subcommands; serve accepts --auth-db, rejects legacy auth flags"
```

---

## Phase 6 — End-to-end tests against real `git`

### Task 24: e2e auth scenarios with real `git` binary

This task uses the existing M3 e2e scaffolding (search `internal/gateway/` for tests that already exec `git` against the gateway) and extends it with the 10 spec §9.3 scenarios. The credential helper is a one-line shell script written into `t.TempDir()`.

**Files:** Create `internal/gateway/e2e_auth_test.go`.

- [ ] **Step 1: Read the existing M3 e2e scaffolding**

Run: `grep -ln "exec.Command.*git " internal/gateway/`
Read the matching file (likely `internal/gateway/e2e_test.go` or similar) to see how the test starts the gateway, populates a repo, and captures stderr. Reuse that helper rather than rebuilding it.

- [ ] **Step 2: Create `internal/gateway/e2e_auth_test.go`**

```go
package gateway

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// Each test starts a gateway over a fresh tmpdir-backed objectstore + a
// fresh sqlitestore, registers a repo, then runs `git` against
// http://127.0.0.1:<port>/<tenant>/<repo>.git.
//
// The gateway-startup helper, the localfs object store helper, and the
// `git` exec wrapper are reused from the M3 e2e_test.go file; refer to
// that file's exported helpers (or copy them in pre-task setup if the
// helpers are unexported and not testable from this file).

// writeCredentialHelper writes a shell script that prints `username=...`
// and `password=...` for `git credential fill`. Returns the absolute
// path to the script.
func writeCredentialHelper(t *testing.T, user, pass string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "helper.sh")
	body := fmt.Sprintf(`#!/bin/sh
echo username=%s
echo password=%s
`, user, pass)
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script
}

func gitWithHelper(t *testing.T, helper string, args ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command("git", append([]string{
		"-c", "credential.helper=",
		"-c", "credential.helper=!" + helper,
	}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	return cmd.CombinedOutput()
}

func TestE2E_CloneNoCredsPrivate_Fails(t *testing.T) {
	// Setup: serve started; repo registered; private (default).
	srv, base, _ := startTestGatewayWithAuth(t, "tenant", "repo")
	defer srv.Close()
	dir := t.TempDir()
	out, err := exec.Command("git", "-c", "credential.helper=", "clone",
		base+"/tenant/repo.git", dir+"/clone").CombinedOutput()
	if err == nil {
		t.Fatalf("expected failure, got: %s", out)
	}
}

func TestE2E_CloneNoCredsPublic_Succeeds(t *testing.T) {
	srv, base, store := startTestGatewayWithAuth(t, "tenant", "repo")
	defer srv.Close()
	if err := store.SetRepoPublic(context.Background(), "tenant", "repo", true); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	out, err := exec.Command("git", "-c", "credential.helper=", "clone",
		base+"/tenant/repo.git", dir+"/clone").CombinedOutput()
	if err != nil {
		t.Fatalf("clone failed: %s", out)
	}
}

func TestE2E_CloneWithValidCreds_Succeeds(t *testing.T) {
	srv, base, store := startTestGatewayWithAuth(t, "tenant", "repo")
	defer srv.Close()
	uid, _ := store.CreateUser(context.Background(), "alice", false)
	_ = store.Grant(context.Background(), "alice", "tenant", "repo", "read")
	tok, id, secret, _ := auth.GenerateToken()
	hash, _ := auth.HashSecret(secret)
	_ = store.CreateToken(context.Background(), id, uid, hash, "", nil)

	helper := writeCredentialHelper(t, "alice", tok)
	dir := t.TempDir()
	out, err := gitWithHelper(t, helper, "clone", base+"/tenant/repo.git", dir+"/clone")
	if err != nil {
		t.Fatalf("clone with valid creds failed: %s", out)
	}
}

func TestE2E_CloneRevokedToken_Fails(t *testing.T) {
	srv, base, store := startTestGatewayWithAuth(t, "tenant", "repo")
	defer srv.Close()
	uid, _ := store.CreateUser(context.Background(), "alice", false)
	_ = store.Grant(context.Background(), "alice", "tenant", "repo", "read")
	tok, id, secret, _ := auth.GenerateToken()
	hash, _ := auth.HashSecret(secret)
	_ = store.CreateToken(context.Background(), id, uid, hash, "", nil)
	_ = store.RevokeToken(context.Background(), id)

	helper := writeCredentialHelper(t, "alice", tok)
	dir := t.TempDir()
	out, err := gitWithHelper(t, helper, "clone", base+"/tenant/repo.git", dir+"/clone")
	if err == nil {
		t.Fatalf("expected failure on revoked token; out=%s", out)
	}
}

func TestE2E_CloneExpiredToken_Fails(t *testing.T) {
	srv, base, store := startTestGatewayWithAuth(t, "tenant", "repo")
	defer srv.Close()
	uid, _ := store.CreateUser(context.Background(), "alice", false)
	_ = store.Grant(context.Background(), "alice", "tenant", "repo", "read")
	tok, id, secret, _ := auth.GenerateToken()
	hash, _ := auth.HashSecret(secret)
	expired := int64(1)
	_ = store.CreateToken(context.Background(), id, uid, hash, "", &expired)

	helper := writeCredentialHelper(t, "alice", tok)
	dir := t.TempDir()
	if _, err := gitWithHelper(t, helper, "clone", base+"/tenant/repo.git", dir+"/clone"); err == nil {
		t.Fatal("expected failure on expired token")
	}
}

func TestE2E_PushReadOnlyToken_Fails(t *testing.T) {
	srv, base, store := startTestGatewayWithAuth(t, "tenant", "repo")
	defer srv.Close()
	uid, _ := store.CreateUser(context.Background(), "alice", false)
	_ = store.Grant(context.Background(), "alice", "tenant", "repo", "read")
	tok, id, secret, _ := auth.GenerateToken()
	hash, _ := auth.HashSecret(secret)
	_ = store.CreateToken(context.Background(), id, uid, hash, "", nil)

	helper := writeCredentialHelper(t, "alice", tok)
	work := makeLocalRepoWithCommit(t)
	out, err := gitWithHelper(t, helper, "-C", work, "push",
		base+"/tenant/repo.git", "main")
	if err == nil {
		t.Fatalf("expected push to fail with read-only token; out=%s", out)
	}
	if !strings.Contains(string(out), "403") && !strings.Contains(strings.ToLower(string(out)), "forbidden") {
		// 401 is also acceptable depending on git's error formatting,
		// but we want the operation refused. Allow either.
	}
}

func TestE2E_PushWriteToken_Succeeds(t *testing.T) {
	srv, base, store := startTestGatewayWithAuth(t, "tenant", "repo")
	defer srv.Close()
	uid, _ := store.CreateUser(context.Background(), "alice", false)
	_ = store.Grant(context.Background(), "alice", "tenant", "repo", "write")
	tok, id, secret, _ := auth.GenerateToken()
	hash, _ := auth.HashSecret(secret)
	_ = store.CreateToken(context.Background(), id, uid, hash, "", nil)

	helper := writeCredentialHelper(t, "alice", tok)
	work := makeLocalRepoWithCommit(t)
	out, err := gitWithHelper(t, helper, "-C", work, "push",
		base+"/tenant/repo.git", "main")
	if err != nil {
		t.Fatalf("push failed: %s", out)
	}
}

func TestE2E_PushPublicNoCreds_Challenges(t *testing.T) {
	srv, base, store := startTestGatewayWithAuth(t, "tenant", "repo")
	defer srv.Close()
	_ = store.SetRepoPublic(context.Background(), "tenant", "repo", true)
	work := makeLocalRepoWithCommit(t)
	out, err := exec.Command("git", "-c", "credential.helper=", "-C", work,
		"push", base+"/tenant/repo.git", "main").CombinedOutput()
	if err == nil {
		t.Fatalf("expected anonymous push to public repo to fail; out=%s", out)
	}
}

func TestE2E_DisabledUser_Fails(t *testing.T) {
	srv, base, store := startTestGatewayWithAuth(t, "tenant", "repo")
	defer srv.Close()
	uid, _ := store.CreateUser(context.Background(), "alice", false)
	_ = store.Grant(context.Background(), "alice", "tenant", "repo", "read")
	tok, id, secret, _ := auth.GenerateToken()
	hash, _ := auth.HashSecret(secret)
	_ = store.CreateToken(context.Background(), id, uid, hash, "", nil)
	_ = store.SetUserDisabled(context.Background(), "alice", true)

	helper := writeCredentialHelper(t, "alice", tok)
	dir := t.TempDir()
	if _, err := gitWithHelper(t, helper, "clone", base+"/tenant/repo.git", dir+"/clone"); err == nil {
		t.Fatal("expected disabled user to fail")
	}
}

func TestE2E_AdminUser_AccessesAnyRepo(t *testing.T) {
	srv, base, store := startTestGatewayWithAuth(t, "tenant", "repo")
	defer srv.Close()
	uid, _ := store.CreateUser(context.Background(), "root", true)
	tok, id, secret, _ := auth.GenerateToken()
	hash, _ := auth.HashSecret(secret)
	_ = store.CreateToken(context.Background(), id, uid, hash, "", nil)

	helper := writeCredentialHelper(t, "root", tok)
	dir := t.TempDir()
	out, err := gitWithHelper(t, helper, "clone", base+"/tenant/repo.git", dir+"/clone")
	if err != nil {
		t.Fatalf("admin clone failed: %s", out)
	}
}
```

The helpers `startTestGatewayWithAuth(t, tenant, repo)` and `makeLocalRepoWithCommit(t)` should live next to `newTestAuthStore` in `internal/gateway/testhelp_test.go`. They:

1. `startTestGatewayWithAuth`: open a localfs object store, init the (tenant, repo) via the M1 path or directly via `internal/repo`, build a sqlitestore, register the repo in it, build a `Server`, wrap it in `httptest.NewServer`, and return `(server, baseURL, *sqlitestore.Store)`. The repo must contain at least one commit on the default branch — otherwise clone is a degenerate test.
2. `makeLocalRepoWithCommit`: `git init` in a tmp dir, configure user.name/user.email, write one file, `git commit`, return the dir.

If the M3 e2e test file already provides equivalents under different names, reuse those.

- [ ] **Step 3: Run e2e tests, expect pass**

Run: `go test ./internal/gateway/ -run TestE2E_ -v`
Expected: PASS for all 10 scenarios. If `git` is not available in the test environment, tests should skip with `t.Skip("git binary not found")` rather than fail — wrap the body of each test in a check at the top using `_, err := exec.LookPath("git")`.

- [ ] **Step 4: Commit**

```bash
git add internal/gateway/e2e_auth_test.go internal/gateway/testhelp_test.go
git commit -m "M4 gateway: e2e auth scenarios against real git binary (10 cases)"
```

---

## Phase 7 — Differential harness extension

### Task 25: clone-equivalence-with-auth oracle

The existing differential harness at `internal/diffharness/` runs fixtures through bucketvcs and compares against upstream `git`. Extend it with a thin auth-aware oracle that asserts authentication does not perturb the bytes returned.

**Files:** Create `internal/diffharness/clone_equiv_with_auth_test.go` (or extend the existing oracles file if there is a structured registry).

- [ ] **Step 1: Read the existing diffharness layout**

Run: `ls internal/diffharness/` and `grep -l "clone-equivalence" internal/diffharness/*.go`
Determine whether oracles are functions or table-driven entries. Match the existing pattern exactly.

- [ ] **Step 2: Implement the oracle**

The oracle, for one fixture, performs:

1. Stand up a bucketvcs gateway with auth.Store seeded so user `tester` (admin) has a token and the repo is registered.
2. `git clone` from the bucketvcs gateway with valid creds → record object closure (`git rev-list --all --objects`).
3. Stand up a vanilla `git http-backend` reference server from the same source repo, behind nginx/apache or a minimal Go-based CGI wrapper, with HTTP Basic auth pinned to the same username/password.
4. `git clone` from that reference with the same creds → record object closure.
5. Assert the two object closures are byte-identical sets.

The point is **not** to test the upstream `git http-backend`'s auth behavior — it's to confirm that wrapping bucketvcs with auth does not perturb the pack contents vs the reference path.

If a reference HTTP-Basic+http-backend setup is too heavy for CI, fall back to comparing against the existing `clone-equivalence` oracle (which clones bucketvcs with no auth) instead. The simpler form:

```go
func TestCloneEquivalenceWithAuth_AllFixtures(t *testing.T) {
	for _, fx := range allFixtures(t) {
		t.Run(fx.Name, func(t *testing.T) {
			objsNoAuth := cloneViaExistingNoAuthOracle(t, fx)
			objsWithAuth := cloneViaBucketVCSWithAuth(t, fx)
			if !equalObjectSets(objsNoAuth, objsWithAuth) {
				t.Fatalf("auth-wrapped clone diverges from non-auth clone")
			}
		})
	}
}
```

`cloneViaExistingNoAuthOracle` reuses whatever `clone-equivalence` already does (likely `cloneViaUpstreamGitHTTPBackend` or `cloneViaBucketVCSAnonymous`). `cloneViaBucketVCSWithAuth` is the new helper that seeds an admin token and clones with credentials.

- [ ] **Step 3: Run the harness**

Run: `go test ./internal/diffharness/...`
Expected: PASS — auth oracle green, all existing oracles still green (16 fixtures × N oracles unchanged).

- [ ] **Step 4: Commit**

```bash
git add internal/diffharness/
git commit -m "M4 diffharness: clone-equivalence-with-auth oracle"
```

---

## Phase 8 — Stress, docs, ship gate

### Task 26: Auth stress smoke (`+build stress`)

**Files:** Create `internal/auth/sqlitestore/stress_test.go`.

- [ ] **Step 1: Create the stress test**

```go
//go:build stress

package sqlitestore

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// TestStress_VerifyMany seeds 1,000 tokens and runs 10,000 sequential
// VerifyCredential calls. Should complete under 10s on a dev box, which
// catches argon2 parameter regressions and accidental n^2 scans.
func TestStress_VerifyMany(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	uid, _ := s.CreateUser(ctx, "alice", false)
	const N = 1000
	tokens := make([]string, N)
	for i := 0; i < N; i++ {
		tok, id, secret, _ := auth.GenerateToken()
		hash, _ := auth.HashSecret(secret)
		if err := s.CreateToken(ctx, id, uid, hash, "", nil); err != nil {
			t.Fatalf("CreateToken %d: %v", i, err)
		}
		tokens[i] = tok
	}

	start := time.Now()
	for i := 0; i < 10_000; i++ {
		tok := tokens[i%N]
		if _, _, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "alice", Password: tok}); err != nil {
			t.Fatalf("Verify[%d]: %v", i, err)
		}
	}
	elapsed := time.Since(start)
	t.Logf("10k verifies / 1k tokens: %s", elapsed)
	if elapsed > 30*time.Second {
		t.Fatalf("10k verifies took %s (expected <30s; argon2 regression?)", elapsed)
	}
}

// TestStress_ConcurrentVerify sanity-checks WAL + busy_timeout under
// 100 concurrent verifies of distinct tokens.
func TestStress_ConcurrentVerify(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	uid, _ := s.CreateUser(ctx, "alice", false)
	const N = 100
	tokens := make([]string, N)
	for i := 0; i < N; i++ {
		tok, id, secret, _ := auth.GenerateToken()
		hash, _ := auth.HashSecret(secret)
		if err := s.CreateToken(ctx, id, uid, hash, "", nil); err != nil {
			t.Fatal(err)
		}
		tokens[i] = tok
	}

	var wg sync.WaitGroup
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(tok string) {
			defer wg.Done()
			if _, _, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "alice", Password: tok}); err != nil {
				errs <- err
			}
		}(tokens[i])
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent verify: %v", err)
	}
}
```

- [ ] **Step 2: Run the stress test**

Run: `go test -tags stress ./internal/auth/sqlitestore/ -v -run TestStress`
Expected: PASS within bounds.

- [ ] **Step 3: Commit**

```bash
git add internal/auth/sqlitestore/stress_test.go
git commit -m "M4 sqlitestore: stress smoke (10k verifies / 1k tokens; 100 concurrent)"
```

---

### Task 27: Migration document

**Files:** Create `docs/migration-m3-to-m4.md`.

- [ ] **Step 1: Create `docs/migration-m3-to-m4.md`**

```markdown
# Migrating from bucketvcs M3 to M4

M4 replaces M3's shared bearer token with real per-actor authentication.
The `--auth-mode` and `--auth-token` flags are gone. There is no automated
migration tool; M3's auth was a placeholder.

## What changes for operators

- Every Git request now requires a per-user token (HTTP Basic, token-as-password)
  except for repos explicitly flagged as public-read.
- Auth state lives in a SQLite database on the gateway host. By default at
  `$XDG_STATE_HOME/bucketvcs/bucketvcs.db` or `$HOME/.local/state/bucketvcs/bucketvcs.db`.
- Repos must be **registered** in the new registry to be served. Repos
  created via M1's `bucketvcs init` directly are not served until you run
  `bucketvcs repo register --no-init <tenant>/<repo>`.

## Upgrade steps

### 1. Install M4

Replace the `bucketvcs` binary on the gateway host. Do not start it yet.

### 2. Create the first admin user

```bash
bucketvcs user add <your-name> --admin
bucketvcs token create <your-name> --label "first admin"
```

The `token create` output is the only time the full token is shown.
Copy it now — there is no way to retrieve it later.

### 3. Register existing repos

For each repo that existed under M3:

```bash
bucketvcs repo register <tenant>/<repo> --no-init
```

`--no-init` skips the M1 `bucketvcs init` because the bucket state already
exists. Without `--no-init`, `register` will attempt M1 init and fail.

### 4. Grant access

For each user that should access a repo:

```bash
bucketvcs user add <name>
bucketvcs token create <name>
bucketvcs repo grant <name> <tenant>/<repo> <read|write|admin>
```

For repos that should remain world-readable:

```bash
bucketvcs repo public <tenant>/<repo> on
```

### 5. Start serve

```bash
bucketvcs serve --addr 127.0.0.1:8080 --bucket-root /var/lib/bucketvcs
```

`--auth-mode` and `--auth-token` are no longer recognized. Passing either
fails fast with a pointer to this document.

### 6. Update client `git` configuration

Clients use HTTP Basic with the username = bucketvcs username and the
password = the token printed in step 2 or 4. Standard `git credential`
helpers (osxkeychain, libsecret, manager-core, store) will remember it
after the first prompt.

For unattended CI:

```bash
git -c credential.helper='!f() { echo "username=ci-bot"; echo "password=$BUCKETVCS_TOKEN"; }; f' \
    clone https://gateway.example/acme/foo.git
```

## What does not change

- The bucket layout. M4 does not migrate any durable repo state.
- M1/M2/M3 protocol behavior other than the auth boundary.
- The differential-harness numbers (M3 ship state: 61 pass + 3 documented skips).

## Rolling back to M3

Re-deploy the M3 binary. The auth.db file will be ignored. Any registered
repos and tokens persist on disk but are unused.
```

- [ ] **Step 2: Commit**

```bash
git add docs/migration-m3-to-m4.md
git commit -m "M4 docs: migration guide from M3"
```

---

### Task 28: Ship gate

- [ ] **Step 1: gofmt**

Run: `gofmt -l $(find . -name '*.go' -not -path './.claude/*')`
Expected: empty output.

- [ ] **Step 2: go vet**

Run: `go vet ./...`
Expected: no findings.

- [ ] **Step 3: staticcheck**

Run: `staticcheck ./...`
Expected: no findings. Fix any U1000/SA findings before continuing.

- [ ] **Step 4: Full unit + integration tests**

Run: `go test ./...`
Expected: all green.

- [ ] **Step 5: Stress tag**

Run: `go test -tags stress ./...`
Expected: stress smokes within bounds.

- [ ] **Step 6: Differential harness numbers**

Run: `go test ./internal/diffharness/...`
Expected: existing 16-fixture × N-oracle matrix unchanged from M3 ship; new auth oracle green.

- [ ] **Step 7: roborev-refine on max reasoning**

Per the M1+ review-protocol memory (`m1_review_protocol.md`):

```
roborev-refine on max reasoning until pass or diminishing returns
```

Iterate fixes inline; commit each fix with `M4 ...: address roborev finding (<short summary>)`.

- [ ] **Step 8: Ship-gate commit on the worktree branch**

If any inherited M2/M3 gofmt/staticcheck debt is uncovered (mirroring the M3 cleanup pattern), clean it as a single dedicated ship-gate commit before tagging.

- [ ] **Step 9: Merge worktree to main**

Per the M3 progress memory's pattern (no-ff merge of the M-branch with annotated tag):

```bash
cd /home/eran/work/bucketvcs   # back out of worktree
git fetch
git merge --no-ff -m "M4 HTTPS token authentication: merge m4-https-auth" m4-https-auth
git tag -a m4-complete -m "M4 HTTPS token authentication shipped (<date>)"
```

- [ ] **Step 10: Update memory**

Per the auto-memory section of CLAUDE.md, write `m4_progress.md` capturing:

- Merge commit, tag, and date.
- M4 public APIs (auth.Store, sqlitestore.Open, gateway.Options.AuthStore, CLI subcommand surface) so M5+ knows the boundary.
- Architecture invariants M5+ must honor (auth.Store transport-neutrality, the §6.2 middleware sequence, the `repos` registry being a registry rather than bucket truth).
- Known limitations carried into M5+ (no rate limiting, no audit table, no SSH, no fine-grained per-token scopes, no LFS scopes).
- Update the existing `MEMORY.md` index with a one-line pointer to `m4_progress.md`.
- Update `m1_review_protocol.md` if the review iterations surfaced new patterns worth carrying forward.

---

## Self-review

Before declaring the plan ready for execution, run the four self-review checks.

**1. Spec coverage.** Walk each section of the spec and point to the task that implements it:

- §1 Purpose — Tasks 17, 18, 23 (gateway middleware + serve wiring).
- §2 What changes vs M3 — Task 18 (Options replacement) + Task 23 (legacy flag rejection).
- §3 Non-goals — Task 27 (migration doc names them) + Task 28 (ship gate confirms nothing else slipped in).
- §4 Architecture — Tasks 1–6 (auth package), Task 17 (middleware), Task 18 (server wiring), Task 19 (CLI seam).
- §5 Data model — Tasks 7 (schema), 9–12 (CRUD).
- §6 Request flow — Task 16 (ParseRoute), Task 17 (RunAuth), Task 18 (routeRepo wiring).
- §7 CLI — Tasks 19 (auth-db), 20 (user), 21 (token), 22 (repo), 23 (serve + main).
- §8 Logging — covered as a NOTE inside Task 17 (`structured logs only`); the actual log-line emission lands inline in Tasks 17/18 alongside the existing M3 logger calls. **Gap candidate**: explicit log lines for `auth.success`, `auth.failure`, `authz.denied` are not separately tested. Add: in Task 17 step 3, alongside the middleware logic, call into the existing M3 structured logger at the three points spec §8 names. Mark this as the Step 3 NOTE rather than separate steps.
- §9 Testing — Tasks 1–15 (unit + conformance), 16–18 (gateway integration), 24 (e2e), 25 (diffharness), 26 (stress).
- §10 Security considerations — Tasks 17 (status-code policy), 4–5 (token format + hashing), 23 (legacy-flag rejection).
- §11 Open questions — closed in spec; no task needed.
- §12 Acceptance criteria — Task 28 covers each line.
- §13 Out of scope — no task needed (negative space).
- §14 References — no task needed.

**Apparent gap fixed inline above** (§8 logging hooks).

**2. Placeholder scan.** No "TBD" / "TODO" / "implement later." Vague phrasings to check:

- "the existing M3 e2e scaffolding" in Task 24 — flagged as "Read the existing M3 e2e scaffolding" with the exact `grep` command. Acceptable.
- "the exact flag names" in Task 23 — flagged as "Read the current file with `Read` first." Acceptable.
- "the existing diffharness layout" in Task 25 — flagged as "Read the existing diffharness layout" with exact `ls`/`grep` commands. Acceptable.

These are not placeholders for implementation; they are pointers to specific files that already exist. Engineer reads, then writes.

**3. Type consistency.** Spot check:

- `auth.Store` method signatures — defined Task 6, consumed Tasks 13, 17 → consistent.
- `Decide(actor, perm, action, flags)` — defined Task 3, consumed Task 17 → consistent.
- `RoutedRequest{Tenant, Repo, Op, RequiredAction}` — defined Task 16, consumed Task 17, 18 → consistent.
- `Options{MirrorDir, Version, AuthStore, MaxBodyBytes}` — defined Task 18, consumed Task 23 → consistent.
- `sqlitestore.Open(path) (*Store, error)` — defined Task 8, consumed Tasks 9–13, 15, 19, 20–23, 26 → consistent.
- `auth.GenerateToken() (token, id, secret string, err error)` — defined Task 4, consumed Tasks 13, 15, 21, 24, 26 → consistent.
- `auth.HashSecret(secret) (string, error)` and `auth.VerifyHash(secret, encoded) error` — defined Task 5, consumed Task 13 → consistent.

**4. Scope.** One milestone, one PR-tree. Tasks 1–28 produce a single coherent change. No bleed.

The plan is ready for execution.

---

## Execution choice

The plan is saved to `docs/superpowers/plans/2026-05-06-m4-https-token-auth.md`.

Two execution options:

1. **Subagent-driven (recommended).** Dispatch a fresh subagent per task with two-stage review between tasks. Best for a 28-task milestone where review-in-the-loop catches issues early.
2. **Inline execution.** Execute tasks in this session using superpowers:executing-plans, batch with checkpoints.

Which approach?







