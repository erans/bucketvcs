# M25 Deployment Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close three operational gaps: repo delete on postgres, SSRF-safe webhook egress, and a `bucketvcs doctor` read-only diagnostics command.

**Architecture:** (A) A migration drops the postgres `webhook_endpoints`→`repos` FK so `DeleteRepoCascade` can run the same explicit child-table sweep on both backends. (B) An `EgressPolicy` in internal/webhooks enforces host/IP deny rules at dial time inside the delivery worker's HTTP client. (C) A small `internal/doctor` framework plus a `doctor` subcommand that accepts the full `serve` flag set (extracted into a shared registration helper) and runs read-only checks.

**Tech Stack:** Go (stdlib `net/netip`, `net.Dialer.Control`), SQLite/Postgres migrations, existing slog metric/audit conventions.

**Spec:** `docs/superpowers/specs/2026-06-04-m25-deployment-hardening-design.md`

**Conventions to follow throughout:**
- Exit codes: 0 success, 1 general error, 2 usage, 3 schema-gate.
- Metrics/audits are slog lines (see internal/webhooks/metrics.go, audit.go).
- Run `gofmt -l .` (must be empty) and `go vet ./...` before every commit.
- All commits end with the project's standard Co-Authored-By line.
- Postgres-gated tests carry `//go:build postgres` and skip unless `BUCKETVCS_POSTGRES_URL` is set. Run them only if you have a local postgres (CI runs them nightly); the sqlite suite must always pass: `go test ./...`.

---

### Task 1: Migration 0015 — drop the postgres webhook_endpoints→repos FK

**Files:**
- Create: `internal/auth/sqlitestore/migrations/0015_webhook_endpoints_fk.sql`
- Create: `internal/auth/sqlitestore/migrations_postgres/0015_webhook_endpoints_fk.sql`
- Create: `internal/auth/sqlitestore/migration0015_test.go`

Background: migration files are embedded (schema.go:12-16) and applied in lexical order; **each migration's SQL must INSERT its own schema_version row** (schema.go:33-34). The libsql/postgres splitter (sqlsplit.go) handles only plain statements + `--` comments — no `DO $$` blocks, so the postgres migration must use plain `ALTER TABLE` statements.

- [ ] **Step 1: Write the failing test**

Create `internal/auth/sqlitestore/migration0015_test.go`:

```go
package sqlitestore

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestMigration0015SqliteNoOp asserts 0015 applies on sqlite (version row
// present) and that the decorative webhook_endpoints FK is left in place —
// sqlite suppresses it via PRAGMA foreign_keys=OFF in DeleteRepoCascade, so
// the schema does not change.
func TestMigration0015SqliteNoOp(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	var n int
	if err := s.db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM schema_version WHERE version=15`).Scan(&n); err != nil {
		t.Fatalf("schema_version query: %v", err)
	}
	if n != 1 {
		t.Fatalf("migration 0015 not applied: count=%d", n)
	}

	var ddl string
	if err := s.db.QueryRowContext(t.Context(),
		`SELECT sql FROM sqlite_master WHERE name='webhook_endpoints'`).Scan(&ddl); err != nil {
		t.Fatalf("sqlite_master query: %v", err)
	}
	if !strings.Contains(ddl, "FOREIGN KEY") {
		t.Fatalf("sqlite webhook_endpoints lost its (decorative) FK:\n%s", ddl)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/auth/sqlitestore/ -run TestMigration0015SqliteNoOp -v`
Expected: FAIL with "migration 0015 not applied: count=0"

- [ ] **Step 3: Create the sqlite migration**

Create `internal/auth/sqlitestore/migrations/0015_webhook_endpoints_fk.sql`:

```sql
-- internal/auth/sqlitestore/migrations/0015_webhook_endpoints_fk.sql
-- M25: no-op on sqlite. The postgres twin drops the webhook_endpoints→repos
-- FK so DeleteRepoCascade can leave endpoint rows alive (M15.1 drain design:
-- a pending repo.deleted delivery must still join its endpoint at claim
-- time). sqlite achieves the same by suppressing FK enforcement via the
-- per-connection PRAGMA foreign_keys=OFF, so its (now decorative) FK stays.
INSERT INTO schema_version (version, applied_at) VALUES (15, strftime('%s','now'));
```

- [ ] **Step 4: Create the postgres migration**

Create `internal/auth/sqlitestore/migrations_postgres/0015_webhook_endpoints_fk.sql`:

```sql
-- internal/auth/sqlitestore/migrations_postgres/0015_webhook_endpoints_fk.sql
-- M25: drop the webhook_endpoints→repos FK (created unnamed by 0006). The
-- M15.1 drain design requires webhook_endpoints rows to SURVIVE repo
-- deletion so a pending repo.deleted delivery still joins its endpoint at
-- claim time. Postgres has no per-connection FK suppression (sqlite's
-- PRAGMA foreign_keys=OFF), so the constraint itself must go.
-- Both default names covered: PG < 12 named composite FKs after the first
-- column only; PG >= 12 uses all columns. IF EXISTS keeps this idempotent.
ALTER TABLE webhook_endpoints DROP CONSTRAINT IF EXISTS webhook_endpoints_tenant_repo_fkey;
ALTER TABLE webhook_endpoints DROP CONSTRAINT IF EXISTS webhook_endpoints_tenant_fkey;
INSERT INTO schema_version (version, applied_at) VALUES (15, EXTRACT(EPOCH FROM now())::bigint);
```

- [ ] **Step 5: Run sqlite test to verify it passes**

Run: `go test ./internal/auth/sqlitestore/ -run TestMigration0015SqliteNoOp -v`
Expected: PASS

- [ ] **Step 6: Add the postgres-gated FK-gone test**

Append to `internal/auth/sqlitestore/migrations_pg_test.go` (it already has `//go:build postgres` — verify, and reuse its `openPostgres`-style helper if one exists in that file; otherwise use `openPostgres(t)` from conformance_pg_test.go, same package):

```go
// TestPGMigration0015DropsWebhookEndpointsFK asserts no FK from
// webhook_endpoints to repos survives migration 0015.
func TestPGMigration0015DropsWebhookEndpointsFK(t *testing.T) {
	s := openPostgres(t)
	var n int
	if err := s.db.QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM pg_constraint
		 WHERE conrelid = 'webhook_endpoints'::regclass
		   AND contype = 'f'
		   AND confrelid = 'repos'::regclass`).Scan(&n); err != nil {
		t.Fatalf("pg_constraint query: %v", err)
	}
	if n != 0 {
		t.Fatalf("webhook_endpoints still has %d FK(s) to repos; migration 0015 did not drop it", n)
	}
}
```

Note: `s.db.QueryRowContext` rebinds `?`→`$n`; this query has no placeholders so it passes through unchanged.

- [ ] **Step 7: Verify everything compiles, including the gated file**

Run: `go vet -tags postgres ./internal/auth/sqlitestore/ && go test ./internal/auth/sqlitestore/`
Expected: vet clean; sqlite suite PASS (postgres tests skip without the env var; if you have `BUCKETVCS_POSTGRES_URL` + a scratch postgres, also run `go test -tags postgres ./internal/auth/sqlitestore/ -run TestPGMigration0015 -v`)

- [ ] **Step 8: Commit**

```bash
git add internal/auth/sqlitestore/migrations/0015_webhook_endpoints_fk.sql \
        internal/auth/sqlitestore/migrations_postgres/0015_webhook_endpoints_fk.sql \
        internal/auth/sqlitestore/migration0015_test.go \
        internal/auth/sqlitestore/migrations_pg_test.go
git commit -m "feat(authdb): migration 0015 drops the pg webhook_endpoints→repos FK"
```

---

### Task 2: DeleteRepoCascade postgres path

**Files:**
- Modify: `internal/auth/sqlitestore/deletecascade.go`
- Modify: `internal/auth/sqlitestore/conformance_pg_test.go:340-365` (replace the refusal test)
- Modify: `cmd/bucketvcs/repocmd.go:500-508` (drop the sqlite-only hint)
- Modify: `internal/web/reposettings.go:294-303` (backend-neutral flash message)
- Modify: `docs/operator-guides/web-ui.md:643` area (remove the postgres-refusal paragraph)

- [ ] **Step 1: Write the failing postgres conformance test**

In `internal/auth/sqlitestore/conformance_pg_test.go`, **replace** `TestPGDeleteRepoCascadeRefused` (lines ~342-365) with:

```go
// TestPGDeleteRepoCascade asserts the M25 postgres cascade path: child rows
// (protected_refs, lfs_locks, ...) are swept, the repos row is gone, and —
// the M15.1 drain invariant — webhook_endpoints + webhook_deliveries rows
// SURVIVE so a pending repo.deleted delivery can still be claimed.
func TestPGDeleteRepoCascade(t *testing.T) {
	s := openPostgres(t)
	ctx := context.Background()

	// Idempotent cleanup so re-runs against a persistent DB don't conflict
	// on UNIQUE(tenant, repo, url) or the repos PK.
	for _, q := range []string{
		`DELETE FROM webhook_deliveries WHERE endpoint_id IN
		   (SELECT id FROM webhook_endpoints WHERE tenant=? AND repo=?)`,
		`DELETE FROM webhook_endpoints WHERE tenant=? AND repo=?`,
		`DELETE FROM protected_refs WHERE tenant=? AND repo=?`,
		`DELETE FROM lfs_locks WHERE tenant=? AND repo=?`,
		`DELETE FROM repos WHERE tenant=? AND name=?`,
	} {
		if _, err := s.db.ExecContext(ctx, q, "cascade", "pgdel"); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}

	if err := s.RegisterRepo(ctx, "cascade", "pgdel"); err != nil {
		t.Fatalf("register repo: %v", err)
	}
	// Seed one row in each table the sweep covers + a webhook endpoint with
	// a pending delivery (the rows that must survive).
	now := time.Now().Unix()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO protected_refs (tenant, repo, refname_pattern, created_at)
		 VALUES (?, ?, ?, ?)`, "cascade", "pgdel", "refs/heads/main", now); err != nil {
		t.Fatalf("seed protected_refs: %v", err)
	}
	var epID int64
	err := s.db.RunInTx(ctx, func(tx Tx) error {
		var e error
		epID, e = tx.InsertReturningID(ctx,
			`INSERT INTO webhook_endpoints (tenant, repo, url, secret, event_mask, active, created_at)
			 VALUES (?, ?, ?, ?, ?, 1, ?)`,
			"cascade", "pgdel", "https://example.invalid/hook", "shh", 1, now)
		return e
	})
	if err != nil {
		t.Fatalf("seed endpoint: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO webhook_deliveries (id, endpoint_id, event_type, payload_json, status, next_attempt_at, created_at)
		 VALUES (?, ?, ?, ?, 'pending', ?, ?)`,
		"dlv-pgdel-1", epID, "repo.deleted", []byte(`{}`), now, now); err != nil {
		t.Fatalf("seed delivery: %v", err)
	}

	if err := s.DeleteRepoCascade(ctx, "cascade", "pgdel"); err != nil {
		t.Fatalf("DeleteRepoCascade on postgres: %v", err)
	}

	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := s.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("count %q: %v", q, err)
		}
		return n
	}
	if n := count(`SELECT COUNT(*) FROM repos WHERE tenant=? AND name=?`, "cascade", "pgdel"); n != 0 {
		t.Errorf("repos row survived: %d", n)
	}
	if n := count(`SELECT COUNT(*) FROM protected_refs WHERE tenant=? AND repo=?`, "cascade", "pgdel"); n != 0 {
		t.Errorf("protected_refs not swept: %d", n)
	}
	if n := count(`SELECT COUNT(*) FROM webhook_endpoints WHERE tenant=? AND repo=?`, "cascade", "pgdel"); n != 1 {
		t.Errorf("webhook_endpoints did not survive: %d (want 1)", n)
	}
	if n := count(`SELECT COUNT(*) FROM webhook_deliveries WHERE endpoint_id=?`, epID); n != 1 {
		t.Errorf("webhook_deliveries did not survive: %d (want 1)", n)
	}
}
```

Add `"time"` to that file's imports if absent. The `protected_refs` INSERT column list must match migration 0005 — check `migrations_postgres/0005_protected_refs.sql` and adjust columns (e.g. if it has an `actor` or `id` column) before running.

- [ ] **Step 2: Verify it fails to compile / fails (gated)**

Run: `go vet -tags postgres ./internal/auth/sqlitestore/`
Expected: compiles. If you have a scratch postgres: `go test -tags postgres ./internal/auth/sqlitestore/ -run TestPGDeleteRepoCascade -v` → FAIL with `want nil, got ErrCascadeUnsupportedBackend` (the gate at deletecascade.go:53 still refuses).

- [ ] **Step 3: Implement the postgres path**

In `internal/auth/sqlitestore/deletecascade.go`:

1. Hoist the `stmts` slice (currently local at lines 104-115) to a package-level var above `DeleteRepoCascade`:

```go
// cascadeStmts is the ordered child-table sweep shared by the sqlite and
// postgres paths of DeleteRepoCascade. webhook_endpoints/_deliveries are
// deliberately absent — those rows survive (M15.1 drain design).
var cascadeStmts = []struct {
	name string
	sql  string
}{
	{"protected_refs", `DELETE FROM protected_refs WHERE tenant=? AND repo=?`},
	{"protected_paths", `DELETE FROM protected_paths WHERE tenant=? AND repo=?`},
	{"hooks", `DELETE FROM hooks WHERE tenant=? AND repo=?`},
	{"repo_permissions", `DELETE FROM repo_permissions WHERE tenant=? AND repo=?`},
	{"ssh_keys (deploy-scope)", `DELETE FROM ssh_keys WHERE scope_tenant=? AND scope_repo=?`},
	{"lfs_locks", `DELETE FROM lfs_locks WHERE tenant=? AND repo=?`},
	{"repos", `DELETE FROM repos WHERE tenant=? AND name=?`},
}
```

2. Replace the refusal block (lines 46-55) with a dispatch to the new method:

```go
	// Postgres path (M25): migration 0015 dropped the webhook_endpoints→repos
	// FK, so a plain transaction suffices — endpoint rows survive by
	// construction and no pragma gymnastics are needed.
	if s.backend.Name() == "postgres" {
		return s.deleteRepoCascadePostgres(ctx, tenant, repo)
	}
```

3. In the sqlite body, replace the local `stmts` definition with use of `cascadeStmts` (the `for _, st := range stmts` loop becomes `for _, st := range cascadeStmts`; delete the local declaration).

4. Add the postgres method at the bottom of the file:

```go
// deleteRepoCascadePostgres sweeps the same child tables as the sqlite path,
// in one transaction. The remaining child-table FKs still declare ON DELETE
// CASCADE on postgres, so the explicit DELETEs are redundant there — they are
// kept so both backends read identically and behavior does not silently
// depend on FK presence. webhook_endpoints is untouched: its FK to repos was
// dropped by migration 0015 precisely so these rows outlive the repo.
func (s *Store) deleteRepoCascadePostgres(ctx context.Context, tenant, repo string) error {
	return s.db.RunInTx(ctx, func(tx Tx) error {
		for _, st := range cascadeStmts {
			if _, err := tx.ExecContext(ctx, st.sql, tenant, repo); err != nil {
				return fmt.Errorf("delete from %s: %w", st.name, err)
			}
		}
		return nil
	})
}
```

5. Update `ErrCascadeUnsupportedBackend`'s godoc (lines 11-16): it is no longer returned by any shipped backend; keep it defined for future backends:

```go
// ErrCascadeUnsupportedBackend is reserved for future auth-db backends on
// which DeleteRepoCascade cannot preserve the M15.1 drain design (webhook
// endpoint rows must survive repo deletion). As of M25 every shipped backend
// (sqlite, libsql, postgres) supports the cascade and this error is not
// returned; callers keep their errors.Is branches as cheap insurance.
var ErrCascadeUnsupportedBackend = errors.New("sqlitestore: repo delete cascade not supported on this backend")
```

Also update the `DeleteRepoCascade` godoc paragraph that says "Refuse rather than silently destroy" to mention the M25 postgres path.

- [ ] **Step 4: Update the CLI hint and web flash**

`cmd/bucketvcs/repocmd.go` — replace lines 500-508 with:

```go
	if err := s.DeleteRepoCascade(ctx, tenant, repo); err != nil {
		fmt.Fprintf(stderr, "delete: %v\n", err)
		return 1
	}
```

Then check whether `sqlitestore` is still imported elsewhere in repocmd.go (`grep -n sqlitestore cmd/bucketvcs/repocmd.go`); remove the import only if now unused.

`internal/web/reposettings.go` lines ~295-302 — keep the `errors.Is` branch (insurance for future backends) but make the message backend-neutral. Replace the flash string with:

```go
			s.redirectFlash(w, r, "/"+sr.tenant+"/"+sr.repo+"/settings",
				"repo delete is not supported on this server's database backend")
```

and update the comment above it from "Postgres can't suppress..." to "Reserved for future backends that can't preserve the M15.1 drain design (every shipped backend supports the cascade as of M25)."

- [ ] **Step 5: Update web-ui.md**

In `docs/operator-guides/web-ui.md` around line 643, find the paragraph beginning "Repo deletion via the web UI (or `bucketvcs repo delete`) is refused on Postgres" and delete it (or, if it carries surrounding context, replace with one sentence: "Repo deletion works on every auth-db backend (sqlite, libsql, Postgres).").

- [ ] **Step 6: Run tests**

Run: `go test ./internal/auth/sqlitestore/ ./cmd/bucketvcs/ ./internal/web/ && go vet -tags postgres ./internal/auth/sqlitestore/`
Expected: PASS (sqlite cascade tests in deletecascade_test.go still pass — the sqlite path is behavior-identical). With a scratch postgres: `go test -tags postgres ./internal/auth/sqlitestore/ -run 'TestPGDeleteRepoCascade|TestPGMigration0015' -v` → PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/auth/sqlitestore/deletecascade.go internal/auth/sqlitestore/conformance_pg_test.go \
        cmd/bucketvcs/repocmd.go internal/web/reposettings.go docs/operator-guides/web-ui.md
git commit -m "feat(authdb): repo delete cascade on postgres — drain design preserved via migration 0015"
```

---

### Task 3: Extract serve flag registration (prep for doctor, zero behavior change)

**Files:**
- Create: `cmd/bucketvcs/serveflags.go`
- Modify: `cmd/bucketvcs/serve.go:66-166` (flag definitions → one call + local aliases)

The trick: `serveFlags` holds the **pointers** returned by `fs.String`/`fs.Bool`/etc. `runServe` aliases them into the same local names it already uses, so the 700-line body is untouched.

- [ ] **Step 1: Create serveflags.go**

```go
package main

import (
	"flag"
	"time"
)

// serveFlags carries every `bucketvcs serve` flag as the pointer returned by
// the flag package. Registered via registerServeFlags so `bucketvcs doctor`
// can accept the exact same command line ("swap serve for doctor") and
// validate the configuration without serving.
type serveFlags struct {
	addr            *string
	storeURL        *string
	mirrorDir       *string
	authDB          *string
	authDBMaxConns  *int
	maxBody         *int64
	shutdownTimeout *time.Duration

	sshAddr    *string
	sshHostKey *string
	sshGrace   *time.Duration

	bundleURIMode    *string
	packURIMode      *string
	proxiedKeyFile   *string
	proxiedBundleTTL *time.Duration
	proxiedPackTTL   *time.Duration
	warmCommits      *int
	warmAge          *time.Duration
	proxiedBaseURL   *string

	lfsEnabled     *bool
	lfsPresignTTL  *time.Duration
	lfsSSHTokenTTL *time.Duration

	authRateLimitBurst        *int
	authRateLimitRefillPerMin *float64
	trustProxyHeaders         *bool
	authRateLimitDisabled     *bool

	hooksEnabled                *bool
	hooksRoot                   *string
	hooksUnsafeNoSandbox        *bool
	hooksOnInternalError        *string
	hooksTimeoutSec             *int
	hooksCPUSec                 *int
	hooksMemoryMB               *int
	hooksOutputMaxKB            *int
	hooksAllowNetwork           *string
	hooksEnv                    *string
	hooksPostReceiveConcurrency *int
	hooksPostReceiveQueue       *int

	oidcEnabled       *bool
	oidcSweepInterval *time.Duration

	uiEnabled       *bool
	uiAddr          *string
	uiDir           *string
	uiSessionTTL    *time.Duration
	uiBrowseTimeout *time.Duration

	oidcLogin      *bool
	oidcIssuer     *string
	oidcClientID   *string
	oidcSecretFile *string
	oidcRedirect   *string
	oidcScopes     *string
	oidcLabel      *string
}

// registerServeFlags registers the full serve flag surface on fs. The flag
// definitions here are MOVED verbatim from runServe — flag names, defaults,
// and help strings must not change (operator-visible surface).
func registerServeFlags(fs *flag.FlagSet) *serveFlags {
	sf := &serveFlags{}
	sf.addr = fs.String("addr", "", "HTTP listen address (host:port); leave empty to disable HTTP (default 127.0.0.1:8080 when --ssh-addr is also absent)")
	// ... every fs.String/fs.Int/fs.Int64/fs.Duration/fs.Bool/fs.Float64 call
	// currently at serve.go lines 68-165, moved verbatim, each assigned to
	// the matching sf field.
	return sf
}
```

Move ALL flag definitions from serve.go lines 68-165 into the function body (the comment blocks above each group move too — they document the flags, not the serve body).

- [ ] **Step 2: Rewire runServe**

In `runServe` (serve.go), replace the moved definition block (lines 68-165) with:

```go
	sf := registerServeFlags(fs)
	addr, storeURL, mirrorDir, authDB := sf.addr, sf.storeURL, sf.mirrorDir, sf.authDB
	authDBMaxConns, maxBody, shutdownTimeout := sf.authDBMaxConns, sf.maxBody, sf.shutdownTimeout
	sshAddr, sshHostKey, sshGrace := sf.sshAddr, sf.sshHostKey, sf.sshGrace
	bundleURIMode, packURIMode, proxiedKeyFile := sf.bundleURIMode, sf.packURIMode, sf.proxiedKeyFile
	proxiedBundleTTL, proxiedPackTTL, warmCommits, warmAge := sf.proxiedBundleTTL, sf.proxiedPackTTL, sf.warmCommits, sf.warmAge
	proxiedBaseURL := sf.proxiedBaseURL
	lfsEnabled, lfsPresignTTL, lfsSSHTokenTTL := sf.lfsEnabled, sf.lfsPresignTTL, sf.lfsSSHTokenTTL
	authRateLimitBurst, authRateLimitRefillPerMin := sf.authRateLimitBurst, sf.authRateLimitRefillPerMin
	trustProxyHeaders, authRateLimitDisabled := sf.trustProxyHeaders, sf.authRateLimitDisabled
	hooksEnabled, hooksRoot, hooksUnsafeNoSandbox := sf.hooksEnabled, sf.hooksRoot, sf.hooksUnsafeNoSandbox
	hooksOnInternalError, hooksTimeoutSec, hooksCPUSec := sf.hooksOnInternalError, sf.hooksTimeoutSec, sf.hooksCPUSec
	hooksMemoryMB, hooksOutputMaxKB := sf.hooksMemoryMB, sf.hooksOutputMaxKB
	hooksAllowNetwork, hooksEnv := sf.hooksAllowNetwork, sf.hooksEnv
	hooksPostReceiveConcurrency, hooksPostReceiveQueue := sf.hooksPostReceiveConcurrency, sf.hooksPostReceiveQueue
	oidcEnabled, oidcSweepInterval := sf.oidcEnabled, sf.oidcSweepInterval
	uiEnabled, uiAddr, uiDir, uiSessionTTL, uiBrowseTimeout := sf.uiEnabled, sf.uiAddr, sf.uiDir, sf.uiSessionTTL, sf.uiBrowseTimeout
	oidcLogin, oidcIssuer, oidcClientID := sf.oidcLogin, sf.oidcIssuer, sf.oidcClientID
	oidcSecretFile, oidcRedirect, oidcScopes, oidcLabel := sf.oidcSecretFile, sf.oidcRedirect, sf.oidcScopes, sf.oidcLabel
```

The rest of runServe compiles unchanged because the locals are the same pointers with the same names.

- [ ] **Step 3: Verify zero behavior change**

Run: `gofmt -l ./cmd/bucketvcs && go vet ./cmd/bucketvcs/ && go test ./cmd/bucketvcs/ -count=1`
Expected: gofmt empty, vet clean, all serve tests (serve_test.go, serve_ui_test.go, serve_oidc_test.go) PASS.

- [ ] **Step 4: Commit**

```bash
git add cmd/bucketvcs/serveflags.go cmd/bucketvcs/serve.go
git commit -m "refactor(serve): extract flag registration into registerServeFlags (prep for doctor)"
```

---

### Task 4: Webhook egress policy (internal/webhooks/egress.go)

**Files:**
- Create: `internal/webhooks/egress.go`
- Create: `internal/webhooks/egress_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/webhooks/egress_test.go`:

```go
package webhooks_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

func TestEgressIPDenied(t *testing.T) {
	deny := []string{
		"127.0.0.1", "::1", // loopback
		"169.254.169.254", "169.254.1.1", "fe80::1", // link-local / metadata
		"10.0.0.5", "172.16.3.4", "192.168.1.1", // RFC1918
		"fd12:3456::1",      // ULA
		"0.0.0.0", "::",     // unspecified
		"224.0.0.1", "ff02::1", // multicast
		"255.255.255.255",   // broadcast
		"::ffff:10.0.0.5",   // v4-mapped v6 private (Unmap coverage)
	}
	allow := []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111", "93.184.216.34"}

	p := &webhooks.EgressPolicy{}
	for _, s := range deny {
		if !p.IPDenied(netip.MustParseAddr(s)) {
			t.Errorf("IPDenied(%s) = false, want true", s)
		}
	}
	for _, s := range allow {
		if p.IPDenied(netip.MustParseAddr(s)) {
			t.Errorf("IPDenied(%s) = true, want false", s)
		}
	}
}

func TestEgressAllowCIDRPunchesHole(t *testing.T) {
	p := &webhooks.EgressPolicy{AllowCIDRs: []netip.Prefix{
		netip.MustParsePrefix("192.168.1.0/24"),
		netip.MustParsePrefix("127.0.0.0/8"),
	}}
	for _, s := range []string{"192.168.1.7", "127.0.0.1"} {
		if p.IPDenied(netip.MustParseAddr(s)) {
			t.Errorf("IPDenied(%s) = true, want false (allow-cidr hole)", s)
		}
	}
	// Adjacent private space stays denied.
	if !p.IPDenied(netip.MustParseAddr("192.168.2.7")) {
		t.Errorf("IPDenied(192.168.2.7) = false, want true")
	}
}

func TestEgressHostDenied(t *testing.T) {
	p := &webhooks.EgressPolicy{DenyHosts: []string{"metadata.google.internal", "*.corp.example.com"}}
	cases := []struct {
		host   string
		denied bool
	}{
		{"metadata.google.internal", true},
		{"METADATA.GOOGLE.INTERNAL", true},       // case-insensitive
		{"metadata.google.internal.", true},      // trailing-dot FQDN form
		{"jenkins.corp.example.com", true},       // wildcard suffix
		{"a.b.corp.example.com", true},           // deep subdomain
		{"corp.example.com", false},              // *. requires a label before the suffix
		{"example.com", false},
		{"10.0.0.5", false},                      // raw IPs never glob-match (IP layer handles them)
	}
	for _, c := range cases {
		if _, got := p.HostDenied(c.host); got != c.denied {
			t.Errorf("HostDenied(%q) = %v, want %v", c.host, got, c.denied)
		}
	}
}

func TestEgressDialDeniedAndAllowed(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	ctx := context.Background()
	denyAll := &webhooks.EgressPolicy{}
	if _, err := denyAll.DialContext(ctx, "tcp", ln.Addr().String()); err == nil {
		t.Fatal("dial to 127.0.0.1 with default policy: want error, got nil")
	} else {
		var denied *webhooks.EgressDeniedError
		if !errors.As(err, &denied) {
			t.Fatalf("want EgressDeniedError, got %T: %v", err, err)
		}
		if denied.DeniedBy != "ip" {
			t.Errorf("DeniedBy=%q, want ip", denied.DeniedBy)
		}
	}

	allowLoop := &webhooks.EgressPolicy{AllowCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}}
	c, err := allowLoop.DialContext(ctx, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial with 127.0.0.0/8 allow: %v", err)
	}
	c.Close()
}

func TestEgressClientDoesNotFollowRedirects(t *testing.T) {
	redirSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.invalid/next", http.StatusFound)
	}))
	defer redirSrv.Close()

	client := webhooks.NewHTTPClient(
		&webhooks.EgressPolicy{AllowCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}},
		2*time.Second)
	resp, err := client.Get(redirSrv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status=%d, want 302 (redirect must not be followed)", resp.StatusCode)
	}
}

func TestEgressDeniedErrorSurvivesClientDo(t *testing.T) {
	client := webhooks.NewHTTPClient(&webhooks.EgressPolicy{}, 2*time.Second)
	_, err := client.Get("http://127.0.0.1:1/hook")
	if err == nil {
		t.Fatal("want error")
	}
	var denied *webhooks.EgressDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("EgressDeniedError not unwrappable from client.Do error chain: %T: %v", err, err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/webhooks/ -run TestEgress -v`
Expected: FAIL (compile error: undefined webhooks.EgressPolicy)

- [ ] **Step 3: Implement egress.go**

Create `internal/webhooks/egress.go`:

```go
package webhooks

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"time"
)

// EgressPolicy decides which hosts/IPs the webhook delivery worker may dial.
// The zero value is the secure default: no host patterns, no allow holes —
// loopback, link-local (incl. cloud metadata 169.254.169.254), RFC1918/ULA
// private space, multicast, unspecified, and broadcast addresses are denied.
//
// Enforcement is two-layered (spec: M25 §B):
//
//  1. HostDenied — operator policy on the hostname, checked BEFORE DNS
//     resolution. Not a security boundary (a raw IP or alternate name
//     bypasses it); it covers what IP rules can't: internal names that
//     resolve to public IPs (split-horizon DNS, internal apps behind
//     public load balancers).
//  2. IPDenied — checked on the RESOLVED address inside the dialer's
//     Control hook, which closes DNS rebinding: whatever the name resolved
//     to at delivery time is what gets checked.
//
// There is deliberately NO allow-host list: allow-by-name against an IP deny
// set would mean "this name may resolve to private IPs", re-opening rebinding
// through the allowed name. Private receivers are reached via AllowCIDRs.
type EgressPolicy struct {
	DenyHosts  []string       // lowercase glob patterns: "exact.name" or "*.suffix"
	AllowCIDRs []netip.Prefix // holes punched in the IP deny set
}

// EgressDeniedError reports a delivery connection refused by policy.
// recordResult stores its Error() string as last_error, so the message
// carries the operator-facing remediation hint.
type EgressDeniedError struct {
	Host     string // hostname (or literal IP) from the endpoint URL
	IP       string // resolved IP that was denied ("" when DeniedBy=="host")
	DeniedBy string // "host" | "ip"
	Pattern  string // matched deny-host pattern (DeniedBy=="host" only)
}

func (e *EgressDeniedError) Error() string {
	if e.DeniedBy == "host" {
		return fmt.Sprintf("egress denied: host %q matches deny pattern %q (see --webhook-deny-host)", e.Host, e.Pattern)
	}
	return fmt.Sprintf("egress denied: %s resolves to %s in a blocked range (see --webhook-allow-cidr)", e.Host, e.IP)
}

// HostDenied reports whether host matches a deny pattern, returning the
// matched pattern. Matching is case-insensitive; a trailing dot (FQDN form)
// is ignored. "*.suffix" matches any name with at least one label before
// ".suffix" (so "*.corp.com" does NOT match the bare "corp.com").
func (p *EgressPolicy) HostDenied(host string) (string, bool) {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	for _, pat := range p.DenyHosts {
		lp := strings.ToLower(pat)
		if rest, ok := strings.CutPrefix(lp, "*."); ok {
			if strings.HasSuffix(h, "."+rest) {
				return pat, true
			}
		} else if h == lp {
			return pat, true
		}
	}
	return "", false
}

// IPDenied reports whether ip is outside the allowed egress set. AllowCIDRs
// are checked first (an allow hole wins); otherwise the address must be
// global unicast and not private.
func (p *EgressPolicy) IPDenied(ip netip.Addr) bool {
	ip = ip.Unmap()
	for _, pfx := range p.AllowCIDRs {
		if pfx.Contains(ip) {
			return false
		}
	}
	// IsGlobalUnicast already excludes loopback, link-local, multicast,
	// unspecified, and the IPv4 broadcast address — but (per Go docs) it
	// returns true for RFC1918/ULA private space, hence the IsPrivate OR.
	return !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate()
}

// DialContext is the policy-enforcing dialer used by NewHTTPClient. The
// hostname check runs pre-resolution; the IP check runs in the Dialer's
// Control hook on every address the resolver produced (the stdlib tries
// candidates in order, so a name resolving to [private, public] connects to
// the public one).
func (p *EgressPolicy) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if pat, denied := p.HostDenied(host); denied {
		return nil, &EgressDeniedError{Host: host, DeniedBy: "host", Pattern: pat}
	}
	d := &net.Dialer{
		Timeout: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			h, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(h)
			if err != nil {
				return err
			}
			if p.IPDenied(ip) {
				return &EgressDeniedError{Host: host, IP: ip.String(), DeniedBy: "ip"}
			}
			return nil
		},
	}
	return d.DialContext(ctx, network, addr)
}

// NewHTTPClient builds the delivery worker's HTTP client: policy-enforcing
// dialer, no proxy (an env-configured HTTP_PROXY would dial on our behalf and
// bypass the policy), and no redirect following (a 3xx is a delivery failure;
// industry convention, and simpler to reason about than re-checking hops).
func NewHTTPClient(p *EgressPolicy, timeout time.Duration) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.DialContext = p.DialContext
	return &http.Client{
		Timeout:   timeout,
		Transport: t,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/webhooks/ -run TestEgress -v`
Expected: PASS (all 6). If `TestEgressDeniedErrorSurvivesClientDo` fails on the `errors.As` assertion, the stdlib wrapped the Control error in a non-unwrapping type — fix by checking `errors.As` inside `DialContext` on the dialer's return error and re-returning the bare `*EgressDeniedError`; do NOT weaken the test to string matching.

- [ ] **Step 5: Commit**

```bash
git add internal/webhooks/egress.go internal/webhooks/egress_test.go
git commit -m "feat(webhooks): EgressPolicy — dial-time host/IP deny-list with allow-cidr holes"
```

---

### Task 5: Wire egress policy into the delivery worker

**Files:**
- Modify: `internal/webhooks/worker.go` (WorkerConfig field, client construction at :86-89, deliver at :242-275)
- Modify: `internal/webhooks/metrics.go`, `internal/webhooks/audit.go`
- Modify: `internal/webhooks/worker_test.go` (existing 4 tests need a loopback allow hole)
- Create test in: `internal/webhooks/worker_test.go` (default-deny regression)

- [ ] **Step 1: Update existing worker tests (they deliver to 127.0.0.1 httptest servers)**

In `worker_test.go`, add to the imports `"net/netip"`, and in **each** of the four `cfg := webhooks.WorkerConfig{...}` literals (lines ~54, ~113, ~161, ~221) add:

```go
		Egress: &webhooks.EgressPolicy{AllowCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}},
```

These four tests now double as the `--webhook-allow-cidr` regression suite: with the default-deny policy active and a loopback allow hole, delivery still works.

Then add the new default-deny test:

```go
// TestWorker_EgressDeniedByDefault asserts the secure default: with no
// Egress configured (nil → zero-value policy), a delivery to a loopback
// receiver is refused at dial time, never reaches the receiver, and lands
// back in pending with an egress-denied last_error.
func TestWorker_EgressDeniedByDefault(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := webhooks.New(db)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if _, err := svc.Create(ctx, webhooks.EndpointInput{
		Tenant: "acme", Repo: "site", URL: srv.URL, EventMask: webhooks.EventPush,
	}); err != nil {
		t.Fatalf("Create endpoint: %v", err)
	}
	if err := svc.Enqueue(ctx, webhooks.EventPush, "acme", "site", "alice",
		webhooks.PushPayload{TxID: "tx-1"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	cfg := webhooks.WorkerConfig{
		TickInterval:    25 * time.Millisecond,
		ClaimBatchSize:  32,
		Concurrency:     4,
		HTTPTimeout:     1 * time.Second,
		BackoffSchedule: []time.Duration{time.Hour}, // park after first failure
		// Egress deliberately nil → default deny-private policy.
	}
	go webhooks.StartWorker(ctx, svc, cfg)

	waitFor(t, 3*time.Second, func() bool {
		var lastErr string
		row := db.QueryRowContext(ctx, `SELECT COALESCE(last_error,'') FROM webhook_deliveries LIMIT 1`)
		_ = row.Scan(&lastErr)
		return strings.Contains(lastErr, "egress denied")
	}, "last_error never recorded an egress denial")

	if hits.Load() != 0 {
		t.Fatalf("receiver was reached %d time(s); egress deny must block at dial", hits.Load())
	}
}
```

- [ ] **Step 2: Run tests to verify the new one fails**

Run: `go test ./internal/webhooks/ -run TestWorker_EgressDeniedByDefault -v`
Expected: FAIL (compile error: unknown field Egress)

- [ ] **Step 3: Implement worker wiring**

In `internal/webhooks/worker.go`:

1. Imports: add `"errors"` and `"strings"`.

2. Add to `WorkerConfig` (after `HTTPClient`):

```go
	// Egress is the delivery egress policy. nil means the secure default
	// (zero-value EgressPolicy: deny loopback/link-local/private/etc.).
	// Ignored when HTTPClient is set (tests inject their own client).
	Egress *EgressPolicy
```

3. Replace the client construction (lines 86-89):

```go
	client := cfg.HTTPClient
	if client == nil {
		pol := cfg.Egress
		if pol == nil {
			pol = &EgressPolicy{} // secure default: deny private/loopback/link-local
		}
		client = NewHTTPClient(pol, cfg.HTTPTimeout)
	}
```

4. In `deliver` (line 242), add a scheme gate right after `t := time.Now().Unix()` / before building the request (defense in depth — Create validates the scheme, but a row edited directly in the DB bypasses it):

```go
	if !strings.HasPrefix(row.URL, "http://") && !strings.HasPrefix(row.URL, "https://") {
		recordResult(ctx, svc, cfg, row, 0,
			fmt.Errorf("egress denied: endpoint URL scheme must be http or https"),
			logger, time.Since(start).Milliseconds())
		return
	}
```

5. In `deliver`, in the `client.Do` error branch (line 261-264), detect a policy denial and emit observability before recording:

```go
	if err != nil {
		var denied *EgressDeniedError
		if errors.As(err, &denied) {
			EmitEgressDeniedMetric(ctx, logger)
			EmitEgressDenied(ctx, logger, row.ID, row.EndpointID, denied.Host, denied.IP, denied.DeniedBy, denied.Pattern)
		}
		recordResult(ctx, svc, cfg, row, 0, err, logger, durationMs)
		return
	}
```

6. Append to `metrics.go`:

```go
// EmitEgressDeniedMetric logs one webhook_egress_denied_total sample. The
// metric deliberately carries NO host/url/tenant labels — endpoint URLs are
// attacker-influenced and would explode cardinality under probing (same
// reasoning as M19's proxied_url_token_invalid_total).
func EmitEgressDeniedMetric(ctx context.Context, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "metric",
		slog.String("name", "webhook_egress_denied_total"),
		slog.Int("value", 1),
	)
}
```

7. Append to `audit.go`:

```go
// EmitEgressDenied logs the webhooks.egress_denied audit event when the
// delivery worker refuses to dial an endpoint under the M25 egress policy.
// deniedBy is "host" (deny-host pattern matched, pattern populated) or "ip"
// (resolved address in a blocked range, ip populated).
func EmitEgressDenied(ctx context.Context, logger *slog.Logger,
	deliveryID string, endpointID int64, host, ip, deniedBy, pattern string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, slog.LevelWarn, "webhooks.egress_denied",
		slog.String("delivery_id", deliveryID),
		slog.Int64("endpoint_id", endpointID),
		slog.String("host", host),
		slog.String("ip", ip),
		slog.String("denied_by", deniedBy),
		slog.String("pattern", pattern),
	)
}
```

- [ ] **Step 4: Run the full webhooks suite**

Run: `go test ./internal/webhooks/ -count=1 -v 2>&1 | tail -30`
Expected: PASS — including the four updated worker tests (loopback allow hole) and the new default-deny test.

- [ ] **Step 5: Commit**

```bash
git add internal/webhooks/worker.go internal/webhooks/worker_test.go \
        internal/webhooks/metrics.go internal/webhooks/audit.go
git commit -m "feat(webhooks): enforce egress policy in delivery worker — deny-private default, no redirects"
```

---

### Task 6: serve flags, registration-time checks, web/CLI surfaces

**Files:**
- Modify: `cmd/bucketvcs/serveflags.go` (two repeatable flags)
- Modify: `cmd/bucketvcs/serve.go` (build policy; wire into Service + worker)
- Modify: `internal/webhooks/service.go` (Egress field + Create check + sentinel)
- Modify: `internal/web/reposettings_webhooks.go` (flash for denied URL)
- Modify: `cmd/bucketvcs/webhook.go` (literal-IP warning on endpoint add)
- Test: `internal/webhooks/service_test.go` or wherever Create is tested (`grep -rn "func TestCreate" internal/webhooks/` to find it; add alongside)

- [ ] **Step 1: Write the failing service test**

Add to the file containing the existing `Service.Create` tests (find via `grep -rln "EndpointInput{" internal/webhooks/*_test.go`, likely service_test.go or enqueue_test.go; create `internal/webhooks/service_egress_test.go` if Create tests live in `webhooks_test` package files):

```go
package webhooks_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

func TestCreateRejectsLiteralDeniedIP(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := webhooks.New(db)
	svc.Egress = &webhooks.EgressPolicy{}
	ctx := context.Background()

	_, err := svc.Create(ctx, webhooks.EndpointInput{
		Tenant: "acme", Repo: "site",
		URL: "http://169.254.169.254/latest/meta-data", EventMask: webhooks.EventPush,
	})
	if !errors.Is(err, webhooks.ErrEgressDeniedURL) {
		t.Fatalf("Create(metadata IP): got %v, want ErrEgressDeniedURL", err)
	}

	// Hostname deny pattern also rejects up front.
	svc.Egress = &webhooks.EgressPolicy{DenyHosts: []string{"*.internal.example.com"}}
	_, err = svc.Create(ctx, webhooks.EndpointInput{
		Tenant: "acme", Repo: "site",
		URL: "https://ci.internal.example.com/hook", EventMask: webhooks.EventPush,
	})
	if !errors.Is(err, webhooks.ErrEgressDeniedURL) {
		t.Fatalf("Create(denied host): got %v, want ErrEgressDeniedURL", err)
	}

	// Allow hole permits the literal IP.
	svc.Egress = &webhooks.EgressPolicy{AllowCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}}
	if _, err := svc.Create(ctx, webhooks.EndpointInput{
		Tenant: "acme", Repo: "site",
		URL: "http://127.0.0.1:9999/hook", EventMask: webhooks.EventPush,
	}); err != nil {
		t.Fatalf("Create(allowed loopback): %v", err)
	}

	// nil policy (CLI process) skips the check entirely.
	svc2 := webhooks.New(db)
	if _, err := svc2.Create(ctx, webhooks.EndpointInput{
		Tenant: "acme", Repo: "site",
		URL: "http://10.0.0.5/hook", EventMask: webhooks.EventPush,
	}); err != nil {
		t.Fatalf("Create(nil policy): %v", err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/webhooks/ -run TestCreateRejectsLiteralDeniedIP -v`
Expected: FAIL (undefined: webhooks.ErrEgressDeniedURL / svc.Egress)

- [ ] **Step 3: Implement Service-side check**

In `internal/webhooks/service.go`:

1. Add `"net/netip"` to imports.

2. Add the sentinel next to the other Err vars:

```go
// ErrEgressDeniedURL is returned by Create when the endpoint URL names a
// literal IP (or matches a deny-host pattern) the configured egress policy
// would refuse at delivery time. Registration-time UX sugar only — the
// dial-time check in the worker remains the real gate (DNS at registration
// != DNS at delivery). Only enforced when Service.Egress is set (the serve
// process); the standalone CLI has no policy knowledge and warns instead.
var ErrEgressDeniedURL = errors.New("webhooks: endpoint URL targets an egress-denied address")
```

3. Add the field to `Service`:

```go
type Service struct {
	db sqlitestore.Querier
	// Egress, when set (serve wires the flag-built policy), lets Create
	// reject obviously-denied endpoint URLs up front. nil skips the check.
	Egress *EgressPolicy
}
```

4. In `Create`, after the `validateURL` block (after line ~91 `if err := validateURL(in.URL); err != nil {...}`), insert:

```go
	if err := s.checkEgressURL(in.URL); err != nil {
		return Endpoint{}, err
	}
```

5. Add the helper near `validateURL`:

```go
// checkEgressURL is the registration-time egress pre-check (no-op when no
// policy is configured). It catches only what is knowable without DNS:
// literal denied IPs and deny-host pattern matches.
func (s *Service) checkEgressURL(rawURL string) error {
	if s.Egress == nil {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidInput, err.Error())
	}
	host := u.Hostname()
	if pat, denied := s.Egress.HostDenied(host); denied {
		return fmt.Errorf("%w: host %q matches deny pattern %q", ErrEgressDeniedURL, host, pat)
	}
	if ip, perr := netip.ParseAddr(host); perr == nil && s.Egress.IPDenied(ip) {
		return fmt.Errorf("%w: literal IP %s is in a blocked range; deliveries will fail unless serve runs with a covering --webhook-allow-cidr", ErrEgressDeniedURL, ip)
	}
	return nil
}
```

- [ ] **Step 4: Run service test**

Run: `go test ./internal/webhooks/ -run TestCreateRejectsLiteralDeniedIP -v`
Expected: PASS

- [ ] **Step 5: Add the serve flags + wiring**

In `cmd/bucketvcs/serveflags.go`:

1. Add imports `"fmt"`, `"net/netip"`, `"strings"`.
2. Add two value fields to `serveFlags`:

```go
	// M25 webhook egress policy (populated by repeatable fs.Func flags).
	webhookAllowCIDRs []netip.Prefix
	webhookDenyHosts  []string
```

3. In `registerServeFlags`, after the rate-limit flag group:

```go
	// M25 webhook egress policy. The delivery worker denies loopback,
	// link-local (incl. cloud metadata), and private/ULA ranges by default;
	// these flags punch holes / add hostname deny patterns.
	fs.Func("webhook-allow-cidr",
		"CIDR webhook deliveries may reach despite the private-range deny default, e.g. 192.168.1.0/24 (repeatable; 0.0.0.0/0 restores the pre-M25 open behavior)",
		func(v string) error {
			p, err := netip.ParsePrefix(strings.TrimSpace(v))
			if err != nil {
				return fmt.Errorf("--webhook-allow-cidr: %w", err)
			}
			sf.webhookAllowCIDRs = append(sf.webhookAllowCIDRs, p)
			return nil
		})
	fs.Func("webhook-deny-host",
		"hostname glob webhook deliveries may never target: exact name or *.suffix, e.g. *.internal.example.com (repeatable; policy aid, not a security boundary — raw IPs bypass it)",
		func(v string) error {
			v = strings.TrimSpace(v)
			if v == "" || v == "*." || v == "*" {
				return fmt.Errorf("--webhook-deny-host: pattern must be an exact hostname or *.suffix, got %q", v)
			}
			sf.webhookDenyHosts = append(sf.webhookDenyHosts, v)
			return nil
		})
```

In `cmd/bucketvcs/serve.go`:

4. After the `webhookSvc := webhooks.New(authS.DB())` line (~:282), build + attach the policy:

```go
	// M25 egress policy: shared by the delivery worker (dial-time gate) and
	// Create's registration-time pre-check (web UI rejects literal denied
	// IPs up front).
	webhookEgress := &webhooks.EgressPolicy{
		DenyHosts:  sf.webhookDenyHosts,
		AllowCIDRs: sf.webhookAllowCIDRs,
	}
	webhookSvc.Egress = webhookEgress
```

5. Replace the worker start (~:450):

```go
	wcfg := webhooks.DefaultWorkerConfig()
	wcfg.Egress = webhookEgress
	go webhooks.StartWorker(serveCtx, webhookSvc, wcfg)
```

- [ ] **Step 6: Web UI flash for denied URLs**

In `internal/web/reposettings_webhooks.go`, in `webhooksAdd`'s Create error handling (after the `ErrConflict` branch, ~line 151), add:

```go
		if errors.Is(err, webhooks.ErrEgressDeniedURL) {
			EmitAdminActionMetric(r.Context(), s.logger, "webhook", "endpoint_add", "invalid")
			s.redirectFlash(w, r, sr.webhooksBase(), err.Error())
			return
		}
```

(Confirm `errors` is already imported in that file; it is — the ErrConflict branch uses it.)

- [ ] **Step 7: CLI literal-IP warning**

In `cmd/bucketvcs/webhook.go`, `runWebhookEndpointAdd`, after the successful `fmt.Fprintf(stdout, "endpoint_id=...")` print, add (with imports `"net/netip"`, `"net/url"` added to the file):

```go
	// The CLI process cannot know serve's --webhook-allow-cidr config, so a
	// literal IP in the default deny set gets a warning, not a rejection
	// (the dial-time gate in serve is the real enforcement).
	if u, perr := url.Parse(*urlFlag); perr == nil {
		if ip, ierr := netip.ParseAddr(u.Hostname()); ierr == nil && (&webhooks.EgressPolicy{}).IPDenied(ip) {
			fmt.Fprintf(stderr, "warning: %s is in the default egress deny set; deliveries will fail unless serve runs with a covering --webhook-allow-cidr\n", ip)
		}
	}
	return 0
```

- [ ] **Step 8: Full test pass**

Run: `gofmt -l ./cmd ./internal && go vet ./... && go test ./internal/webhooks/ ./internal/web/ ./cmd/bucketvcs/ -count=1`
Expected: gofmt empty, vet clean, PASS. (web-ui-phase3 smoke registers `https://example.invalid/hook` — a hostname, no literal IP, no delivery exercised — so smokes need no changes; the loopback regression lives in the Task 5 worker tests.)

- [ ] **Step 9: Commit**

```bash
git add cmd/bucketvcs/serveflags.go cmd/bucketvcs/serve.go cmd/bucketvcs/webhook.go \
        internal/webhooks/service.go internal/webhooks/service_egress_test.go \
        internal/web/reposettings_webhooks.go
git commit -m "feat(serve): --webhook-allow-cidr/--webhook-deny-host + registration-time egress pre-check"
```

---

### Task 7: sqlitestore inspection helpers (no-migrate open, schema version)

**Files:**
- Modify: `internal/auth/sqlitestore/store.go` (OpenForInspection)
- Modify: `internal/auth/sqlitestore/schema.go` (SchemaVersion, LatestMigrationVersion)
- Create: `internal/auth/sqlitestore/inspection_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/auth/sqlitestore/inspection_test.go`:

```go
package sqlitestore

import (
	"context"
	"io/fs"
	"path/filepath"
	"testing"
)

func TestSchemaVersionAndLatest(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "auth.db")
	s, err := Open(path) // applies all migrations
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	v, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if want := LatestMigrationVersion(); v != want {
		t.Fatalf("SchemaVersion=%d, want LatestMigrationVersion=%d", v, want)
	}
	if v < 15 {
		t.Fatalf("SchemaVersion=%d; expected at least 15 (M25 migration)", v)
	}
	s.Close()

	// OpenForInspection must NOT migrate: simulate an older db by deleting
	// the newest version row, reopen for inspection, version must stay stale.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := s2.db.ExecContext(ctx, `DELETE FROM schema_version WHERE version=?`, LatestMigrationVersion()); err != nil {
		t.Fatalf("simulate stale db: %v", err)
	}
	s2.Close()

	insp, err := OpenForInspection(path)
	if err != nil {
		t.Fatalf("OpenForInspection: %v", err)
	}
	defer insp.Close()
	v2, err := insp.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion (inspection): %v", err)
	}
	if v2 != LatestMigrationVersion()-1 {
		t.Fatalf("OpenForInspection migrated the db: version=%d, want %d", v2, LatestMigrationVersion()-1)
	}
}

// TestMigrationSetsInLockstep asserts the sqlite and postgres migration dirs
// carry identical version numbers — LatestMigrationVersion (computed from the
// sqlite set) is only meaningful for both backends under this invariant.
func TestMigrationSetsInLockstep(t *testing.T) {
	versions := func(fsys fs.FS, dir string) map[int]bool {
		t.Helper()
		entries, err := fs.ReadDir(fsys, dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		out := map[int]bool{}
		for _, e := range entries {
			v, err := parseVersion(e.Name())
			if err != nil {
				t.Fatalf("parse %s: %v", e.Name(), err)
			}
			out[v] = true
		}
		return out
	}
	sq := versions(migrationsFS, "migrations")
	pg := versions(postgresMigrations, "migrations_postgres")
	if len(sq) != len(pg) {
		t.Fatalf("migration count differs: sqlite=%d postgres=%d", len(sq), len(pg))
	}
	for v := range sq {
		if !pg[v] {
			t.Errorf("version %d in sqlite set but not postgres", v)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/auth/sqlitestore/ -run 'TestSchemaVersionAndLatest|TestMigrationSetsInLockstep' -v`
Expected: FAIL (undefined: OpenForInspection / SchemaVersion / LatestMigrationVersion)

- [ ] **Step 3: Implement**

In `internal/auth/sqlitestore/store.go`, below `Open`:

```go
// OpenForInspection opens the metadata database WITHOUT applying pending
// migrations. Used by `bucketvcs doctor`, which must observe — not mutate —
// the schema state. Callers that intend to write should use Open.
//
// NOTE: the sqlite driver creates a missing database file on first use;
// doctor stats filesystem paths before calling this so a missing db is
// reported, not silently created.
func OpenForInspection(value string, opts ...Option) (*Store, error) {
	b, err := resolveBackend(value, opts...)
	if err != nil {
		return nil, err
	}
	db, err := b.Open()
	if err != nil {
		return nil, fmt.Errorf("open authdb (%s): %w", b.Name(), err)
	}
	return &Store{db: &dbWrap{db: db, backend: b}, backend: b}, nil
}
```

(Match `Open`'s actual `Store` literal shape — if `Open` sets additional fields, copy them.)

In `internal/auth/sqlitestore/schema.go`:

```go
// SchemaVersion returns the highest applied migration number, or 0 for a
// virgin database (schema_version table absent — the query error is treated
// as "no migrations", matching loadAppliedVersions' convention).
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&v); err != nil {
		return 0, nil
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}

// LatestMigrationVersion returns the highest migration number shipped in
// this binary, computed from the embedded sqlite set. The postgres set is
// numbered in lockstep (asserted by TestMigrationSetsInLockstep).
func LatestMigrationVersion() int {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return 0 // unreachable: embedded FS
	}
	max := 0
	for _, e := range entries {
		if v, err := parseVersion(e.Name()); err == nil && v > max {
			max = v
		}
	}
	return max
}
```

Add `"context"` and `"database/sql"` to schema.go's imports (it already has `"io/fs"` as `fs`).

- [ ] **Step 4: Run tests**

Run: `go test ./internal/auth/sqlitestore/ -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/auth/sqlitestore/store.go internal/auth/sqlitestore/schema.go \
        internal/auth/sqlitestore/inspection_test.go
git commit -m "feat(authdb): OpenForInspection + SchemaVersion/LatestMigrationVersion for doctor"
```

---

### Task 8: internal/doctor framework

**Files:**
- Create: `internal/doctor/doctor.go`
- Create: `internal/doctor/doctor_test.go`

- [ ] **Step 1: Write the failing test**

```go
package doctor_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/doctor"
)

func checks() []doctor.Check {
	return []doctor.Check{
		{Name: "alpha.ok", Run: func(context.Context) doctor.Result {
			return doctor.Result{Status: doctor.StatusOK, Detail: "fine"}
		}},
		{Name: "beta.warn", Run: func(context.Context) doctor.Result {
			return doctor.Result{Status: doctor.StatusWarn, Detail: "meh"}
		}},
		{Name: "gamma.fail", Run: func(context.Context) doctor.Result {
			return doctor.Result{Status: doctor.StatusFail, Detail: "broken thing"}
		}},
		{Name: "delta.skip", Run: func(context.Context) doctor.Result {
			return doctor.Result{Status: doctor.StatusSkip, Detail: "disabled"}
		}},
	}
}

func TestRunHumanOutput(t *testing.T) {
	var buf bytes.Buffer
	failed := doctor.Run(context.Background(), &buf, false, checks())
	if failed != 1 {
		t.Fatalf("failed=%d, want 1 (warn/skip must not count)", failed)
	}
	out := buf.String()
	for _, want := range []string{"OK", "alpha.ok", "WARN", "FAIL", "gamma.fail", "broken thing", "SKIP"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRunJSONOutput(t *testing.T) {
	var buf bytes.Buffer
	_ = doctor.Run(context.Background(), &buf, true, checks())
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("want 4 NDJSON lines, got %d:\n%s", len(lines), buf.String())
	}
	var first struct {
		Check  string `json:"check"`
		Status string `json:"status"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line 1 not JSON: %v", err)
	}
	if first.Check != "alpha.ok" || first.Status != "ok" || first.Detail != "fine" {
		t.Fatalf("unexpected first line: %+v", first)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/doctor/ -v`
Expected: FAIL (no such package yet → build error)

- [ ] **Step 3: Implement doctor.go**

```go
// Package doctor is the check framework behind `bucketvcs doctor`: a list of
// named read-only checks executed sequentially, reported one line per check
// (human or NDJSON). Checks themselves live in cmd/bucketvcs/doctor.go —
// they close over CLI flags and the cmd package's open helpers.
package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Status is a check outcome. Only StatusFail affects the exit code.
type Status string

const (
	StatusOK   Status = "ok"
	StatusWarn Status = "warn" // suspicious but not fatal (e.g. unsafe-no-sandbox)
	StatusFail Status = "fail"
	StatusSkip Status = "skip" // not applicable under this configuration
)

// Result is what a check returns.
type Result struct {
	Status Status
	Detail string
}

// Check is one named diagnostic.
type Check struct {
	Name string
	Run  func(ctx context.Context) Result
}

// jsonLine is the NDJSON shape (house style: one object per line).
type jsonLine struct {
	Check  string `json:"check"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// Run executes checks in order, writing one line per check to w. Returns the
// number of failed checks (callers exit 1 when > 0).
func Run(ctx context.Context, w io.Writer, jsonOut bool, checks []Check) int {
	failed := 0
	enc := json.NewEncoder(w)
	for _, c := range checks {
		res := c.Run(ctx)
		if res.Status == StatusFail {
			failed++
		}
		if jsonOut {
			_ = enc.Encode(jsonLine{Check: c.Name, Status: string(res.Status), Detail: res.Detail})
			continue
		}
		fmt.Fprintf(w, "%-5s %-24s %s\n", strings.ToUpper(string(res.Status)), c.Name, res.Detail)
	}
	return failed
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/doctor/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/doctor/
git commit -m "feat(doctor): check framework — sequential named checks, human + NDJSON output"
```

---

### Task 9: `bucketvcs doctor` command

**Files:**
- Create: `cmd/bucketvcs/doctor.go`
- Create: `cmd/bucketvcs/doctor_test.go`
- Modify: `cmd/bucketvcs/main.go` (dispatch + usage)

- [ ] **Step 1: Write the failing tests**

Create `cmd/bucketvcs/doctor_test.go`:

```go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

// doctorEnv creates a healthy localfs store dir + migrated auth.db and
// returns the doctor base args.
func doctorEnv(t *testing.T) (storeDir, dbPath string) {
	t.Helper()
	storeDir = t.TempDir()
	dbPath = filepath.Join(t.TempDir(), "auth.db")
	s, err := sqlitestore.Open(dbPath)
	if err != nil {
		t.Fatalf("seed authdb: %v", err)
	}
	s.Close()
	return storeDir, dbPath
}

func TestDoctorHealthy(t *testing.T) {
	storeDir, dbPath := doctorEnv(t)
	var out, errb bytes.Buffer
	code := run(context.Background(),
		[]string{"doctor", "--store", "localfs:" + storeDir, "--auth-db", dbPath, "--lfs=false"},
		&out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d, want 0\nstdout:\n%s\nstderr:\n%s", code, out.String(), errb.String())
	}
	for _, want := range []string{"storage.reachable", "storage.writable", "authdb.open", "authdb.migrations", "deps.git"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing check %q:\n%s", want, out.String())
		}
	}
}

func TestDoctorFailsOnLFSWithoutKey(t *testing.T) {
	storeDir, dbPath := doctorEnv(t)
	var out, errb bytes.Buffer
	code := run(context.Background(),
		[]string{"doctor", "--store", "localfs:" + storeDir, "--auth-db", dbPath, "--lfs=true"},
		&out, &errb)
	if code != 1 {
		t.Fatalf("exit=%d, want 1\nstdout:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "config.lfs") || !strings.Contains(out.String(), "FAIL") {
		t.Errorf("expected FAIL on config.lfs:\n%s", out.String())
	}
}

func TestDoctorFailsOnStaleMigrations(t *testing.T) {
	storeDir, dbPath := doctorEnv(t)
	s, err := sqlitestore.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(context.Background(),
		`DELETE FROM schema_version WHERE version=?`, sqlitestore.LatestMigrationVersion()); err != nil {
		t.Fatalf("simulate stale: %v", err)
	}
	s.Close()

	var out, errb bytes.Buffer
	code := run(context.Background(),
		[]string{"doctor", "--store", "localfs:" + storeDir, "--auth-db", dbPath, "--lfs=false"},
		&out, &errb)
	if code != 1 {
		t.Fatalf("exit=%d, want 1\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "authdb.migrations") {
		t.Errorf("expected authdb.migrations failure:\n%s", out.String())
	}
}

func TestDoctorMissingAuthDBDoesNotCreateIt(t *testing.T) {
	storeDir, _ := doctorEnv(t)
	missing := filepath.Join(t.TempDir(), "nope", "auth.db")
	var out, errb bytes.Buffer
	code := run(context.Background(),
		[]string{"doctor", "--store", "localfs:" + storeDir, "--auth-db", missing, "--lfs=false"},
		&out, &errb)
	if code != 1 {
		t.Fatalf("exit=%d, want 1\n%s", code, out.String())
	}
	if _, err := filepath.Glob(missing); err != nil {
		t.Fatal(err)
	}
	if fileExists(missing) {
		t.Fatal("doctor created the missing auth.db — it must be read-only")
	}
}

func TestDoctorJSON(t *testing.T) {
	storeDir, dbPath := doctorEnv(t)
	var out, errb bytes.Buffer
	code := run(context.Background(),
		[]string{"doctor", "--json", "--store", "localfs:" + storeDir, "--auth-db", dbPath, "--lfs=false"},
		&out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d\n%s\n%s", code, out.String(), errb.String())
	}
	for i, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("line %d not JSON: %v\n%s", i+1, err, line)
		}
	}
}

func fileExists(p string) bool {
	_, err := osStat(p)
	return err == nil
}
```

(Use `os.Stat` directly — replace `osStat` with `os.Stat` and import `"os"`; the helper indirection above is illustrative only, write it plainly.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./cmd/bucketvcs/ -run TestDoctor -v`
Expected: FAIL (unknown subcommand "doctor" → exit 2)

- [ ] **Step 3: Implement cmd/bucketvcs/doctor.go**

```go
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/doctor"
	"github.com/bucketvcs/bucketvcs/internal/gateway"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// runDoctor is `bucketvcs doctor`: read-only diagnostics over the same flag
// surface as serve ("swap serve for doctor"). It validates storage, the auth
// DB, config coherence, and host dependencies WITHOUT binding ports or
// mutating user data (the storage.writable probe PUTs and DELETEs one object
// under the reserved _doctor/ prefix).
//
// Exit codes: 0 all checks pass (warn/skip allowed), 1 any check fails,
// 2 usage error.
func runDoctor(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sf := registerServeFlags(fs)
	repoFlag := fs.String("repo", "", "Optional tenant/name to deep-check (manifest loads, schema gate, sampled storage keys exist)")
	asJSON := fs.Bool("json", false, "Emit one NDJSON object per check instead of the human table")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *sf.storeURL == "" {
		fmt.Fprintln(stderr, "doctor: --store is required")
		return 2
	}
	var repoTenant, repoName string
	if *repoFlag != "" {
		var ok bool
		repoTenant, repoName, ok = strings.Cut(*repoFlag, "/")
		if !ok || repoTenant == "" || repoName == "" {
			fmt.Fprintln(stderr, "doctor: --repo must be <tenant>/<name>")
			return 2
		}
	}

	// Shared lazily-opened store handle: storage.reachable opens it; later
	// storage checks skip when it is unavailable rather than re-failing.
	var store storage.ObjectStore
	defer func() {
		if store != nil {
			closeStore(store)
		}
	}()

	checks := []doctor.Check{
		{Name: "storage.reachable", Run: func(ctx context.Context) doctor.Result {
			s, err := openStore(*sf.storeURL)
			if err != nil {
				return doctor.Result{Status: doctor.StatusFail, Detail: err.Error()}
			}
			store = s
			if _, err := s.List(ctx, "", &storage.ListOptions{MaxKeys: 1}); err != nil {
				return doctor.Result{Status: doctor.StatusFail, Detail: "list: " + err.Error()}
			}
			return doctor.Result{Status: doctor.StatusOK, Detail: s.Name() + " backend, list ok"}
		}},
		{Name: "storage.writable", Run: func(ctx context.Context) doctor.Result {
			if store == nil {
				return doctor.Result{Status: doctor.StatusSkip, Detail: "store unavailable"}
			}
			var buf [8]byte
			if _, err := rand.Read(buf[:]); err != nil {
				return doctor.Result{Status: doctor.StatusFail, Detail: "rand: " + err.Error()}
			}
			key := "_doctor/probe-" + hex.EncodeToString(buf[:])
			ver, err := store.PutIfAbsent(ctx, key, strings.NewReader("bucketvcs doctor probe"), nil)
			if err != nil {
				return doctor.Result{Status: doctor.StatusFail, Detail: "put " + key + ": " + err.Error()}
			}
			if err := store.DeleteIfVersionMatches(ctx, key, ver); err != nil {
				return doctor.Result{Status: doctor.StatusFail, Detail: "cleanup delete " + key + ": " + err.Error()}
			}
			return doctor.Result{Status: doctor.StatusOK, Detail: "probe put+delete ok"}
		}},
		{Name: "authdb.open", Run: func(ctx context.Context) doctor.Result {
			path, err := resolveAuthDB(*sf.authDB, realEnv())
			if err != nil {
				return doctor.Result{Status: doctor.StatusFail, Detail: err.Error()}
			}
			// Filesystem-path backends (sqlite): stat first so a missing db
			// is reported, not created by the driver.
			if !strings.Contains(path, "://") {
				if _, err := os.Stat(path); err != nil {
					return doctor.Result{Status: doctor.StatusFail,
						Detail: "not found at " + path + " (created on first serve/user command)"}
				}
			}
			s, err := sqlitestore.OpenForInspection(path, sqlitestore.WithMaxConns(*sf.authDBMaxConns))
			if err != nil {
				return doctor.Result{Status: doctor.StatusFail, Detail: err.Error()}
			}
			defer s.Close()
			var one int
			if err := s.DB().QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
				return doctor.Result{Status: doctor.StatusFail, Detail: "ping: " + err.Error()}
			}
			return doctor.Result{Status: doctor.StatusOK, Detail: path}
		}},
		{Name: "authdb.migrations", Run: func(ctx context.Context) doctor.Result {
			path, err := resolveAuthDB(*sf.authDB, realEnv())
			if err != nil {
				return doctor.Result{Status: doctor.StatusSkip, Detail: "authdb unavailable"}
			}
			if !strings.Contains(path, "://") {
				if _, err := os.Stat(path); err != nil {
					return doctor.Result{Status: doctor.StatusSkip, Detail: "authdb unavailable"}
				}
			}
			s, err := sqlitestore.OpenForInspection(path, sqlitestore.WithMaxConns(*sf.authDBMaxConns))
			if err != nil {
				return doctor.Result{Status: doctor.StatusSkip, Detail: "authdb unavailable"}
			}
			defer s.Close()
			have, err := s.SchemaVersion(ctx)
			if err != nil {
				return doctor.Result{Status: doctor.StatusFail, Detail: err.Error()}
			}
			want := sqlitestore.LatestMigrationVersion()
			switch {
			case have == want:
				return doctor.Result{Status: doctor.StatusOK, Detail: fmt.Sprintf("schema version %d (current)", have)}
			case have < want:
				return doctor.Result{Status: doctor.StatusFail,
					Detail: fmt.Sprintf("db at version %d, binary expects %d — any serve/CLI command that opens the auth-db will migrate it", have, want)}
			default:
				return doctor.Result{Status: doctor.StatusFail,
					Detail: fmt.Sprintf("db at version %d, binary only knows %d — this binary is older than the database", have, want)}
			}
		}},
		{Name: "config.lfs", Run: func(ctx context.Context) doctor.Result {
			if !*sf.lfsEnabled {
				return doctor.Result{Status: doctor.StatusSkip, Detail: "--lfs=false"}
			}
			if *sf.proxiedKeyFile == "" || *sf.proxiedBaseURL == "" {
				return doctor.Result{Status: doctor.StatusFail,
					Detail: "--lfs=true requires both --proxied-url-signing-key and --proxied-url-base (serve refuses to start)"}
			}
			return doctor.Result{Status: doctor.StatusOK, Detail: "signing key + base URL configured"}
		}},
		{Name: "config.proxied", Run: func(ctx context.Context) doctor.Result {
			bMode, ok := gateway.ParseURIMode(*sf.bundleURIMode)
			if !ok {
				return doctor.Result{Status: doctor.StatusFail, Detail: "--bundle-uri-mode=" + *sf.bundleURIMode + " not auto|direct|proxied|off"}
			}
			pMode, ok := gateway.ParseURIMode(*sf.packURIMode)
			if !ok {
				return doctor.Result{Status: doctor.StatusFail, Detail: "--pack-uri-mode=" + *sf.packURIMode + " not auto|direct|proxied|off"}
			}
			needsKey := bMode == gateway.URIModeAuto || bMode == gateway.URIModeProxied ||
				pMode == gateway.URIModeAuto || pMode == gateway.URIModeProxied ||
				(*sf.lfsEnabled && *sf.proxiedKeyFile != "" && *sf.proxiedBaseURL != "")
			if !needsKey {
				return doctor.Result{Status: doctor.StatusSkip, Detail: "no proxied/auto URI mode configured"}
			}
			if *sf.proxiedKeyFile == "" || *sf.proxiedBaseURL == "" {
				return doctor.Result{Status: doctor.StatusFail, Detail: "proxied/auto mode requires --proxied-url-signing-key and --proxied-url-base"}
			}
			raw, err := os.ReadFile(*sf.proxiedKeyFile)
			if err != nil {
				return doctor.Result{Status: doctor.StatusFail, Detail: "read signing key: " + err.Error()}
			}
			if len(strings.TrimSpace(string(raw))) < 16 {
				return doctor.Result{Status: doctor.StatusFail, Detail: fmt.Sprintf("signing key too short (%d bytes); need >= 16", len(strings.TrimSpace(string(raw))))}
			}
			u, err := url.Parse(*sf.proxiedBaseURL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return doctor.Result{Status: doctor.StatusFail, Detail: "--proxied-url-base must be an http(s) URL with a host"}
			}
			return doctor.Result{Status: doctor.StatusOK, Detail: "signing key readable, base URL parses"}
		}},
		{Name: "config.hooks", Run: func(ctx context.Context) doctor.Result {
			if !*sf.hooksEnabled {
				return doctor.Result{Status: doctor.StatusSkip, Detail: "--hooks-enabled=false"}
			}
			if *sf.hooksRoot == "" || !filepath.IsAbs(*sf.hooksRoot) {
				return doctor.Result{Status: doctor.StatusFail, Detail: "--hooks-root must be a non-empty absolute path"}
			}
			if st, err := os.Stat(*sf.hooksRoot); err != nil || !st.IsDir() {
				return doctor.Result{Status: doctor.StatusFail, Detail: "--hooks-root is not an existing directory: " + *sf.hooksRoot}
			}
			if *sf.hooksUnsafeNoSandbox {
				return doctor.Result{Status: doctor.StatusWarn, Detail: "running without bwrap sandbox — NOT multi-tenant safe"}
			}
			if runtime.GOOS != "linux" {
				return doctor.Result{Status: doctor.StatusFail, Detail: "sandboxed hooks need Linux (bwrap); set --hooks-unsafe-no-sandbox=true elsewhere"}
			}
			p, err := exec.LookPath("bwrap")
			if err != nil {
				return doctor.Result{Status: doctor.StatusFail, Detail: "bwrap not on PATH (install bubblewrap) and --hooks-unsafe-no-sandbox=false"}
			}
			return doctor.Result{Status: doctor.StatusOK, Detail: "bwrap at " + p}
		}},
		{Name: "deps.git", Run: func(ctx context.Context) doctor.Result {
			p, err := exec.LookPath("git")
			if err != nil {
				return doctor.Result{Status: doctor.StatusFail, Detail: "git not on PATH (import/export/maintenance need it)"}
			}
			vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			out, err := exec.CommandContext(vctx, p, "version").Output()
			if err != nil {
				return doctor.Result{Status: doctor.StatusFail, Detail: "git version: " + err.Error()}
			}
			return doctor.Result{Status: doctor.StatusOK, Detail: strings.TrimSpace(string(out))}
		}},
	}

	if *repoFlag != "" {
		checks = append(checks, doctor.Check{
			Name: "repo." + repoTenant + "/" + repoName,
			Run: func(ctx context.Context) doctor.Result {
				if store == nil {
					return doctor.Result{Status: doctor.StatusSkip, Detail: "store unavailable"}
				}
				r, err := repo.Open(ctx, store, repoTenant, repoName)
				if errors.Is(err, repo.ErrRepoNotFound) {
					return doctor.Result{Status: doctor.StatusFail, Detail: "repo not found"}
				}
				if errors.Is(err, repo.ErrUnsupportedSchema) {
					return doctor.Result{Status: doctor.StatusFail, Detail: "schema gate: " + err.Error()}
				}
				if err != nil {
					return doctor.Result{Status: doctor.StatusFail, Detail: err.Error()}
				}
				view, err := r.ReadRoot(ctx)
				if err != nil {
					return doctor.Result{Status: doctor.StatusFail, Detail: "read root: " + err.Error()}
				}
				body, err := manifest.UnmarshalBody(view.Body)
				if err != nil {
					return doctor.Result{Status: doctor.StatusFail, Detail: "parse body: " + err.Error()}
				}
				// Sample manifest-referenced keys (cap 50) and Head each.
				keys := make([]string, 0, 50)
				for _, p := range body.Packs {
					keys = append(keys, p.PackKey, p.IdxKey)
				}
				for _, sh := range body.RefShards {
					keys = append(keys, sh.Key)
				}
				const sampleCap = 50
				if len(keys) > sampleCap {
					keys = keys[:sampleCap]
				}
				for _, k := range keys {
					if k == "" {
						continue
					}
					if _, err := store.Head(ctx, k); err != nil {
						return doctor.Result{Status: doctor.StatusFail,
							Detail: fmt.Sprintf("manifest references missing key %s: %v", k, err)}
					}
				}
				return doctor.Result{Status: doctor.StatusOK,
					Detail: fmt.Sprintf("schema v%d, %d sampled keys present", view.Header.SchemaVersion, len(keys))}
			},
		})
	}

	if failed := doctor.Run(ctx, stdout, *asJSON, checks); failed > 0 {
		fmt.Fprintf(stderr, "doctor: %d check(s) failed\n", failed)
		return 1
	}
	return 0
}
```

Adjust to reality while implementing (compile will tell you): `Store.DB()` is the Querier accessor used elsewhere in cmd (`grep -n "\.DB()" cmd/bucketvcs/*.go`), `WithMaxConns` exists (serve.go:252), and `view.Header.SchemaVersion` is shown by inspect.go:150. If `repo.Open` already calls `ReadRoot` semantics differ, mirror inspect.go's exact sequence (inspect.go:103-110).

- [ ] **Step 4: Register the subcommand**

In `cmd/bucketvcs/main.go`:

```go
	case "doctor":
		return runDoctor(ctx, rest, stdout, stderr)
```

(insert after the `"gc"` case), and add to the usage text after the `gc` line:

```
  doctor             Read-only health checks: storage, auth-db, config coherence, host deps
```

- [ ] **Step 5: Run tests**

Run: `go test ./cmd/bucketvcs/ -run TestDoctor -v`
Expected: PASS (all 5). Then the full package: `go test ./cmd/bucketvcs/ -count=1` → PASS.

- [ ] **Step 6: Manual sanity check**

```bash
go build -o /tmp/bucketvcs ./cmd/bucketvcs
mkdir -p /tmp/bvdoc/store
/tmp/bucketvcs doctor --store localfs:/tmp/bvdoc/store --auth-db /tmp/bvdoc/auth.db --lfs=false; echo "exit=$?"
```
Expected: `authdb.open` FAILs (db doesn't exist — doctor must NOT create it), exit=1, and `ls /tmp/bvdoc/auth.db` → No such file. Then create it (`/tmp/bucketvcs user add --auth-db /tmp/bvdoc/auth.db alice` or any authdb-opening command) and re-run → exit=0.

- [ ] **Step 7: Commit**

```bash
git add cmd/bucketvcs/doctor.go cmd/bucketvcs/doctor_test.go cmd/bucketvcs/main.go
git commit -m "feat(cli): bucketvcs doctor — read-only diagnostics over the serve flag surface"
```

---

### Task 10: Documentation + final verification

**Files:**
- Create: `docs/operator-guides/doctor.md`
- Modify: `docs/operator-guides/webhooks.md` (Egress policy section + deferred-items list)
- Modify: `README.md` (command list / features mention)
- Modify: `docs/operator-guides/postgres.md` (note repo delete works)

- [ ] **Step 1: Write docs/operator-guides/doctor.md**

```markdown
# bucketvcs doctor

Read-only health checks for a bucketvcs deployment. `doctor` accepts the same
flags as `serve` — take your serve command line, swap `serve` for `doctor`,
and it validates the configuration without binding any ports.

```bash
bucketvcs doctor \
  --store s3://my-bucket/prefix \
  --auth-db /var/lib/bucketvcs/bucketvcs.db \
  --lfs=true --proxied-url-signing-key /etc/bucketvcs/urlkey --proxied-url-base https://gw.example
```

Output is one line per check; exit 0 when nothing failed, 1 when any check
failed, 2 on usage errors. `warn` and `skip` do not affect the exit code.
`--json` emits one NDJSON object per check (`{"check":..,"status":..,"detail":..}`).

## Checks

| Check | What it verifies |
|---|---|
| `storage.reachable` | the `--store` URL opens and a List succeeds |
| `storage.writable` | a probe object PUTs and DELETEs under the reserved `_doctor/` prefix — the only write doctor ever makes; user data is never touched |
| `authdb.open` | the auth DB exists and answers `SELECT 1` (sqlite paths are stat'ed first — doctor never creates a missing db) |
| `authdb.migrations` | applied schema version matches what this binary ships; flags both stale-db and binary-older-than-db |
| `config.lfs` | `--lfs=true` has its required `--proxied-url-signing-key` + `--proxied-url-base` |
| `config.proxied` | URI modes parse; signing key file is readable and >= 16 bytes; base URL is a valid http(s) URL |
| `config.hooks` | hooks root exists; bwrap present (warns when `--hooks-unsafe-no-sandbox=true`) |
| `deps.git` | git CLI on PATH (import/export/maintenance shell out to it) |
| `repo.<t>/<n>` | with `--repo tenant/name`: manifest loads, schema gate passes, up to 50 manifest-referenced pack/ref-shard keys exist in the bucket |

`doctor` applies no migrations and repairs nothing; it observes. Repair
tooling (`--fix`) is a possible future extension on the same check framework.
```

- [ ] **Step 2: Add the Egress policy section to webhooks.md**

In `docs/operator-guides/webhooks.md`, add a new section (after the delivery/backoff material; pick the numbering that fits the existing section sequence and update the table of contents if one exists):

```markdown
## Egress policy (SSRF protection)

The delivery worker refuses to connect to private and internal addresses by
default. The check runs at **dial time on the resolved IP**, so DNS-rebinding
(a hostname that resolves clean at registration and to 169.254.169.254 at
delivery) is covered. Denied by default:

- loopback (`127.0.0.0/8`, `::1`)
- link-local (`169.254.0.0/16` — includes cloud metadata endpoints — and `fe80::/10`)
- private ranges (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, ULA `fc00::/7`)
- unspecified, multicast, and broadcast addresses

**To deliver to a receiver in a private range** (a LAN Jenkins, an internal
service), punch a hole with the repeatable flag:

```bash
bucketvcs serve ... --webhook-allow-cidr=192.168.1.0/24
```

`--webhook-allow-cidr=0.0.0.0/0` restores the fully-open pre-M25 behavior.

**To deny by hostname** — useful for internal names that resolve to *public*
IPs (split-horizon DNS, internal apps behind public load balancers):

```bash
bucketvcs serve ... --webhook-deny-host='*.corp.example.com' --webhook-deny-host=metadata.google.internal
```

Hostname rules are policy, not a security boundary: a raw IP or an alternate
DNS name pointing at the same box bypasses them. The IP-range check is the
real gate. There is deliberately no allow-by-hostname flag — allowing a name
to resolve into denied IP space would re-open DNS rebinding.

A denied delivery is recorded as a normal failure: it follows the standard
backoff schedule and dead-letters after 5 attempts, with
`last_error = "egress denied: ..."` naming the flag to fix. If you add the
missing `--webhook-allow-cidr` and restart, parked deliveries succeed on
their next scheduled attempt — no manual replay needed.

Additional hardening in the delivery client: HTTP redirects are not followed
(a 3xx response is a failed attempt), proxy environment variables are
ignored (a proxy would dial on the worker's behalf and bypass the policy),
and endpoint URLs must be http/https.

Registration of an endpoint whose URL contains a **literal** denied IP is
rejected up front in the web UI (which knows the live policy) and warned
about by `bucketvcs webhook endpoint add` (a separate process that cannot
know serve's flags).

Observability: `webhook_egress_denied_total` metric (deliberately unlabeled —
endpoint URLs are attacker-influenced) and the `webhooks.egress_denied` audit
event (`delivery_id`, `endpoint_id`, `host`, `ip`, `denied_by`, `pattern`).

> **Upgrade note (breaking):** deployments delivering webhooks to private or
> loopback receivers MUST add `--webhook-allow-cidr` covering those receivers
> when upgrading to this version; their deliveries will otherwise park in
> retry and dead-letter after ~14.5h.
```

Also update the deferred/out-of-scope list in webhooks.md §1/§11: remove or amend any line implying egress filtering is missing, and add "per-endpoint / per-tenant egress policy" to the deferred list.

- [ ] **Step 3: README + postgres.md touch-ups**

- `README.md`: add `doctor` wherever the CLI commands are listed (grep for `inspect-manifest` to find the spot), one line: "`bucketvcs doctor` — read-only health checks for storage, auth-db, and config."
- `docs/operator-guides/postgres.md`: in the section listing Postgres capabilities/limitations, add a line that `bucketvcs repo delete` (and web-UI repo delete) is fully supported as of M25 (migration 0015). Grep the file for any stale "not supported on postgres" phrasing first: `grep -rn "sqlite/libsql" docs/`.

- [ ] **Step 4: Sweep for stale references**

Run: `grep -rn "requires a sqlite/libsql auth-db\|refused on Postgres\|ErrCascadeUnsupportedBackend" docs/ README.md`
Expected: no hits in docs (code references in internal/ are fine).

- [ ] **Step 5: Full verification**

```bash
gofmt -l .                 # expect: empty
go vet ./...               # expect: clean
go vet -tags postgres ./internal/auth/sqlitestore/
go build ./...             # expect: clean
go test ./... -count=1     # expect: all PASS
```

- [ ] **Step 6: Commit**

```bash
git add docs/operator-guides/doctor.md docs/operator-guides/webhooks.md \
        docs/operator-guides/postgres.md README.md
git commit -m "docs(m25): doctor operator guide, webhook egress policy section, pg repo-delete notes"
```

---

## Self-review checklist (run after all tasks)

1. **Spec coverage:** A → Tasks 1-2; B → Tasks 4-6; C → Tasks 3, 7-9; docs → Task 10. Deferred items (notarization, --fix, allow-host) intentionally absent.
2. **Spec deviations (already approved in plan-writing):** (a) the spec's "smokes gain --webhook-allow-cidr" became "worker tests gain the allow hole" — no smoke exercises delivery (verified: phase3 registers `https://example.invalid/hook`, never delivers); (b) the spec's "CLI rejects literal denied IPs" became warn-for-CLI / reject-for-web-UI, because the CLI process cannot know serve's allow-cidr flags. Both noted in the design's spirit (dial-time is the real gate).
3. **Cross-task type consistency:** `EgressPolicy`/`EgressDeniedError`/`NewHTTPClient` (Task 4) used by Tasks 5-6; `serveFlags`/`registerServeFlags` (Task 3) used by Task 9; `OpenForInspection`/`SchemaVersion`/`LatestMigrationVersion` (Task 7) used by Task 9; `cascadeStmts` shared within Task 2.
