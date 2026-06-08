# M30: Build Triggers (CI Integration) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** On push, fire durable, ref-filtered HTTP requests that start GCP Cloud Build / AWS CodeBuild, and hand the build a short-lived, single-repo token to pull the code.

**Architecture:** A new `internal/buildtrigger` subsystem modeled on M15 webhooks (durable claim/backoff/dead-letter/replay worker), reusing M15's `Sign`/egress HTTP client, M16's `**`-aware ref matcher, and M22's repo-scoped short-lived token minting. Three trigger kinds — `generic` and `cloudbuild` (signed JSON POST) and `codebuild` (native SigV4 `StartBuild`) — behind a `Deliverer` interface. Per-repo triggers live in the authdb (DB+CLI + declarative `apply`); operator settings live in a scoped serve YAML. M15 webhooks are not modified.

**Tech Stack:** Go, sqlite/libsql/postgres (authdb), `aws-sdk-go-v2/service/codebuild` (new sub-module), `gopkg.in/yaml.v3` (new), OTel-via-slog metrics, slog audit.

**Reference files (read these before starting; the plan mirrors their shapes):**
- `internal/webhooks/service.go`, `worker.go`, `enqueue.go`, `sign.go`, `egress.go`, `metrics.go`, `audit.go` — the engine being mirrored.
- `internal/auth/sqlitestore/oidc.go` (`MintOIDCToken`, `SweepExpiredOIDCTokens`, `MintOIDCParams`, `oidcSystemUserID`) — token-minting + sweep pattern.
- `internal/auth/sqlitestore/store.go:354` (`CreateToken` signature).
- `internal/policy/match.go` (`MatchPath`, `ValidatePathPattern`) — the `**` matcher being reused.
- `internal/gitproto/receivepack/complete.go:560-595` (EventPush enqueue site) + `engine.go:17` (`EngineRequest`).
- `cmd/bucketvcs/webhook.go` (CLI shape), `cmd/bucketvcs/serve.go:673-705` (worker + sweep goroutine wiring).
- `internal/auth/scopes.go` (`ParseScopes`, `FormatScopes`, `TokenScope`, `ScopeRepoRead`, `ScopeLFSRead`).
- `internal/auth/sqlitestore/querier.go` (`Querier`, `Tx`, `RunInTx`, `SupportsSkipLocked`).

**Conventions to honor throughout:** errors as `var Err… = errors.New(...)` sentinels; metric emit = `slog LevelInfo "metric"` with `metric_name`; audit emit = `slog` with `slog.Bool("audit", true)` + `slog.String("event", …)`; CLI exit code 2 for usage errors, 1 for operational; NDJSON for `--format=json`; nil `*Service` is a no-op (optional-deps pattern); fail-open on enqueue.

---

## Task 1: Migration — `build_triggers` + `build_trigger_deliveries` + `_build` user

**Files:**
- Create: `internal/auth/sqlitestore/migrations/0017_build_triggers.sql`
- Create: `internal/auth/sqlitestore/migrations_postgres/0017_build_triggers.sql`
- Test: `internal/auth/sqlitestore/migration_buildtriggers_test.go`

- [ ] **Step 1: Write the failing test**

```go
package sqlitestore

import (
	"context"
	"testing"
)

func TestMigration0017_BuildTriggersTablesExist(t *testing.T) {
	s := newTestStore(t) // existing helper used by other sqlitestore tests
	ctx := context.Background()
	// Reserved _build user is present.
	var name string
	if err := s.db.QueryRowContext(ctx,
		`SELECT name FROM users WHERE id='_build'`).Scan(&name); err != nil {
		t.Fatalf("expected _build system user: %v", err)
	}
	// Tables accept a round-trip insert.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO build_triggers
		   (id,tenant,repo,name,kind,config_json,ref_include,ref_exclude,
		    token_mode,token_scopes,token_ttl_seconds,active,created_at)
		 VALUES ('bvbt_x','t','r','n','generic','{}','[]','[]','none',0,900,1,0)`,
	); err != nil {
		t.Fatalf("insert build_triggers: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO build_trigger_deliveries
		   (id,trigger_id,payload_json,status,attempts,next_attempt_at,created_at)
		 VALUES ('bvbd_x','bvbt_x','{}','pending',0,0,0)`,
	); err != nil {
		t.Fatalf("insert build_trigger_deliveries: %v", err)
	}
}
```

> Check `newTestStore`/field name `s.db` against an existing `*_test.go` in the package (e.g. `store_test.go`) and match the real helper; adjust if the accessor differs.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/auth/sqlitestore/ -run TestMigration0017 -v`
Expected: FAIL (no such table `build_triggers`).

- [ ] **Step 3: Write the sqlite migration**

`internal/auth/sqlitestore/migrations/0017_build_triggers.sql`:

```sql
CREATE TABLE build_triggers (
    id                TEXT PRIMARY KEY,
    tenant            TEXT NOT NULL,
    repo              TEXT NOT NULL,
    name              TEXT NOT NULL,
    kind              TEXT NOT NULL,
    config_json       BLOB NOT NULL,
    ref_include       BLOB NOT NULL,
    ref_exclude       BLOB NOT NULL,
    token_mode        TEXT NOT NULL,
    token_scopes      INTEGER NOT NULL,
    token_ttl_seconds INTEGER NOT NULL,
    active            INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0,1)),
    created_at        INTEGER NOT NULL,
    UNIQUE (tenant, repo, name),
    FOREIGN KEY (tenant, repo) REFERENCES repos(tenant, name) ON DELETE CASCADE
);
CREATE INDEX build_triggers_by_repo ON build_triggers (tenant, repo, active);

CREATE TABLE build_trigger_deliveries (
    id                TEXT PRIMARY KEY,
    trigger_id        TEXT NOT NULL,
    payload_json      BLOB NOT NULL,
    status            TEXT NOT NULL,
    attempts          INTEGER NOT NULL DEFAULT 0,
    next_attempt_at   INTEGER NOT NULL,
    last_attempt_at   INTEGER,
    last_status_code  INTEGER,
    last_error        TEXT,
    delivered_at      INTEGER,
    created_at        INTEGER NOT NULL
);
CREATE INDEX build_trigger_deliveries_claim ON build_trigger_deliveries (status, next_attempt_at);

INSERT INTO users (id, name, is_admin, created_at)
VALUES ('_build', '_build', 0, strftime('%s','now'));
```

- [ ] **Step 4: Write the postgres migration**

`internal/auth/sqlitestore/migrations_postgres/0017_build_triggers.sql`: same as above. Open `migrations_postgres/0006_webhooks.sql` and match its dialect choices (it uses identical `TEXT`/`INTEGER`/`BLOB`→`BYTEA` conventions for the existing tables). If the existing postgres migrations use `BYTEA` instead of `BLOB`, use `BYTEA` for `config_json`/`ref_include`/`ref_exclude`/`payload_json`. Keep the `users` insert as `strftime` → for postgres use `EXTRACT(EPOCH FROM now())::bigint` (copy the exact form used by `migrations_postgres/0010_oidc.sql`'s `_oidc` insert).

- [ ] **Step 5: Run test to verify it passes (sqlite)**

Run: `go test ./internal/auth/sqlitestore/ -run TestMigration0017 -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/auth/sqlitestore/migrations/0017_build_triggers.sql \
        internal/auth/sqlitestore/migrations_postgres/0017_build_triggers.sql \
        internal/auth/sqlitestore/migration_buildtriggers_test.go
git commit -m "feat(m30): migration 0017 build_triggers + _build system user"
```

---

## Task 2: Token minting — `MintBuildToken` + sweep (sqlitestore)

**Files:**
- Create: `internal/auth/sqlitestore/buildtoken.go`
- Modify: `internal/auth/sqlitestore/oidc.go` (refactor sweep into a shared helper — see Step 3)
- Test: `internal/auth/sqlitestore/buildtoken_test.go`

- [ ] **Step 1: Write the failing test**

```go
package sqlitestore

import (
	"context"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func TestMintBuildToken_ScopedReadOnly(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedRepo(t, s, "acme", "app") // use the package's existing repo-seed helper
	tok, err := s.MintBuildToken(ctx, MintBuildParams{
		Tenant: "acme", Repo: "app",
		Scopes: auth.ScopeRepoRead | auth.ScopeLFSRead,
		TTLSeconds: 900, Label: "build:acme/app:main",
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if tok == "" {
		t.Fatal("empty token")
	}
	// Verifiable as a normal credential bound to the repo, read-only.
	id := auth.TokenID(tok) // existing helper that extracts the id prefix
	row, err := s.GetTokenByID(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.UserID != "_build" || row.ScopeTenant != "acme" || row.ScopeRepo != "app" || row.ScopePerm != "read" {
		t.Fatalf("unexpected scope binding: %+v", row)
	}
}

func TestSweepExpiredBuildTokens_OnlyBuildUser(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedRepo(t, s, "acme", "app")
	past := time.Now().Add(-time.Hour).Unix()
	// Expired _build token.
	mustExec(t, s, `INSERT INTO tokens (id,user_id,secret_hash,created_at,expires_at,scopes)
	                VALUES ('bt1','_build','h',0,?,0)`, past)
	// Expired ordinary user token must NOT be swept.
	seedUser(t, s, "u1")
	mustExec(t, s, `INSERT INTO tokens (id,user_id,secret_hash,created_at,expires_at,scopes)
	                VALUES ('ut1','u1','h',0,?,0)`, past)
	n, err := s.SweepExpiredBuildTokens(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
	if _, err := s.GetTokenByID(ctx, "ut1"); err != nil {
		t.Fatalf("ordinary token wrongly swept: %v", err)
	}
}
```

> Verify helper names (`seedRepo`, `seedUser`, `mustExec`, `auth.TokenID`) against the package's existing tests; substitute the real ones. If `auth.TokenID` does not exist, parse the id as the substring before the second `_` the same way `oidc_test.go` does.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/auth/sqlitestore/ -run 'TestMintBuildToken|TestSweepExpiredBuildTokens' -v`
Expected: FAIL (undefined `MintBuildParams`/`MintBuildToken`/`SweepExpiredBuildTokens`).

- [ ] **Step 3: Implement (mirror the OIDC versions)**

`internal/auth/sqlitestore/buildtoken.go`:

```go
package sqlitestore

import (
	"context"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// buildSystemUserID is the reserved user inserted by migration 0017. Build
// triggers mint short-lived repo-bound tokens under it, mirroring the M22
// _oidc pattern.
const buildSystemUserID = "_build"

// MintBuildParams is the input to MintBuildToken.
type MintBuildParams struct {
	Tenant     string
	Repo       string
	Scopes     auth.TokenScope
	TTLSeconds int64
	Label      string // "build:<tenant>/<repo>:<trigger-name>"
}

// MintBuildToken creates a short-lived, single-repo, read-only bvts token
// under the _build system user and returns the wire-format token string.
// The secret is shown only here (stored hashed). Read-only by construction:
// scope_perm is always "read" (builds pull, never push).
func (s *Store) MintBuildToken(ctx context.Context, p MintBuildParams) (string, error) {
	token, id, secret, err := auth.GenerateToken()
	if err != nil {
		return "", err
	}
	hash, err := auth.HashSecret(secret)
	if err != nil {
		return "", err
	}
	exp := time.Now().Unix() + p.TTLSeconds
	if err := s.CreateToken(ctx, id, buildSystemUserID, hash, p.Label, &exp,
		p.Scopes, p.Tenant, p.Repo, "read"); err != nil {
		return "", err
	}
	return token, nil
}

// SweepExpiredBuildTokens deletes expired tokens owned by the _build system
// user and returns the count. Scoped to _build so it never touches
// operator-managed user tokens.
func (s *Store) SweepExpiredBuildTokens(ctx context.Context) (int64, error) {
	return s.sweepExpiredTokensForUser(ctx, buildSystemUserID)
}
```

Refactor the shared delete out of `oidc.go`. In `oidc.go`, replace the body of `SweepExpiredOIDCTokens` with:

```go
func (s *Store) SweepExpiredOIDCTokens(ctx context.Context) (int64, error) {
	return s.sweepExpiredTokensForUser(ctx, oidcSystemUserID)
}
```

and add the shared helper (put it in `buildtoken.go` next to its first build-side caller, or in `store.go`):

```go
// sweepExpiredTokensForUser deletes expired tokens owned by a single reserved
// system user. Shared by the _oidc (M22) and _build (M30) sweeps so the two
// never diverge; scoping by user_id keeps operator tokens untouched.
func (s *Store) sweepExpiredTokensForUser(ctx context.Context, userID string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM tokens WHERE user_id = ? AND expires_at IS NOT NULL AND expires_at < ?`,
		userID, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/auth/sqlitestore/ -run 'TestMintBuildToken|TestSweepExpiredBuildTokens|OIDCToken' -v`
Expected: PASS (including the existing OIDC sweep tests — the refactor must not break them).

- [ ] **Step 5: Commit**

```bash
git add internal/auth/sqlitestore/buildtoken.go internal/auth/sqlitestore/oidc.go \
        internal/auth/sqlitestore/buildtoken_test.go
git commit -m "feat(m30): MintBuildToken + _build sweep (shared sweep helper)"
```

---

## Task 3: Types + ref matcher (`internal/buildtrigger`)

**Files:**
- Create: `internal/buildtrigger/doc.go`
- Create: `internal/buildtrigger/types.go`
- Create: `internal/buildtrigger/match.go`
- Test: `internal/buildtrigger/match_test.go`

- [ ] **Step 1: Write the failing test**

```go
package buildtrigger

import "testing"

func TestRefMatches(t *testing.T) {
	cases := []struct {
		name             string
		include, exclude []string
		ref              string
		want             bool
	}{
		{"empty include = all", nil, nil, "refs/heads/main", true},
		{"exact include", []string{"refs/heads/main"}, nil, "refs/heads/main", true},
		{"non-match include", []string{"refs/heads/main"}, nil, "refs/heads/dev", false},
		{"single-seg glob", []string{"refs/heads/release/*"}, nil, "refs/heads/release/1.0", true},
		{"single-seg glob too deep", []string{"refs/heads/release/*"}, nil, "refs/heads/release/a/b", false},
		{"tag glob", []string{"refs/tags/v*"}, nil, "refs/tags/v1.2.3", true},
		{"exclude wins", []string{"refs/heads/**"}, []string{"refs/heads/dependabot/**"}, "refs/heads/dependabot/x", false},
		{"exclude subtree, sibling passes", []string{"refs/heads/**"}, []string{"refs/heads/dependabot/**"}, "refs/heads/main", true},
		{"empty include + exclude", nil, []string{"refs/tags/**"}, "refs/tags/v1", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := RefMatches(c.include, c.exclude, c.ref)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if got != c.want {
				t.Fatalf("RefMatches(%v,%v,%q)=%v want %v", c.include, c.exclude, c.ref, got, c.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/buildtrigger/ -run TestRefMatches -v`
Expected: FAIL (no package / undefined `RefMatches`).

- [ ] **Step 3: Implement types + matcher**

`internal/buildtrigger/doc.go`:

```go
// Package buildtrigger fires durable, ref-filtered HTTP requests on push to
// start CI builds (GCP Cloud Build, AWS CodeBuild), optionally minting a
// short-lived, single-repo pull token. It mirrors the M15 webhooks delivery
// engine (claim/backoff/dead-letter/replay) and reuses M16's `**`-aware ref
// matcher and M22's repo-scoped token minting. M15 webhooks are untouched.
package buildtrigger
```

`internal/buildtrigger/types.go`:

```go
package buildtrigger

import (
	"errors"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// Kind identifies how a trigger delivers.
type Kind string

const (
	KindGeneric   Kind = "generic"   // signed JSON POST to an arbitrary URL
	KindCloudBuild Kind = "cloudbuild" // generic POST, Cloud-Build-shaped default body
	KindCodeBuild Kind = "codebuild"  // native SigV4 StartBuild
)

// TokenMode controls short-lived token injection.
type TokenMode string

const (
	TokenNone   TokenMode = "none"   // OIDC-pull (default); no credential in the trigger
	TokenInject TokenMode = "inject" // mint a bvts and inject it into the delivery
)

// Sentinels (mirror webhooks.Err*). CLI maps ErrInvalidInput → exit 2.
var (
	ErrNotFound     = errors.New("buildtrigger: not found")
	ErrConflict     = errors.New("buildtrigger: trigger already exists")
	ErrInvalidInput = errors.New("buildtrigger: invalid input")
	ErrReplayInFlight = errors.New("buildtrigger: delivery is in_flight; wait for the attempt to finish")
)

// Trigger is the canonical view returned to operators. Secret is populated
// ONLY by Create (returned once); List/Get return SecretPreview.
type Trigger struct {
	ID            string
	Tenant        string
	Repo          string
	Name          string
	Kind          Kind
	Config        Config
	RefInclude    []string
	RefExclude    []string
	TokenMode     TokenMode
	TokenScopes   auth.TokenScope
	TokenTTL      time.Duration
	Active        bool
	CreatedAt     time.Time
	SecretPreview string // first 6 chars of generic/cloudbuild HMAC secret
}

// Config is the kind-specific configuration stored as config_json.
// Generic/cloudbuild use URL+Secret; codebuild uses the AWS fields.
type Config struct {
	URL          string `json:"url,omitempty"`           // generic/cloudbuild
	Secret       string `json:"secret,omitempty"`        // generic/cloudbuild HMAC secret
	AWSRegion    string `json:"aws_region,omitempty"`    // codebuild
	AWSProject   string `json:"aws_project,omitempty"`   // codebuild
	AWSConnector string `json:"aws_connector,omitempty"` // codebuild; names a serve-config connector
}

// TriggerInput is the operator-supplied data for Create. For generic/cloudbuild,
// Secret is generated server-side when empty.
type TriggerInput struct {
	Tenant      string
	Repo        string
	Name        string
	Kind        Kind
	Config      Config
	RefInclude  []string
	RefExclude  []string
	TokenMode   TokenMode
	TokenScopes auth.TokenScope
	TokenTTL    time.Duration
}

// BuildPayload is the snapshot enqueued per matching trigger and rendered by
// the deliverer. It is the raw build context; token injection happens at
// delivery time (the token must be fresh, not stored in the queue row).
type BuildPayload struct {
	Tenant    string      `json:"tenant"`
	Repo      string      `json:"repo"`
	Actor     string      `json:"actor"`
	TxID      string      `json:"tx_id"`
	HeadOID   string      `json:"head_oid"`
	RefUpdate RefUpdate   `json:"ref_update"` // the specific ref that matched
}

// RefUpdate mirrors webhooks.RefUpdate (old/new "0000..." conventions).
type RefUpdate struct {
	Refname string `json:"refname"`
	OldOID  string `json:"old_oid"`
	NewOID  string `json:"new_oid"`
}

// TokenCeiling is the hard upper bound on a trigger's token TTL (reuses M22's
// 1h ceiling). Enforced at Create.
const TokenCeiling = time.Hour
```

`internal/buildtrigger/match.go`:

```go
package buildtrigger

import "github.com/bucketvcs/bucketvcs/internal/policy"

// RefMatches reports whether ref should fire a trigger with the given include
// and exclude glob lists. Rule: fire if (include is empty OR any include
// matches) AND no exclude matches. Exclude wins. Globs use the M16 `**`-aware
// matcher (policy.MatchPath), evaluated against the full refname
// (e.g. "refs/heads/main").
func RefMatches(include, exclude []string, ref string) (bool, error) {
	for _, pat := range exclude {
		ok, err := policy.MatchPath(pat, ref)
		if err != nil {
			return false, err
		}
		if ok {
			return false, nil
		}
	}
	if len(include) == 0 {
		return true, nil
	}
	for _, pat := range include {
		ok, err := policy.MatchPath(pat, ref)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}
```

> Note: `policy.MatchPath` rejects an empty path and validates the pattern; a malformed glob returns an error (surfaced at Create-time validation in Task 4, so runtime never sees a bad pattern).

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/buildtrigger/ -run TestRefMatches -v`
Expected: PASS (all 9 cases).

- [ ] **Step 5: Commit**

```bash
git add internal/buildtrigger/doc.go internal/buildtrigger/types.go \
        internal/buildtrigger/match.go internal/buildtrigger/match_test.go
git commit -m "feat(m30): buildtrigger types + M16-reuse ref matcher"
```

---

## Task 4: Store CRUD (`internal/buildtrigger`)

**Files:**
- Create: `internal/buildtrigger/store.go`
- Test: `internal/buildtrigger/store_test.go`

- [ ] **Step 1: Write the failing test**

```go
package buildtrigger

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func TestStore_CreateListGetRemove(t *testing.T) {
	svc, db := newTestSvc(t) // helper: opens an authdb with migrations, seeds repo acme/app
	_ = db
	ctx := context.Background()
	tr, err := svc.Create(ctx, TriggerInput{
		Tenant: "acme", Repo: "app", Name: "main-cb", Kind: KindCloudBuild,
		Config:      Config{URL: "https://cloudbuild.example/x:webhook?key=k&secret=s"},
		RefInclude:  []string{"refs/heads/main"},
		TokenMode:   TokenNone,
		TokenScopes: auth.ScopeRepoRead | auth.ScopeLFSRead,
		TokenTTL:    15 * time.Minute,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if tr.Secret == "" {
		t.Fatal("expected generated secret on create")
	}
	got, err := svc.List(ctx, "acme", "app")
	if err != nil || len(got) != 1 {
		t.Fatalf("list: %v len=%d", err, len(got))
	}
	if got[0].Secret != "" || got[0].SecretPreview == "" {
		t.Fatal("list must hide secret, show preview")
	}
	// Conflict on duplicate (tenant,repo,name).
	if _, err := svc.Create(ctx, TriggerInput{
		Tenant: "acme", Repo: "app", Name: "main-cb", Kind: KindGeneric,
		Config: Config{URL: "https://x"},
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
	// Disable then remove.
	if err := svc.Disable(ctx, tr.ID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := svc.Remove(ctx, tr.ID); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := svc.Remove(ctx, tr.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestStore_CreateValidation(t *testing.T) {
	svc, _ := newTestSvc(t)
	ctx := context.Background()
	bad := []TriggerInput{
		{Tenant: "", Repo: "app", Name: "n", Kind: KindGeneric, Config: Config{URL: "https://x"}},
		{Tenant: "acme", Repo: "app", Name: "n", Kind: "bogus", Config: Config{URL: "https://x"}},
		{Tenant: "acme", Repo: "app", Name: "n", Kind: KindGeneric, Config: Config{URL: ""}},                       // generic needs URL
		{Tenant: "acme", Repo: "app", Name: "n", Kind: KindCodeBuild, Config: Config{AWSProject: ""}},              // codebuild needs project+region
		{Tenant: "acme", Repo: "app", Name: "n", Kind: KindGeneric, Config: Config{URL: "https://x"}, RefInclude: []string{"["}}, // bad glob
		{Tenant: "acme", Repo: "app", Name: "n", Kind: KindGeneric, Config: Config{URL: "https://x"}, TokenTTL: 2 * time.Hour},   // over ceiling
		{Tenant: "acme", Repo: "app", Name: "bad name", Kind: KindGeneric, Config: Config{URL: "https://x"}},       // bad name
	}
	for i, in := range bad {
		if _, err := svc.Create(ctx, in); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("case %d: want ErrInvalidInput, got %v", i, err)
		}
	}
}
```

> Implement `newTestSvc(t)` in `store_test.go` by copying the authdb-open + repo-seed pattern from `internal/webhooks/service_test.go` (it opens a `sqlitestore` with migrations and seeds a repo). `New(db sqlitestore.Querier)` mirrors `webhooks.New`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/buildtrigger/ -run TestStore -v`
Expected: FAIL (undefined `New`/`Create`/...).

- [ ] **Step 3: Implement the Store**

`internal/buildtrigger/store.go`:

```go
package buildtrigger

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/gateway/routenames"
)

// Service manages build triggers + their delivery queue against the authdb.
type Service struct {
	db sqlitestore.Querier
}

// New constructs a Service backed by the given authdb handle.
func New(db sqlitestore.Querier) *Service { return &Service{db: db} }

func newID(prefix string) (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func genSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func validKind(k Kind) bool {
	return k == KindGeneric || k == KindCloudBuild || k == KindCodeBuild
}

// Create validates and inserts a trigger, generating a secret for
// generic/cloudbuild when none is supplied. The returned Trigger carries the
// full Secret exactly once.
func (s *Service) Create(ctx context.Context, in TriggerInput) (Trigger, error) {
	if in.Tenant == "" || in.Repo == "" || in.Name == "" {
		return Trigger{}, fmt.Errorf("%w: tenant/repo/name required", ErrInvalidInput)
	}
	if !routenames.ValidateName(in.Name) {
		return Trigger{}, fmt.Errorf("%w: invalid name %q", ErrInvalidInput, in.Name)
	}
	if !validKind(in.Kind) {
		return Trigger{}, fmt.Errorf("%w: invalid kind %q", ErrInvalidInput, in.Kind)
	}
	if in.TokenTTL < 0 || in.TokenTTL > TokenCeiling {
		return Trigger{}, fmt.Errorf("%w: token-ttl must be 0 < ttl <= %s", ErrInvalidInput, TokenCeiling)
	}
	if in.TokenTTL == 0 {
		in.TokenTTL = 15 * time.Minute
	}
	if in.TokenMode == "" {
		in.TokenMode = TokenNone
		if in.Kind == KindCodeBuild {
			in.TokenMode = TokenInject // AWS can't OIDC-pull; default to inject
		}
	}
	if in.TokenMode != TokenNone && in.TokenMode != TokenInject {
		return Trigger{}, fmt.Errorf("%w: invalid token-mode %q", ErrInvalidInput, in.TokenMode)
	}
	if in.TokenScopes == 0 {
		in.TokenScopes = auth.ScopeRepoRead | auth.ScopeLFSRead
	}
	// Validate ref globs up front (so the worker never hits a bad pattern).
	for _, pats := range [][]string{in.RefInclude, in.RefExclude} {
		for _, p := range pats {
			if err := policyValidate(p); err != nil {
				return Trigger{}, fmt.Errorf("%w: bad ref pattern %q: %v", ErrInvalidInput, p, err)
			}
		}
	}
	// Kind-specific config validation + secret generation.
	switch in.Kind {
	case KindGeneric, KindCloudBuild:
		if in.Config.URL == "" {
			return Trigger{}, fmt.Errorf("%w: %s requires --url", ErrInvalidInput, in.Kind)
		}
		if in.Config.Secret == "" {
			sec, err := genSecret()
			if err != nil {
				return Trigger{}, err
			}
			in.Config.Secret = sec
		}
	case KindCodeBuild:
		if in.Config.AWSProject == "" || in.Config.AWSRegion == "" {
			return Trigger{}, fmt.Errorf("%w: codebuild requires --aws-region and --aws-project", ErrInvalidInput)
		}
	}

	id, err := newID("bvbt_")
	if err != nil {
		return Trigger{}, err
	}
	cfgJSON, _ := json.Marshal(in.Config)
	incJSON, _ := json.Marshal(nonNil(in.RefInclude))
	excJSON, _ := json.Marshal(nonNil(in.RefExclude))
	now := time.Now()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO build_triggers
		   (id,tenant,repo,name,kind,config_json,ref_include,ref_exclude,
		    token_mode,token_scopes,token_ttl_seconds,active,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,1,?)`,
		id, in.Tenant, in.Repo, in.Name, string(in.Kind), cfgJSON, incJSON, excJSON,
		string(in.TokenMode), int64(in.TokenScopes), int64(in.TokenTTL.Seconds()), now.Unix())
	if err != nil {
		if s.isUnique(err) {
			return Trigger{}, ErrConflict
		}
		return Trigger{}, fmt.Errorf("buildtrigger: insert: %w", err)
	}
	out := triggerFromInput(id, in, now)
	out.Secret = in.Config.Secret // shown once
	return out, nil
}
```

Add the remaining methods to the same file:

```go
// List returns triggers for (tenant, repo) with Secret hidden (SecretPreview only).
func (s *Service) List(ctx context.Context, tenant, repo string) ([]Trigger, error) { /* SELECT … ORDER BY name */ }

// Get returns one trigger by id (Secret hidden). ErrNotFound when absent.
func (s *Service) Get(ctx context.Context, id string) (Trigger, error) { /* SELECT … WHERE id=? */ }

// Remove deletes a trigger. ErrNotFound when the id does not exist.
func (s *Service) Remove(ctx context.Context, id string) error { /* DELETE … RowsAffected==0 → ErrNotFound */ }

// Enable / Disable flip the active flag. ErrNotFound when absent.
func (s *Service) Enable(ctx context.Context, id string) error  { return s.setActive(ctx, id, true) }
func (s *Service) Disable(ctx context.Context, id string) error { return s.setActive(ctx, id, false) }
```

Implement the helpers (`scanTrigger` decoding config_json + ref arrays + `auth.TokenScope(scopes)` + `time.Duration(ttl)*time.Second` + 6-char `SecretPreview` from the decoded `Config.Secret`; `setActive` with `RowsAffected==0 → ErrNotFound`; `nonNil([]string) []string` returning `[]string{}` for nil so JSON encodes `[]`; `triggerFromInput`; `isUnique` delegating to the backend — copy `webhooks.Service`'s unique-violation detection or call `s.db`-side helper). Add `policyValidate` as a thin wrapper:

```go
func policyValidate(pattern string) error { return policy.ValidatePathPattern(pattern) }
```

(add the `internal/policy` import).

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/buildtrigger/ -run TestStore -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/buildtrigger/store.go internal/buildtrigger/store_test.go
git commit -m "feat(m30): buildtrigger Store CRUD + validation"
```

---

## Task 5: Enqueue on push (`internal/buildtrigger`)

**Files:**
- Create: `internal/buildtrigger/enqueue.go`
- Test: `internal/buildtrigger/enqueue_test.go`

- [ ] **Step 1: Write the failing test**

```go
package buildtrigger

import (
	"context"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func TestEnqueue_RefFilter(t *testing.T) {
	svc, _ := newTestSvc(t)
	ctx := context.Background()
	mustCreate(t, svc, TriggerInput{
		Tenant: "acme", Repo: "app", Name: "main", Kind: KindGeneric,
		Config: Config{URL: "https://x"}, RefInclude: []string{"refs/heads/main"},
		TokenScopes: auth.ScopeRepoRead,
	})
	push := PushInfo{
		Tenant: "acme", Repo: "app", Actor: "u", TxID: "tx1", HeadOID: "abc",
		RefUpdates: []RefUpdate{
			{Refname: "refs/heads/main", OldOID: "0", NewOID: "abc"},
			{Refname: "refs/heads/dev", OldOID: "0", NewOID: "def"},
		},
	}
	if err := svc.Enqueue(ctx, push); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	rows := svc.pendingForTest(ctx, t)
	if len(rows) != 1 {
		t.Fatalf("got %d deliveries, want 1 (only main matched)", len(rows))
	}
}

func TestEnqueue_NilServiceNoop(t *testing.T) {
	var svc *Service
	if err := svc.Enqueue(context.Background(), PushInfo{}); err != nil {
		t.Fatalf("nil enqueue must be no-op, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/buildtrigger/ -run TestEnqueue -v`
Expected: FAIL (undefined `PushInfo`/`Enqueue`).

- [ ] **Step 3: Implement Enqueue**

`internal/buildtrigger/enqueue.go`:

```go
package buildtrigger

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// PushInfo is the per-push context handed to Enqueue from receive-pack.
type PushInfo struct {
	Tenant     string
	Repo       string
	Actor      string
	TxID       string
	HeadOID    string
	RefUpdates []RefUpdate
}

// Enqueue inserts one build_trigger_deliveries row for each (active trigger,
// matching ref) pair. One delivery per matching ref keeps the build context
// (which ref changed) precise. A nil *Service is a no-op. Insert errors are
// returned; the caller fails open (audits and continues the push).
func (s *Service) Enqueue(ctx context.Context, push PushInfo) error {
	if s == nil {
		return nil
	}
	triggers, err := s.listActiveForRepo(ctx, push.Tenant, push.Repo)
	if err != nil {
		return fmt.Errorf("buildtrigger: enqueue lookup: %w", err)
	}
	if len(triggers) == 0 {
		return nil
	}
	now := time.Now()
	for _, tr := range triggers {
		for _, ru := range push.RefUpdates {
			ok, merr := RefMatches(tr.RefInclude, tr.RefExclude, ru.Refname)
			if merr != nil || !ok {
				continue // patterns were validated at Create; merr should not occur
			}
			id, err := newID("bvbd_")
			if err != nil {
				return err
			}
			payload := BuildPayload{
				Tenant: push.Tenant, Repo: push.Repo, Actor: push.Actor,
				TxID: push.TxID, HeadOID: push.HeadOID, RefUpdate: ru,
			}
			body, err := json.Marshal(payload)
			if err != nil {
				return fmt.Errorf("buildtrigger: marshal payload: %w", err)
			}
			if _, err := s.db.ExecContext(ctx,
				`INSERT INTO build_trigger_deliveries
				   (id,trigger_id,payload_json,status,attempts,next_attempt_at,created_at)
				 VALUES (?,?,?,'pending',0,?,?)`,
				id, tr.ID, body, now.Unix(), now.Unix()); err != nil {
				return fmt.Errorf("buildtrigger: insert delivery: %w", err)
			}
		}
	}
	return nil
}
```

Add `listActiveForRepo` (SELECT full trigger rows WHERE tenant=? AND repo=? AND active=1, decoded via `scanTrigger`) and a test-only `pendingForTest` (SELECT id,trigger_id,status FROM build_trigger_deliveries WHERE status IN ('pending','in_flight')). Add `mustCreate` test helper in `enqueue_test.go`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/buildtrigger/ -run TestEnqueue -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/buildtrigger/enqueue.go internal/buildtrigger/enqueue_test.go
git commit -m "feat(m30): buildtrigger Enqueue with per-ref filtering"
```

---

## Task 6: Payload rendering + token injection (`internal/buildtrigger`)

**Files:**
- Create: `internal/buildtrigger/render.go`
- Test: `internal/buildtrigger/render_test.go`

- [ ] **Step 1: Write the failing test**

```go
package buildtrigger

import (
	"encoding/json"
	"testing"
)

func TestRenderBody_GenericIncludesContext(t *testing.T) {
	p := BuildPayload{Tenant: "acme", Repo: "app", HeadOID: "abc",
		RefUpdate: RefUpdate{Refname: "refs/heads/main"}}
	body, err := RenderBody(KindGeneric, p, "") // no token
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	if m["repo"] != "app" || m["head_oid"] != "abc" {
		t.Fatalf("missing context: %v", m)
	}
	if _, ok := m["bvts_token"]; ok {
		t.Fatal("token must be absent when not injected")
	}
}

func TestRenderBody_CloudBuildInjectsToken(t *testing.T) {
	p := BuildPayload{Tenant: "acme", Repo: "app", RefUpdate: RefUpdate{Refname: "refs/heads/main"}}
	body, err := RenderBody(KindCloudBuild, p, "bvts_secret")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	if m["bvts_token"] != "bvts_secret" {
		t.Fatalf("expected injected token, got %v", m["bvts_token"])
	}
	// Cloud Build preset exposes a flat ref name for $(body.ref) mapping.
	if m["ref"] != "refs/heads/main" {
		t.Fatalf("expected flat ref, got %v", m["ref"])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/buildtrigger/ -run TestRenderBody -v`
Expected: FAIL (undefined `RenderBody`).

- [ ] **Step 3: Implement**

`internal/buildtrigger/render.go`:

```go
package buildtrigger

import "encoding/json"

// RenderBody produces the JSON POST body for generic/cloudbuild deliveries.
// token is injected as "bvts_token" only when non-empty (TokenInject path).
// The cloudbuild preset additionally flattens commonly-referenced fields
// (ref, commit, repo) to top level so Cloud Build's $(body.ref) substitution
// mapping is ergonomic.
func RenderBody(kind Kind, p BuildPayload, token string) ([]byte, error) {
	m := map[string]any{
		"tenant":    p.Tenant,
		"repo":      p.Repo,
		"actor":     p.Actor,
		"tx_id":     p.TxID,
		"head_oid":  p.HeadOID,
		"ref_update": p.RefUpdate,
	}
	if kind == KindCloudBuild {
		m["ref"] = p.RefUpdate.Refname
		m["commit"] = p.RefUpdate.NewOID
	}
	if token != "" {
		m["bvts_token"] = token
	}
	return json.Marshal(m)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/buildtrigger/ -run TestRenderBody -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/buildtrigger/render.go internal/buildtrigger/render_test.go
git commit -m "feat(m30): build payload rendering + token injection"
```

---

## Task 7: Deliverer interface + generic/cloudbuild HTTP deliverer

**Files:**
- Create: `internal/buildtrigger/deliver.go`
- Test: `internal/buildtrigger/deliver_test.go`

- [ ] **Step 1: Write the failing test**

```go
package buildtrigger

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

func TestHTTPDeliverer_PostsSignedBodyWithToken(t *testing.T) {
	var gotSig, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get("BucketVCS-Signature")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	tr := Trigger{Kind: KindGeneric, TokenMode: TokenInject,
		Config: Config{URL: srv.URL, Secret: "shh"}}
	p := BuildPayload{Tenant: "acme", Repo: "app", RefUpdate: RefUpdate{Refname: "refs/heads/main"}}

	d := &httpDeliverer{client: srv.Client(), mintFn: func(context.Context, Trigger, BuildPayload) (string, error) {
		return "bvts_minted", nil
	}}
	code, err := d.Deliver(context.Background(), tr, p)
	if err != nil || code != 200 {
		t.Fatalf("deliver: code=%d err=%v", code, err)
	}
	if gotSig == "" || !strings.HasPrefix(gotSig, "t=") {
		t.Fatalf("missing/invalid signature: %q", gotSig)
	}
	if !strings.Contains(gotBody, `"bvts_token":"bvts_minted"`) {
		t.Fatalf("token not injected: %s", gotBody)
	}
	// Signature must validate over the exact posted body.
	// (Re-sign with same secret + the t embedded in the header is covered by webhooks.Sign tests;
	// here we just assert the header shape.)
	_ = webhooks.Sign
}

func TestHTTPDeliverer_NoTokenWhenModeNone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "bvts_token") {
			t.Errorf("token must be absent in TokenNone mode: %s", b)
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()
	tr := Trigger{Kind: KindGeneric, TokenMode: TokenNone, Config: Config{URL: srv.URL, Secret: "s"}}
	d := &httpDeliverer{client: srv.Client(), mintFn: func(context.Context, Trigger, BuildPayload) (string, error) {
		t.Fatal("mint must not be called in TokenNone mode")
		return "", nil
	}}
	if code, err := d.Deliver(context.Background(), tr, BuildPayload{Repo: "app"}); err != nil || code != 204 {
		t.Fatalf("code=%d err=%v", code, err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/buildtrigger/ -run TestHTTPDeliverer -v`
Expected: FAIL (undefined `httpDeliverer`).

- [ ] **Step 3: Implement the interface + HTTP deliverer**

`internal/buildtrigger/deliver.go`:

```go
package buildtrigger

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

// Deliverer performs one attempt to start a build. Implementations are chosen
// by trigger kind. A non-nil error means the attempt failed and should be
// retried per the backoff schedule; statusCode is advisory (HTTP code or 0).
type Deliverer interface {
	Deliver(ctx context.Context, tr Trigger, p BuildPayload) (statusCode int, err error)
}

// MintFunc mints a fresh short-lived bvts for a trigger at delivery time.
type MintFunc func(ctx context.Context, tr Trigger, p BuildPayload) (string, error)

// httpDeliverer handles KindGeneric and KindCloudBuild (signed JSON POST).
type httpDeliverer struct {
	client *http.Client
	mintFn MintFunc
}

func (d *httpDeliverer) Deliver(ctx context.Context, tr Trigger, p BuildPayload) (int, error) {
	if !strings.HasPrefix(tr.Config.URL, "http://") && !strings.HasPrefix(tr.Config.URL, "https://") {
		return 0, fmt.Errorf("egress denied: trigger URL scheme must be http or https")
	}
	var token string
	if tr.TokenMode == TokenInject {
		t, err := d.mintFn(ctx, tr, p)
		if err != nil {
			return 0, fmt.Errorf("mint token: %w", err) // retryable
		}
		token = t
	}
	body, err := RenderBody(tr.Kind, p, token)
	if err != nil {
		return 0, fmt.Errorf("render body: %w", err)
	}
	now := time.Now().Unix()
	sig := webhooks.Sign(tr.Config.Secret, now, body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tr.Config.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("BucketVCS-Signature", sig)
	req.Header.Set("User-Agent", "bucketvcs-buildtrigger/1")
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.CopyN(io.Discard, resp.Body, 512)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/buildtrigger/ -run TestHTTPDeliverer -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/buildtrigger/deliver.go internal/buildtrigger/deliver_test.go
git commit -m "feat(m30): Deliverer interface + generic/cloudbuild HTTP deliverer"
```

---

## Task 8: CodeBuild deliverer (SigV4 StartBuild)

**Files:**
- Create: `internal/buildtrigger/codebuild.go`
- Test: `internal/buildtrigger/codebuild_test.go`
- Modify: `go.mod` / `go.sum` (add `aws-sdk-go-v2/service/codebuild`)

- [ ] **Step 1: Add the dependency**

Run: `go get github.com/aws/aws-sdk-go-v2/service/codebuild@latest`
Expected: `go.mod` gains the module (same SDK family already present).

- [ ] **Step 2: Write the failing test**

```go
package buildtrigger

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/codebuild"
	cbtypes "github.com/aws/aws-sdk-go-v2/service/codebuild/types"
)

type fakeStartBuild struct {
	in *codebuild.StartBuildInput
}

func (f *fakeStartBuild) StartBuild(ctx context.Context, in *codebuild.StartBuildInput, _ ...func(*codebuild.Options)) (*codebuild.StartBuildOutput, error) {
	f.in = in
	return &codebuild.StartBuildOutput{Build: &cbtypes.Build{Id: strptr("b-1")}}, nil
}
func strptr(s string) *string { return &s }

func TestCodeBuildDeliverer_StartBuildInputs(t *testing.T) {
	fake := &fakeStartBuild{}
	d := &codeBuildDeliverer{
		clientFor: func(Trigger) (startBuildAPI, error) { return fake, nil },
		mintFn:    func(context.Context, Trigger, BuildPayload) (string, error) { return "bvts_x", nil },
	}
	tr := Trigger{Kind: KindCodeBuild, TokenMode: TokenInject,
		Config: Config{AWSRegion: "us-east-1", AWSProject: "app-release"}}
	p := BuildPayload{Tenant: "acme", Repo: "app", HeadOID: "abc123",
		RefUpdate: RefUpdate{Refname: "refs/tags/v1", NewOID: "abc123"}}
	if _, err := d.Deliver(context.Background(), tr, p); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if fake.in == nil || *fake.in.ProjectName != "app-release" || *fake.in.SourceVersion != "abc123" {
		t.Fatalf("bad StartBuild input: %+v", fake.in)
	}
	var sawToken, sawRef bool
	for _, ev := range fake.in.EnvironmentVariablesOverride {
		if *ev.Name == "BVTS_TOKEN" && *ev.Value == "bvts_x" {
			sawToken = true
		}
		if *ev.Name == "BV_REF" && *ev.Value == "refs/tags/v1" {
			sawRef = true
		}
	}
	if !sawToken || !sawRef {
		t.Fatalf("missing env overrides: token=%v ref=%v", sawToken, sawRef)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/buildtrigger/ -run TestCodeBuildDeliverer -v`
Expected: FAIL (undefined `codeBuildDeliverer`/`startBuildAPI`).

- [ ] **Step 4: Implement**

`internal/buildtrigger/codebuild.go`:

```go
package buildtrigger

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/codebuild"
	cbtypes "github.com/aws/aws-sdk-go-v2/service/codebuild/types"
)

// startBuildAPI is the slice of the CodeBuild client we use (one method),
// so tests can fake it.
type startBuildAPI interface {
	StartBuild(ctx context.Context, in *codebuild.StartBuildInput, optFns ...func(*codebuild.Options)) (*codebuild.StartBuildOutput, error)
}

// AWSConnector is operator-level AWS config resolved from the serve YAML.
// Region/profile/static creds; ambient chain preferred (Profile/static empty).
type AWSConnector struct {
	Region    string
	Profile   string
	AccessKey string // discouraged; ambient chain preferred
	SecretKey string
}

// codeBuildDeliverer starts an AWS CodeBuild project build via SigV4.
type codeBuildDeliverer struct {
	clientFor func(Trigger) (startBuildAPI, error)
	mintFn    MintFunc
}

// newCodeBuildClient builds a real CodeBuild client for a trigger using the
// named serve-config connector (falling back to the trigger's own region and
// the ambient AWS credential chain). connectors is the serve-config map.
func newCodeBuildClientFactory(connectors map[string]AWSConnector) func(Trigger) (startBuildAPI, error) {
	return func(tr Trigger) (startBuildAPI, error) {
		region := tr.Config.AWSRegion
		var opts []func(*awsconfig.LoadOptions) error
		if c, ok := connectors[tr.Config.AWSConnector]; ok {
			if c.Region != "" {
				region = c.Region
			}
			if c.Profile != "" {
				opts = append(opts, awsconfig.WithSharedConfigProfile(c.Profile))
			}
			// Static creds intentionally omitted from the default path; ambient
			// chain (env/profile/IRSA/instance role) is preferred. If an
			// operator sets AccessKey/SecretKey, wire credentials.NewStaticCredentialsProvider here.
		}
		if region != "" {
			opts = append(opts, awsconfig.WithRegion(region))
		}
		cfg, err := awsconfig.LoadDefaultConfig(context.Background(), opts...)
		if err != nil {
			return nil, fmt.Errorf("aws config: %w", err)
		}
		return codebuild.NewFromConfig(cfg), nil
	}
}

func (d *codeBuildDeliverer) Deliver(ctx context.Context, tr Trigger, p BuildPayload) (int, error) {
	client, err := d.clientFor(tr)
	if err != nil {
		return 0, err
	}
	env := []cbtypes.EnvironmentVariable{
		{Name: aws.String("BV_REF"), Value: aws.String(p.RefUpdate.Refname), Type: cbtypes.EnvironmentVariableTypePlaintext},
		{Name: aws.String("BV_REPO"), Value: aws.String(p.Tenant + "/" + p.Repo), Type: cbtypes.EnvironmentVariableTypePlaintext},
		{Name: aws.String("BV_COMMIT"), Value: aws.String(p.HeadOID), Type: cbtypes.EnvironmentVariableTypePlaintext},
	}
	if tr.TokenMode == TokenInject {
		tok, err := d.mintFn(ctx, tr, p)
		if err != nil {
			return 0, fmt.Errorf("mint token: %w", err)
		}
		env = append(env, cbtypes.EnvironmentVariable{
			Name: aws.String("BVTS_TOKEN"), Value: aws.String(tok),
			Type: cbtypes.EnvironmentVariableTypePlaintext,
		})
	}
	src := p.HeadOID
	if src == "" {
		src = p.RefUpdate.NewOID
	}
	_, err = client.StartBuild(ctx, &codebuild.StartBuildInput{
		ProjectName:                  aws.String(tr.Config.AWSProject),
		SourceVersion:                aws.String(src),
		EnvironmentVariablesOverride: env,
	})
	if err != nil {
		return 0, fmt.Errorf("codebuild StartBuild: %w", err)
	}
	return 200, nil
}
```

> If `cbtypes.EnvironmentVariableTypePlaintext` differs in the pinned SDK version, run `go doc github.com/aws/aws-sdk-go-v2/service/codebuild/types.EnvironmentVariableType` and use the correct constant. AWS retryable-vs-terminal classification is left to the SDK's default retryer at the client layer; a returned error is treated as retryable by the worker (Task 9), which is the safe default.

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/buildtrigger/ -run TestCodeBuildDeliverer -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/buildtrigger/codebuild.go internal/buildtrigger/codebuild_test.go go.mod go.sum
git commit -m "feat(m30): native CodeBuild SigV4 StartBuild deliverer"
```

---

## Task 9: Metrics, audit, and the delivery worker

**Files:**
- Create: `internal/buildtrigger/metrics.go`
- Create: `internal/buildtrigger/audit.go`
- Create: `internal/buildtrigger/worker.go`
- Test: `internal/buildtrigger/worker_test.go`

- [ ] **Step 1: Write metrics + audit (mirror webhooks)**

`internal/buildtrigger/metrics.go` — one emitter per metric, each `slog LevelInfo "metric"` with `metric_name`. Mirror `internal/webhooks/metrics.go` exactly, renaming:

```go
package buildtrigger

import (
	"context"
	"log/slog"
)

// EmitFired logs build_trigger_fired_total{kind,result}. result ∈
// {delivered, failed_retry, dead_letter}.
func EmitFired(ctx context.Context, logger *slog.Logger, kind, result string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("metric_name", "build_trigger_fired_total"),
		slog.String("kind", kind),
		slog.String("result", result),
		slog.Int("value", 1))
}

// EmitAttemptDuration logs build_trigger_delivery_duration_ms{result}.
func EmitAttemptDuration(ctx context.Context, logger *slog.Logger, result string, ms int64) { /* metric_name=build_trigger_delivery_duration_ms */ }

// EmitDeadLetterMetric logs build_trigger_deadletter_total.
func EmitDeadLetterMetric(ctx context.Context, logger *slog.Logger) { /* metric_name=build_trigger_deadletter_total, value 1 */ }

// EmitTokenMinted logs build_token_minted_total.
func EmitTokenMinted(ctx context.Context, logger *slog.Logger) { /* metric_name=build_token_minted_total, value 1 */ }
```

`internal/buildtrigger/audit.go` — mirror `internal/webhooks/audit.go`, with `slog.Bool("audit", true)` + `slog.String("event", …)`:

```go
package buildtrigger

import (
	"context"
	"log/slog"
)

func EmitFiredAudit(ctx context.Context, logger *slog.Logger, deliveryID, triggerID, kind string, refCount int) { /* event=build.trigger.fired */ }
func EmitDelivered(ctx context.Context, logger *slog.Logger, deliveryID, triggerID string, attempts int, ms int64) { /* event=build.trigger.delivered */ }
func EmitFailed(ctx context.Context, logger *slog.Logger, deliveryID, triggerID string, attempts, code int, errMsg string, nextAt int64) { /* event=build.trigger.failed, LevelWarn */ }
func EmitDeadLetter(ctx context.Context, logger *slog.Logger, deliveryID, triggerID string, attempts, code int) { /* event=build.trigger.deadletter, LevelError */ }
func EmitTokenMintedAudit(ctx context.Context, logger *slog.Logger, tenant, repo, tokenLabel string, ttlSeconds int64) { /* event=build.token.minted — NEVER the token value */ }
func EmitEnqueueFailed(ctx context.Context, logger *slog.Logger, tenant, repo, errMsg string) { /* event=build.trigger.enqueue_failed */ }
```

(Lifecycle audits `build.trigger.added|removed|enabled|disabled` are emitted from the CLI in Task 11; add `EmitTriggerLifecycle(ctx, logger, event, triggerID, tenant, repo string)` here for reuse.)

- [ ] **Step 2: Write the failing worker test**

```go
package buildtrigger

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// flakyDeliverer fails N times then succeeds.
type flakyDeliverer struct {
	failuresLeft int32
	calls        int32
}

func (f *flakyDeliverer) Deliver(ctx context.Context, tr Trigger, p BuildPayload) (int, error) {
	atomic.AddInt32(&f.calls, 1)
	if atomic.AddInt32(&f.failuresLeft, -1) >= 0 {
		return 500, context.DeadlineExceeded
	}
	return 200, nil
}

func TestWorker_RetriesThenDelivers(t *testing.T) {
	svc, _ := newTestSvc(t)
	ctx := context.Background()
	tr := mustCreate(t, svc, TriggerInput{Tenant: "acme", Repo: "app", Name: "n",
		Kind: KindGeneric, Config: Config{URL: "https://x"}, RefInclude: []string{"refs/heads/main"}})
	enqueueOne(t, svc, tr.ID, "acme", "app", "refs/heads/main")

	d := &flakyDeliverer{failuresLeft: 1} // fail once, then succeed
	cfg := WorkerConfig{
		TickInterval:    5 * time.Millisecond,
		ClaimBatchSize:  16,
		Concurrency:     2,
		BackoffSchedule: []time.Duration{5 * time.Millisecond, 5 * time.Millisecond, 5 * time.Millisecond},
		Deliverers:      map[Kind]Deliverer{KindGeneric: d},
	}
	wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	go StartWorker(wctx, svc, cfg)

	waitUntil(t, 2*time.Second, func() bool { return svc.countByStatus(ctx, "delivered") == 1 })
	if c := atomic.LoadInt32(&d.calls); c < 2 {
		t.Fatalf("expected >=2 attempts, got %d", c)
	}
}

func TestWorker_DeadLetterAfterExhaustion(t *testing.T) {
	svc, _ := newTestSvc(t)
	ctx := context.Background()
	tr := mustCreate(t, svc, TriggerInput{Tenant: "acme", Repo: "app", Name: "n",
		Kind: KindGeneric, Config: Config{URL: "https://x"}, RefInclude: []string{"refs/heads/main"}})
	enqueueOne(t, svc, tr.ID, "acme", "app", "refs/heads/main")

	d := &flakyDeliverer{failuresLeft: 1 << 30} // always fail
	cfg := WorkerConfig{
		TickInterval:    5 * time.Millisecond,
		BackoffSchedule: []time.Duration{2 * time.Millisecond, 2 * time.Millisecond},
		Deliverers:      map[Kind]Deliverer{KindGeneric: d},
	}
	wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	go StartWorker(wctx, svc, cfg)
	waitUntil(t, 2*time.Second, func() bool { return svc.countByStatus(ctx, "dead_letter") == 1 })
}
```

Add test helpers `enqueueOne`, `waitUntil`, and `(*Service).countByStatus(ctx, status)` (SELECT count(*) … WHERE status=?).

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/buildtrigger/ -run TestWorker -v`
Expected: FAIL (undefined `WorkerConfig`/`StartWorker`).

- [ ] **Step 4: Implement the worker (adapt `internal/webhooks/worker.go`)**

`internal/buildtrigger/worker.go`. Key differences from the webhook worker:
- `WorkerConfig` adds `Deliverers map[Kind]Deliverer` and `MintFn MintFunc`; production sets `Deliverers` to `{generic: httpDeliverer, cloudbuild: httpDeliverer, codebuild: codeBuildDeliverer}`.
- `claim` joins `build_triggers` for the full row (kind, config_json, token_mode, token_scopes, token_ttl_seconds) plus active filter:

```go
func claimSerialized(ctx context.Context, db sqlitestore.Querier, batch int) ([]claimedRow, error) {
	// SELECT d.id, d.trigger_id, d.payload_json, d.attempts,
	//        t.kind, t.config_json, t.token_mode, t.token_scopes, t.token_ttl_seconds,
	//        t.tenant, t.repo, t.name
	//   FROM build_trigger_deliveries d
	//   JOIN build_triggers t ON t.id = d.trigger_id
	//  WHERE d.status='pending' AND d.next_attempt_at <= ? AND t.active=1
	//  ORDER BY d.next_attempt_at LIMIT ?
	// then UPDATE … status='in_flight', attempts=attempts+1, last_attempt_at=?
	// Mirror webhooks claimSerialized exactly (RunInTx); add claimSkipLocked for postgres.
}
```

- `deliver(ctx, svc, cfg, row, logger)`:
  1. reconstruct `Trigger` + `BuildPayload` from the claimed row (decode config_json, payload_json).
  2. `d := cfg.Deliverers[trigger.Kind]`; if nil → `recordResult(... err: fmt.Errorf("no deliverer for kind %q", kind))`.
  3. `code, err := d.Deliver(ctx, trigger, payload)`; `recordResult(...)`.
- `recordResult` is byte-for-byte the webhook logic but against `build_trigger_deliveries` and emitting the build metrics/audit (`EmitFired`, `EmitAttemptDuration`, `EmitDelivered`/`EmitFailed`/`EmitDeadLetter`, `EmitDeadLetterMetric`). Reuse `jitter`, `truncErr` (copy them in, or factor to a tiny shared file — copying is fine and lower-risk).
- `StartWorker` mirrors the webhook one: defaults via `DefaultWorkerConfig()` (`TickInterval 1s`, `ClaimBatchSize 32`, `Concurrency 8`, `HTTPTimeout 10s`, `BackoffSchedule {1m,30m,2h,12h}`, `BackoffJitterFrac 0.25`, `ReclaimThreshold 60s`), initial + periodic `Reclaim`, nil-Service no-op.
- The HTTP deliverer's `client` is built once in `StartWorker` from `cfg.Egress` via `webhooks.NewHTTPClient(pol, cfg.HTTPTimeout)` and injected into the `httpDeliverer`. `MintFn` is injected into both deliverers.
- Add `Reclaim(ctx, db, threshold)` (copy `internal/webhooks/reclaim.go`, retargeting the table) in a `reclaim.go` file.

Provide the `MintFunc` wiring in `worker.go`:

```go
// NewMintFunc returns a MintFunc backed by the authdb store. ttl/scopes come
// from the trigger. Emits build.token.minted audit + metric.
func NewMintFunc(store *sqlitestore.Store, logger *slog.Logger) MintFunc {
	return func(ctx context.Context, tr Trigger, p BuildPayload) (string, error) {
		tok, err := store.MintBuildToken(ctx, sqlitestore.MintBuildParams{
			Tenant: tr.Tenant, Repo: tr.Repo, Scopes: tr.TokenScopes,
			TTLSeconds: int64(tr.TokenTTL.Seconds()),
			Label:      "build:" + tr.Tenant + "/" + tr.Repo + ":" + tr.Name,
		})
		if err != nil {
			return "", err
		}
		EmitTokenMinted(ctx, logger)
		EmitTokenMintedAudit(ctx, logger, tr.Tenant, tr.Repo,
			"build:"+tr.Tenant+"/"+tr.Repo+":"+tr.Name, int64(tr.TokenTTL.Seconds()))
		return tok, nil
	}
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/buildtrigger/ -run TestWorker -v`
Expected: PASS (both retry and dead-letter).

- [ ] **Step 6: Run the whole package + commit**

Run: `go test ./internal/buildtrigger/... -v`
Expected: PASS.

```bash
git add internal/buildtrigger/metrics.go internal/buildtrigger/audit.go \
        internal/buildtrigger/worker.go internal/buildtrigger/reclaim.go \
        internal/buildtrigger/worker_test.go
git commit -m "feat(m30): buildtrigger delivery worker + metrics + audit"
```

---

## Task 10: Operator config + declarative apply (`internal/buildtrigger`)

**Files:**
- Create: `internal/buildtrigger/config.go`
- Create: `internal/buildtrigger/apply.go`
- Test: `internal/buildtrigger/config_test.go`
- Modify: `go.mod` (add `gopkg.in/yaml.v3`)

- [ ] **Step 1: Add the dependency**

Run: `go get gopkg.in/yaml.v3@latest`

- [ ] **Step 2: Write the failing test**

```go
package buildtrigger

import (
	"context"
	"testing"
	"time"
)

func TestParseServeConfig(t *testing.T) {
	yml := []byte(`
build:
  defaults:
    token_ttl: 15m
    token_scopes: ["repo:read","lfs:read"]
    audience: https://bucketvcs.example
  aws_connectors:
    default:
      region: us-east-1
      profile: bucketvcs-codebuild
`)
	cfg, err := ParseServeConfig(yml)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Build.Defaults.TokenTTL != 15*time.Minute {
		t.Fatalf("ttl=%v", cfg.Build.Defaults.TokenTTL)
	}
	c, ok := cfg.Build.AWSConnectors["default"]
	if !ok || c.Region != "us-east-1" || c.Profile != "bucketvcs-codebuild" {
		t.Fatalf("connector: %+v", cfg.Build.AWSConnectors)
	}
}

func TestApply_Reconcile(t *testing.T) {
	svc, _ := newTestSvc(t)
	ctx := context.Background()
	yml := []byte(`
triggers:
  - tenant: acme
    repo: app
    name: main
    kind: cloudbuild
    url: https://cb.example/x:webhook?key=k&secret=s
    ref_include: ["refs/heads/main"]
    token_mode: none
`)
	res, err := Apply(ctx, svc, yml, false)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Created != 1 {
		t.Fatalf("created=%d", res.Created)
	}
	// Idempotent: second apply updates, not duplicates.
	res2, _ := Apply(ctx, svc, yml, false)
	if res2.Created != 0 || res2.Updated != 1 {
		t.Fatalf("second apply: %+v", res2)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/buildtrigger/ -run 'TestParseServeConfig|TestApply' -v`
Expected: FAIL.

- [ ] **Step 4: Implement config + apply**

`internal/buildtrigger/config.go`:

```go
package buildtrigger

import (
	"fmt"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"gopkg.in/yaml.v3"
)

// ServeConfig is the scoped operator config for `bucketvcs serve --config`.
// Namespaced under `build:` so a later milestone can absorb the rest of the
// serve flags without a breaking change.
type ServeConfig struct {
	Build BuildSection `yaml:"build"`
}

type BuildSection struct {
	Defaults      Defaults                `yaml:"defaults"`
	AWSConnectors map[string]AWSConnector `yaml:"aws_connectors"`
}

type Defaults struct {
	TokenTTL    time.Duration   `yaml:"-"`
	TokenTTLRaw string          `yaml:"token_ttl"`
	TokenScopes []string        `yaml:"token_scopes"`
	Audience    string          `yaml:"audience"`
}

// ParseServeConfig parses the scoped build config. token_ttl is a Go duration
// string; aws_connectors values map to AWSConnector.
func ParseServeConfig(data []byte) (ServeConfig, error) {
	var c ServeConfig
	if err := yaml.Unmarshal(data, &c); err != nil {
		return ServeConfig{}, fmt.Errorf("buildtrigger: parse config: %w", err)
	}
	if c.Build.Defaults.TokenTTLRaw != "" {
		d, err := time.ParseDuration(c.Build.Defaults.TokenTTLRaw)
		if err != nil {
			return ServeConfig{}, fmt.Errorf("buildtrigger: token_ttl: %w", err)
		}
		c.Build.Defaults.TokenTTL = d
	}
	return c, nil
}
```

Add a custom `UnmarshalYAML` for `AWSConnector` if the field tags differ (use `yaml:"region"`, `yaml:"profile"`, `yaml:"access_key"`, `yaml:"secret_key"` on the struct in `codebuild.go`, or define a YAML-specific mirror struct here to avoid coupling).

`internal/buildtrigger/apply.go`:

```go
package buildtrigger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"gopkg.in/yaml.v3"
)

// applyDoc is the declarative trigger file shape for `build apply -f`.
type applyDoc struct {
	Triggers []applyTrigger `yaml:"triggers"`
}

type applyTrigger struct {
	Tenant       string   `yaml:"tenant"`
	Repo         string   `yaml:"repo"`
	Name         string   `yaml:"name"`
	Kind         string   `yaml:"kind"`
	URL          string   `yaml:"url"`
	Secret       string   `yaml:"secret"`
	AWSRegion    string   `yaml:"aws_region"`
	AWSProject   string   `yaml:"aws_project"`
	AWSConnector string   `yaml:"aws_connector"`
	RefInclude   []string `yaml:"ref_include"`
	RefExclude   []string `yaml:"ref_exclude"`
	TokenMode    string   `yaml:"token_mode"`
	TokenScopes  []string `yaml:"token_scopes"`
	TokenTTL     string   `yaml:"token_ttl"`
}

// ApplyResult reports the reconcile outcome.
type ApplyResult struct {
	Created, Updated, Pruned int
}

// Apply reconciles build_triggers from a declarative YAML doc. Upsert by
// (tenant,repo,name): existing rows are replaced (Remove+Create) so config
// changes take effect; created_at is reset on replace (documented). When
// prune is true, triggers in the covered (tenant,repo) sets that are absent
// from the doc are removed.
func Apply(ctx context.Context, svc *Service, data []byte, prune bool) (ApplyResult, error) {
	var doc applyDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return ApplyResult{}, fmt.Errorf("buildtrigger: parse apply file: %w", err)
	}
	var res ApplyResult
	covered := map[[2]string]map[string]bool{}
	for _, t := range doc.Triggers {
		in, err := toInput(t)
		if err != nil {
			return res, err
		}
		key := [2]string{t.Tenant, t.Repo}
		if covered[key] == nil {
			covered[key] = map[string]bool{}
		}
		covered[key][t.Name] = true

		existing, err := svc.findByName(ctx, t.Tenant, t.Repo, t.Name)
		switch {
		case errors.Is(err, ErrNotFound):
			if _, err := svc.Create(ctx, in); err != nil {
				return res, err
			}
			res.Created++
		case err != nil:
			return res, err
		default:
			if err := svc.Remove(ctx, existing.ID); err != nil {
				return res, err
			}
			if _, err := svc.Create(ctx, in); err != nil {
				return res, err
			}
			res.Updated++
		}
	}
	if prune {
		for key, names := range covered {
			all, err := svc.List(ctx, key[0], key[1])
			if err != nil {
				return res, err
			}
			for _, tr := range all {
				if !names[tr.Name] {
					if err := svc.Remove(ctx, tr.ID); err != nil {
						return res, err
					}
					res.Pruned++
				}
			}
		}
	}
	return res, nil
}

func toInput(t applyTrigger) (TriggerInput, error) {
	ttl := time.Duration(0)
	if t.TokenTTL != "" {
		d, err := time.ParseDuration(t.TokenTTL)
		if err != nil {
			return TriggerInput{}, fmt.Errorf("%w: token_ttl %q: %v", ErrInvalidInput, t.TokenTTL, err)
		}
		ttl = d
	}
	var scopes auth.TokenScope
	if len(t.TokenScopes) > 0 {
		s, err := auth.ParseScopes(joinComma(t.TokenScopes))
		if err != nil {
			return TriggerInput{}, fmt.Errorf("%w: token_scopes: %v", ErrInvalidInput, err)
		}
		scopes = s
	}
	return TriggerInput{
		Tenant: t.Tenant, Repo: t.Repo, Name: t.Name, Kind: Kind(t.Kind),
		Config: Config{URL: t.URL, Secret: t.Secret, AWSRegion: t.AWSRegion,
			AWSProject: t.AWSProject, AWSConnector: t.AWSConnector},
		RefInclude: t.RefInclude, RefExclude: t.RefExclude,
		TokenMode: TokenMode(t.TokenMode), TokenScopes: scopes, TokenTTL: ttl,
	}, nil
}
```

Add `(*Service).findByName(ctx, tenant, repo, name) (Trigger, error)` (SELECT … WHERE tenant=? AND repo=? AND name=? → ErrNotFound), and `joinComma([]string) string`.

> Document in the doc comment + operator guide that `apply` replace resets `created_at` (acceptable; triggers are config, not audit records). If preserving `created_at` matters later, switch to an in-place UPDATE.

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/buildtrigger/ -run 'TestParseServeConfig|TestApply' -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/buildtrigger/config.go internal/buildtrigger/apply.go \
        internal/buildtrigger/config_test.go go.mod go.sum
git commit -m "feat(m30): scoped serve config + declarative build apply"
```

---

## Task 11: CLI (`bucketvcs build …`)

**Files:**
- Create: `cmd/bucketvcs/build.go`
- Modify: the top-level command dispatch (find where `webhook` is routed — `grep -rn '"webhook"' cmd/bucketvcs/main.go` or `root.go`) to add a `"build"` case.
- Test: `cmd/bucketvcs/build_test.go`

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestBuildTriggerAddListRemove(t *testing.T) {
	db := newCLITestAuthDB(t) // mirror webhook_test.go's authdb-temp helper; seeds repo acme/app
	var out, errb bytes.Buffer
	ctx := context.Background()

	code := runBuild(ctx, []string{"trigger", "add", "--auth-db", db,
		"--tenant", "acme", "--repo", "app", "--name", "main", "--kind", "cloudbuild",
		"--url", "https://cb.example/x:webhook?key=k&secret=s",
		"--ref-include", "refs/heads/main"}, &out, &errb)
	if code != 0 {
		t.Fatalf("add exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "secret=") {
		t.Fatalf("expected secret line: %s", out.String())
	}

	out.Reset()
	code = runBuild(ctx, []string{"trigger", "list", "--auth-db", db,
		"--tenant", "acme", "--repo", "app", "--format", "json"}, &out, &errb)
	if code != 0 || !strings.Contains(out.String(), `"name":"main"`) {
		t.Fatalf("list: code=%d out=%s", code, out.String())
	}
}

func TestBuildTriggerAdd_BadKindExit2(t *testing.T) {
	db := newCLITestAuthDB(t)
	var out, errb bytes.Buffer
	code := runBuild(context.Background(), []string{"trigger", "add", "--auth-db", db,
		"--tenant", "acme", "--repo", "app", "--name", "n", "--kind", "bogus", "--url", "https://x"}, &out, &errb)
	if code != 2 {
		t.Fatalf("want exit 2 for bad kind, got %d", code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/bucketvcs/ -run TestBuildTrigger -v`
Expected: FAIL (undefined `runBuild`).

- [ ] **Step 3: Implement the CLI (mirror `cmd/bucketvcs/webhook.go`)**

`cmd/bucketvcs/build.go` with the same structure as `webhook.go`:
- `runBuild(ctx, args, stdout, stderr) int` dispatches `object` ∈ {`trigger`, `delivery`, `apply`, `test`}.
- `trigger` actions: `add|list|remove|enable|disable`. `--scopes` via `auth.ParseScopes`; `--token-ttl` via `time.ParseDuration`; `--ref-include`/`--ref-exclude` accept csv (split on comma). `add` prints `trigger_id=…  tenant=…  repo=…  name=…  kind=…` then `secret=…  # store this now` (only for generic/cloudbuild). Map `buildtrigger.ErrInvalidInput`/`ErrConflict`/`ErrNotFound` → exit codes (2 for invalid/conflict-as-usage; 1 for operational; `ErrNotFound` on remove → 1). Emit `build.trigger.added|removed|enabled|disabled` via `buildtrigger.EmitTriggerLifecycle`.
- `delivery` actions: `list|show|replay` against `build_trigger_deliveries` (replay flips a `dead_letter`/`delivered` row back to `pending` with `next_attempt_at=now`, returning `ErrReplayInFlight` for in_flight). Mirror `webhook.go`'s delivery subcommands.
- `apply`: read `-f <file>`, call `buildtrigger.Apply(ctx, svc, data, *prune)`, print `created=N updated=N pruned=N`.
- `test`: `--id`, load the trigger, synthesize a `PushInfo` with one fabricated `RefUpdate` (use the first `ref_include` literal if non-glob, else `refs/heads/main`), call `Enqueue` so the worker picks it up; print the enqueued delivery id.
- `openBuildSvc(authDB)` mirrors `openWebhookSvc`.

Provide `Service` helpers needed by the CLI: `ListDeliveries(ctx, triggerID, status, limit)`, `GetDelivery(ctx, id)`, `ReplayDelivery(ctx, id)` — copy the shapes from `webhooks.Service`'s delivery methods, retargeting the table. Add these to `internal/buildtrigger/store.go` (or a `delivery.go`) with a small unit test each (in `store_test.go`).

- [ ] **Step 4: Wire dispatch**

In the main command switch (where `case "webhook":` lives), add:

```go
case "build":
	return runBuild(ctx, args[1:], stdout, stderr)
```

Match the exact dispatch signature used by the neighbouring `webhook` case.

- [ ] **Step 5: Run tests + build**

Run: `go test ./cmd/bucketvcs/ -run TestBuildTrigger -v && go build ./...`
Expected: PASS + clean build.

- [ ] **Step 6: Commit**

```bash
git add cmd/bucketvcs/build.go cmd/bucketvcs/build_test.go cmd/bucketvcs/*.go \
        internal/buildtrigger/store.go internal/buildtrigger/store_test.go
git commit -m "feat(m30): bucketvcs build CLI (trigger/delivery/apply/test)"
```

---

## Task 12: Gateway wiring — enqueue on push

**Files:**
- Modify: `internal/gateway/server.go` (add `Options.BuildTriggers`; pass into `EngineRequest` at both receive-pack construction sites ~`:439`, `:455`)
- Modify: `internal/gitproto/receivepack/engine.go` (add `BuildTriggers *buildtrigger.Service` field)
- Modify: `internal/gitproto/receivepack/complete.go` (enqueue after the EventPush block, ~`:595`)
- Test: `internal/gitproto/receivepack/complete_buildtrigger_test.go`

- [ ] **Step 1: Write the failing test**

Add a test that drives a receive through the engine with a `BuildTriggers` service configured for `acme/app` (`ref_include=refs/heads/main`) and asserts one delivery row is enqueued after a push that updates `refs/heads/main`. Model it on the existing webhook enqueue test in this package (`grep -rn 'EventPush' internal/gitproto/receivepack/*_test.go` to find the closest existing harness and copy its setup). The assertion:

```go
rows := buildSvc.PendingForTest(ctx) // expose a test accessor mirroring webhooks
if len(rows) != 1 {
	t.Fatalf("expected 1 build delivery enqueued, got %d", len(rows))
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/gitproto/receivepack/ -run BuildTrigger -v`
Expected: FAIL (no `BuildTriggers` field).

- [ ] **Step 3: Add the engine field**

In `internal/gitproto/receivepack/engine.go`, after the `Webhooks *webhooks.Service` field (line ~53):

```go
	// BuildTriggers is OPTIONAL (M30). When non-nil, a successful receive
	// enqueues build-trigger deliveries for matching refs. Enqueue failures
	// are logged and never affect the receive outcome (fail-open).
	BuildTriggers *buildtrigger.Service
```

Add the import `"github.com/bucketvcs/bucketvcs/internal/buildtrigger"`.

- [ ] **Step 4: Enqueue in complete.go**

In `internal/gitproto/receivepack/complete.go`, immediately after the `if eng.Webhooks != nil { … }` EventPush block (the one ending ~line 595), add:

```go
	// M30 build triggers: fire CI builds for matching refs. Fail-open —
	// enqueue errors never affect the receive outcome.
	if eng.BuildTriggers != nil {
		var refUpdates []buildtrigger.RefUpdate
		for i, u := range rp.Updates {
			if statuses[i] != "" {
				continue
			}
			refUpdates = append(refUpdates, buildtrigger.RefUpdate{
				Refname: u.Refname, OldOID: u.OldOID, NewOID: u.NewOID,
			})
		}
		if len(refUpdates) > 0 {
			push := buildtrigger.PushInfo{
				Tenant: tenant, Repo: repoID, Actor: actorName,
				TxID:    viewAfter.Header.LatestTx,
				HeadOID: pushHeadOIDFromBT(refUpdates),
				RefUpdates: refUpdates,
			}
			if err := eng.BuildTriggers.Enqueue(ctx, push); err != nil {
				buildtrigger.EmitEnqueueFailed(ctx, eng.loggerOrDefault(),
					tenant, repoID, err.Error())
			}
		}
	}
```

Add a small local `pushHeadOIDFromBT(refUpdates)` helper (or reuse the existing `pushHeadOID` by converting types — simplest: a 4-line helper returning the first non-zero `NewOID`, mirroring `pushHeadOID`).

- [ ] **Step 5: Add the gateway Option + wiring**

In `internal/gateway/server.go`: add to `Options` (near `Webhooks *webhooks.Service`, line ~162):

```go
	// BuildTriggers enables M30 build-trigger enqueue on receive-pack.
	BuildTriggers *buildtrigger.Service
```

and set `BuildTriggers: opts.BuildTriggers` in both `EngineRequest` literals that already set `Webhooks:` (lines ~439 and ~455). Add the import.

- [ ] **Step 6: Run tests + build**

Run: `go test ./internal/gitproto/receivepack/ ./internal/gateway/ -v && go build ./...`
Expected: PASS + clean build.

- [ ] **Step 7: Commit**

```bash
git add internal/gateway/server.go internal/gitproto/receivepack/engine.go \
        internal/gitproto/receivepack/complete.go \
        internal/gitproto/receivepack/complete_buildtrigger_test.go
git commit -m "feat(m30): enqueue build triggers on successful receive-pack"
```

---

## Task 13: serve wiring — flags, service, worker + sweep goroutines

**Files:**
- Modify: `cmd/bucketvcs/serve.go`
- Test: covered by the smoke script (Task 14) + `go build`

- [ ] **Step 1: Add flags**

In `serve.go`'s flag block, add:

```go
buildTriggersEnabled := fs.Bool("build-triggers", false, "Enable M30 build triggers (enqueue on push + delivery worker)")
buildConfigPath := fs.String("build-config", "", "Path to scoped build config YAML (aws_connectors, defaults)")
buildSweepInterval := fs.Duration("build-sweep-interval", 5*time.Minute, "Interval for sweeping expired build-minted tokens")
```

- [ ] **Step 2: Construct the service + load config**

After the webhook service is constructed (~`serve.go:499`):

```go
var buildSvc *buildtrigger.Service
var buildConnectors map[string]buildtrigger.AWSConnector
if *buildTriggersEnabled {
	buildSvc = buildtrigger.New(authS.DB())
	if *buildConfigPath != "" {
		data, err := os.ReadFile(*buildConfigPath)
		if err != nil {
			return fmt.Errorf("read --build-config: %w", err)
		}
		cfg, err := buildtrigger.ParseServeConfig(data)
		if err != nil {
			return err
		}
		buildConnectors = cfg.Build.AWSConnectors
	}
}
```

Pass `BuildTriggers: buildSvc` into the `gateway.Options` literal (wherever `Webhooks: webhookSvc` is set).

- [ ] **Step 3: Start the worker + sweep (guard on replica, like webhooks)**

Next to the webhook worker start (~`serve.go:673`):

```go
if *buildTriggersEnabled && !isReplica {
	wcfg := buildtrigger.DefaultWorkerConfig()
	wcfg.Egress = webhookEgress // reuse the same SSRF egress policy
	wcfg.MintFn = buildtrigger.NewMintFunc(authS, logger)
	wcfg.Deliverers = buildtrigger.ProductionDeliverers(wcfg.MintFn, buildConnectors, webhookEgress, wcfg.HTTPTimeout)
	go buildtrigger.StartWorker(serveCtx, buildSvc, wcfg)

	go func() {
		t := time.NewTicker(*buildSweepInterval)
		defer t.Stop()
		for {
			select {
			case <-serveCtx.Done():
				return
			case <-t.C:
				if n, err := authS.SweepExpiredBuildTokens(serveCtx); err != nil {
					logger.LogAttrs(serveCtx, slog.LevelWarn, "build token sweep error", slog.String("err", err.Error()))
				} else if n > 0 {
					logger.LogAttrs(serveCtx, slog.LevelInfo, "build tokens swept", slog.Int64("count", n))
				}
			}
		}
	}()
}
```

Add a `ProductionDeliverers(mint MintFunc, connectors map[string]AWSConnector, egress *webhooks.EgressPolicy, timeout time.Duration) map[Kind]Deliverer` constructor in `internal/buildtrigger/worker.go` that builds the shared `httpDeliverer` (with `webhooks.NewHTTPClient(egress, timeout)`) for `generic`+`cloudbuild` and the `codeBuildDeliverer` (with `newCodeBuildClientFactory(connectors)`) for `codebuild`. Confirm `authS` is the `*sqlitestore.Store` (it exposes `MintBuildToken`/`SweepExpiredBuildTokens`); if `authS` is a higher-level wrapper, reach the underlying store the same way the OIDC sweep does (`authS.SweepExpiredOIDCTokens` already lives on it, so `SweepExpiredBuildTokens` will too).

- [ ] **Step 4: Build**

Run: `go build ./... && go vet ./internal/buildtrigger/... ./cmd/bucketvcs/...`
Expected: clean.

- [ ] **Step 5: Commit**

```bash
git add cmd/bucketvcs/serve.go internal/buildtrigger/worker.go
git commit -m "feat(m30): serve wiring — build-trigger worker + token sweep"
```

---

## Task 14: End-to-end smoke (localfs)

**Files:**
- Create: `scripts/smoke_buildtriggers.sh` (mirror an existing `scripts/smoke_*.sh`)

- [ ] **Step 1: Write the smoke script**

Model on an existing smoke (e.g. `scripts/smoke_webhooks*.sh` or `scripts/smoke_oidc*.sh` — `ls scripts/`). Steps:

1. `bucketvcs init` a localfs repo `acme/app`; create a user + push token; start `bucketvcs serve --build-triggers …` in the background.
2. Start a tiny receiver: `python3 -m http.server` won't capture bodies; instead use a 15-line `nc`/`python` one-liner that writes the request body to a file and returns `200`. (Copy the receiver pattern from the webhook smoke if present.)
3. `bucketvcs build trigger add --kind generic --token-mode inject --url http://127.0.0.1:<port>/ --ref-include refs/heads/main` (serve started with `--webhook-allow-cidr 127.0.0.1/32` so loopback egress is allowed).
4. `git push` a commit to `main`. Poll the receiver's captured file (up to ~10s).
5. Assert the captured body contains `"repo":"app"` and a `"bvts_token":"bvts_…"`.
6. Extract the token; run `git clone http://x-access-token:$TOKEN@127.0.0.1:<git-port>/acme/app cloned` → succeeds. Clone `acme/other` with the same token → **fails** (403).
7. Push to a non-matching branch `dev`; assert the receiver got **no** new delivery (body file unchanged).
8. `bucketvcs build delivery list` shows a `delivered` row. Exit non-zero on any failed assertion.

- [ ] **Step 2: Run the smoke**

Run: `bash scripts/smoke_buildtriggers.sh`
Expected: prints per-step ✓ and exits 0.

- [ ] **Step 3: Commit**

```bash
git add scripts/smoke_buildtriggers.sh
git commit -m "test(m30): end-to-end build-trigger smoke (localfs)"
```

---

## Task 15: Operator guide + docs

**Files:**
- Create: `docs/operator-guides/build-triggers.md`
- Modify: `docs/operator-guides/` index/overview if one exists (`ls docs/operator-guides/`)

- [ ] **Step 1: Write the operator guide**

Cover, with worked examples:
1. **Concepts** — three kinds; OIDC-pull vs mint-and-inject; ref include/exclude semantics (exclude wins, empty include = all).
2. **Cloud Build (OIDC-pull, recommended)** — register the Google issuer + trust rule (the exact M22 `oidc issuer add` / `oidc rule add` commands from the spec §3.1, matching on the build SA `email`); create a `cloudbuild` trigger with `token_mode=none`; the `gcloud auth print-identity-token --audiences=<aud>` + `POST /_oidc/token` snippet the build runs; how to map `$(body.ref)` in the Cloud Build webhook trigger.
3. **CodeBuild (mint-and-inject)** — the serve `--build-config` YAML with an `aws_connectors.default` (region + profile; note ambient-chain preference and the static-key foot-gun); create a `codebuild` trigger; how `BVTS_TOKEN`/`BV_REF`/`BV_COMMIT` arrive as env vars; a buildspec snippet that clones with `BVTS_TOKEN`.
4. **Generic** — pointing at an API Gateway/Lambda shim; HMAC signature header (`BucketVCS-Signature`, same scheme as webhooks) and how to verify it; opt-in `token_mode=inject`.
5. **Declarative `build apply -f`** — the `triggers.yml` shape; `--prune` semantics; the `created_at` reset caveat.
6. **Security notes** — minted token is short-TTL/single-repo/read-only/revocable/swept; egress policy (`--webhook-allow-cidr`/`--webhook-deny-host`) governs generic/cloudbuild POSTs too; never commit static AWS keys.
7. **Observability** — the `build_trigger_*`/`build_token_minted_total` metrics and `build.*` audit events; `build delivery list`/`replay` for stuck deliveries.
8. **Deferred** (from spec §1.2) so operators aren't surprised: no native GCP connector, no per-commit path filters, no build-status callback.

- [ ] **Step 2: Verify links + render**

Run: `grep -rn 'build-triggers' docs/` to confirm cross-references resolve; eyeball the markdown.

- [ ] **Step 3: Final full test sweep**

Run: `go build ./... && go test ./internal/buildtrigger/... ./internal/auth/sqlitestore/... ./internal/gitproto/receivepack/... ./cmd/bucketvcs/...`
Expected: all PASS.

- [ ] **Step 4: Commit**

```bash
git add docs/operator-guides/build-triggers.md docs/operator-guides/
git commit -m "docs(m30): build-triggers operator guide"
```

---

## Self-Review notes (for the implementer)

- **Spec coverage:** Task 1 (schema/§2.2) · Task 2 (token mint+sweep/§2.3,§3.2) · Task 3 (kinds+matcher/§1.1,§2.1) · Task 4 (CRUD/§4.1) · Task 5 (enqueue/§2.1) · Task 6+7 (generic/cloudbuild deliver/§1.1,§3.2) · Task 8 (codebuild/§1.1) · Task 9 (worker+obs/§6,§7) · Task 10 (config+apply/§4.2,§4.3) · Task 11 (CLI/§4.1) · Task 12 (enqueue hook/§2) · Task 13 (serve/§2.5) · Task 14 (smoke/§8.5) · Task 15 (guide/§5,§9). All spec §s map to a task.
- **Open questions resolved in this plan:** single `minted_tokens_swept_total`→ implemented as a parallel `SweepExpiredBuildTokens` sharing one private helper (lower risk than mutating M22's public sweep; logged via the serve goroutine, not a labeled metric — revisit if a unified metric is wanted). `cloudbuild` kept as a thin preset of the generic deliverer. AWS connectors are **named** in serve YAML; region/project per-trigger. TTL ceiling = M22's 1h.
- **Type consistency:** `Kind`/`TokenMode`/`Config`/`Trigger`/`TriggerInput`/`BuildPayload`/`RefUpdate`/`PushInfo` are defined once in Task 3 and reused verbatim in Tasks 4–13. `MintBuildParams`/`MintBuildToken`/`SweepExpiredBuildTokens` (Task 2) are referenced by Tasks 9/13. `RefMatches`/`RenderBody`/`Deliverer`/`httpDeliverer`/`codeBuildDeliverer`/`WorkerConfig`/`StartWorker`/`Apply`/`ParseServeConfig` names are stable across tasks.
- **Risk note:** Task 2 refactors a tested M22 function (`SweepExpiredOIDCTokens`) — its existing tests must stay green (asserted in Task 2 Step 4). Task 9's worker is the largest copy-from-webhooks surface; keep `recordResult`/`jitter`/`truncErr`/`claim` byte-aligned with the proven originals, changing only the table name, the join target, and the emit calls.
