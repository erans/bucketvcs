# M23 Phase A — Turso / libSQL metadata backend — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let operators back the bucketvcs metadata/auth DB (`internal/auth/sqlitestore`) with Turso / libSQL instead of a local SQLite file, selected by the `--auth-db` value's scheme — without changing any SQL, migration, or store consumer.

**Architecture:** Introduce a small `Backend` seam inside `internal/auth/sqlitestore` capturing the only things that differ between SQLite and libSQL: how to open the `*sql.DB` (driver + DSN + pool) and how to apply a migration file (whole-body vs statement-split). libSQL speaks SQLite's SQL dialect, so the ~45 store methods and all 10 migration `.sql` files are unchanged. `Open` resolves a backend from the `--auth-db` scheme; everything else is identical at runtime.

**Tech Stack:** Go 1.25, `modernc.org/sqlite` (existing, pure-Go SQLite), `github.com/tursodatabase/libsql-client-go/libsql` (new, pure-Go libSQL client — must stay `CGO_ENABLED=0`), `database/sql`.

**Spec:** `docs/superpowers/specs/2026-05-26-m23-turso-libsql-backend-design.md`

---

## Conventions & refinements

- All commands run from the repo/worktree root.
- **Refinement of spec §2.1:** error classification stays as the existing package-level free functions (`isUniqueViolation`, `isCheckViolation`, `isFingerprintUniqueViolation` in `store.go`) rather than `Backend` methods. libSQL surfaces the same SQLite error text, so a single broadened matcher serves both backends and avoids threading the backend through ~a dozen call sites. Task 1 captures libSQL's exact error strings; Task 4 broadens the matchers if (and only if) they differ; Task 6's conformance suite proves `ErrConflict`/CHECK mapping on libSQL. The `Backend` interface is therefore `Name() / Open() / ApplyMigration()`.
- **`Open` keeps its single-string signature** so every caller (`cmd/bucketvcs` subcommands, `serve`, lfs locks, tests) is unchanged.
- TDD where there's logic to assert; the seam-extraction task is behavior-preserving and verified by the existing suite.

## File structure

**New files:**
- `internal/auth/sqlitestore/backend.go` — `Backend` interface, `resolveBackend`, `sqliteBackend`.
- `internal/auth/sqlitestore/backend_test.go` — `resolveBackend` + token-resolution unit tests.
- `internal/auth/sqlitestore/backend_libsql.go` — `libsqlBackend` (driver, DSN, token, conn setup, migration split).
- `internal/auth/sqlitestore/sqlsplit.go` — `splitSQLStatements`.
- `internal/auth/sqlitestore/sqlsplit_test.go` — splitter unit tests over all 10 migrations.
- `internal/auth/sqlitestore/conformance_backend_test.go` — `//go:build libsql` behavioral suite vs a live libSQL (sqld).
- `docs/m23-turso-operator-guide.md` — operator guide.

**Modified files:**
- `internal/auth/sqlitestore/store.go` — `Store` gains `backend Backend`; `Open` delegates to `resolveBackend` + `backend.Open()`; remove the inline DSN building (moves into `sqliteBackend`).
- `internal/auth/sqlitestore/schema.go` — `RunMigrations(db, backend)`; `applyOne` uses `backend.ApplyMigration`.
- `go.mod` / `go.sum` — add the libSQL client.
- `.github/workflows/conformance.yml` — add a nightly `libsql` job (sqld in a container).

---

## Task 1: Spike — add the libSQL driver and pin its behaviors

**Files:**
- Modify: `go.mod`, `go.sum`

This task de-risks the design. It adds the dependency, proves it's pure-Go, and records four findings the later tasks rely on. It commits only the dependency; findings go in the commit message and inform Tasks 3–6.

- [ ] **Step 1: Add the pure-Go libSQL client**

Run:
```bash
go get github.com/tursodatabase/libsql-client-go/libsql@latest
go mod tidy
```
Expected: it resolves. Confirm in `go.mod` it is the `libsql-client-go` module (the pure-Go HTTP client), NOT `github.com/tursodatabase/go-libsql` (the cgo embedded-replica driver). If `go get` pulls any cgo-requiring transitive dependency, STOP and report.

- [ ] **Step 2: Prove `CGO_ENABLED=0` cross-compilation still works with the driver imported**

Create a throwaway file `internal/auth/sqlitestore/zz_spike_libsql.go` that blank-imports the driver so it's linked:
```go
package sqlitestore

import _ "github.com/tursodatabase/libsql-client-go/libsql"
```

Run (this is the release-pipeline gate):
```bash
for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do
  os=${t%/*}; arch=${t#*/}
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -o /dev/null ./cmd/bucketvcs && echo "OK $t" || echo "FAIL $t"
done
```
Expected: `OK` for all five. If any `FAIL` with a cgo error, the pure-Go assumption is wrong — STOP and report (the feature is not viable under the current release constraints).

- [ ] **Step 3: Bring up a local sqld and capture the four findings**

Run sqld (the open-source libSQL server) locally:
```bash
docker run -d --name bv-sqld -p 8080:8080 ghcr.io/tursodatabase/libsql-server:latest
# sqld serves an HTTP endpoint; the pure-Go client connects via http://127.0.0.1:8080
```

Write a temporary `internal/auth/sqlitestore/zz_spike_test.go` (DELETED at end of task) that connects with `sql.Open("libsql", "http://127.0.0.1:8080")` and records:
1. **Multi-statement Exec:** does `db.Exec("CREATE TABLE a(x); CREATE TABLE b(y);")` succeed, or error on the second statement? (Determines whether the splitter is required or merely defensive — we ship it regardless.)
2. **FK enforcement:** does `db.Exec("PRAGMA foreign_keys = ON")` succeed, and are FK violations then enforced?
3. **UNIQUE error string:** insert a duplicate into a `UNIQUE` column; record `err.Error()` verbatim.
4. **CHECK error string:** violate a `CHECK`; record `err.Error()` verbatim.

- [ ] **Step 4: Clean up the spike, record findings, commit the dependency**

Delete `zz_spike_libsql.go` and `zz_spike_test.go`. Stop the container: `docker rm -f bv-sqld`.

Run: `go build ./... && go test ./internal/auth/sqlitestore/`
Expected: unchanged (spike files removed; only go.mod/go.sum differ).

```bash
git add go.mod go.sum
git commit -m "M23: add pure-Go libSQL client dependency

Spike findings (feed Tasks 3-6):
- CGO_ENABLED=0 cross-build OK for all 5 targets.
- multi-statement Exec: <supported|one-statement-only>.
- PRAGMA foreign_keys=ON: <accepted>; FK enforcement: <on|off by default>.
- UNIQUE err string: \"<verbatim>\".
- CHECK err string: \"<verbatim>\"."
```
Record the actual findings in the commit message — Tasks 4 and 6 reference them.

---

## Task 2: `Backend` seam + `sqliteBackend` (behavior-preserving extraction)

**Files:**
- Create: `internal/auth/sqlitestore/backend.go`
- Modify: `internal/auth/sqlitestore/store.go`, `internal/auth/sqlitestore/schema.go`

This refactor changes no behavior — the existing test suite is the assertion.

- [ ] **Step 1: Create the `Backend` interface + `sqliteBackend` + `resolveBackend`**

Create `internal/auth/sqlitestore/backend.go`:

```go
package sqlitestore

import (
	"database/sql"
	"fmt"
	"net/url"
	"strings"

	_ "modernc.org/sqlite"
)

// Backend abstracts the driver-specific concerns that differ between the
// SQLite (modernc) and libSQL (Turso) backends. Phase B adds a postgres
// implementation plus a SQL-dialect helper for the divergent statements.
type Backend interface {
	// Name reports the backend for logging: "sqlite" | "libsql".
	Name() string
	// Open opens the *sql.DB with this backend's driver, DSN, and pool
	// config. It does NOT run migrations.
	Open() (*sql.DB, error)
	// ApplyMigration executes one migration file body within tx.
	ApplyMigration(tx *sql.Tx, body string) error
}

// resolveBackend selects a Backend from the --auth-db value. A recognized URL
// scheme (libsql/http/https) selects libSQL; anything else (bare path, file:,
// sqlite:, or a Windows drive path) is a filesystem path → SQLite.
func resolveBackend(value string) (Backend, error) {
	if isLibsqlValue(value) {
		// Wired to newLibsqlBackend in Task 4 (backend_libsql.go).
		return nil, fmt.Errorf("libsql backend not yet available")
	}
	return sqliteBackend{path: sqlitePath(value)}, nil
}

// isLibsqlValue reports whether value is a libSQL/Turso URL.
func isLibsqlValue(value string) bool {
	u, err := url.Parse(value)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "libsql", "http", "https":
		return true
	default:
		return false
	}
}

// sqlitePath strips a leading sqlite:/file: scheme if present, else returns
// value unchanged (a bare filesystem path).
func sqlitePath(value string) string {
	if u, err := url.Parse(value); err == nil {
		switch strings.ToLower(u.Scheme) {
		case "sqlite", "file":
			if u.Opaque != "" {
				return u.Opaque
			}
			return u.Path
		}
	}
	return value
}

// sqliteBackend is the default modernc.org/sqlite backend — exactly the
// behavior shipped before M23.
type sqliteBackend struct{ path string }

func (sqliteBackend) Name() string { return "sqlite" }

func (b sqliteBackend) Open() (*sql.DB, error) {
	u := &url.URL{
		Scheme: "file",
		Opaque: (&url.URL{Path: b.path}).EscapedPath(),
	}
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	u.RawQuery = q.Encode()

	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	// Single writer connection simplifies WAL semantics (low write
	// concurrency, many concurrent reads).
	db.SetMaxOpenConns(1)
	return db, nil
}

// ApplyMigration execs the whole multi-statement body — modernc.org/sqlite
// supports this, and it is the proven pre-M23 path.
func (sqliteBackend) ApplyMigration(tx *sql.Tx, body string) error {
	_, err := tx.Exec(body)
	return err
}
```

- [ ] **Step 2: Wire `Store` + `Open` + `RunMigrations`**

In `internal/auth/sqlitestore/store.go`:

Change the struct:
```go
// Store is the SQL-backed implementation of auth.Store.
type Store struct {
	db      *sql.DB
	backend Backend
}
```

Replace the body of `Open` (keep the signature `func Open(path string) (*Store, error)`; the parameter is now the `--auth-db` value, which may be a path or a URL):
```go
// Open opens (or creates) the metadata database identified by value and
// applies any pending migrations. value is a filesystem path (SQLite, the
// default) or a libsql://… / https://… URL (Turso/libSQL). See resolveBackend.
func Open(value string) (*Store, error) {
	b, err := resolveBackend(value)
	if err != nil {
		return nil, err
	}
	db, err := b.Open()
	if err != nil {
		return nil, fmt.Errorf("open authdb (%s): %w", b.Name(), err)
	}
	if err := RunMigrations(db, b); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	slog.Default().Info("authdb opened", "backend", b.Name())
	return &Store{db: db, backend: b}, nil
}
```
Add `"log/slog"` to the store.go imports. The `_ "modernc.org/sqlite"` blank import and the `net/url` import move to / are shared with `backend.go`; ensure `store.go` still compiles (remove `net/url` from store.go if no longer used there, keep it in backend.go).

In `internal/auth/sqlitestore/schema.go`, change `RunMigrations` and `applyOne`:
```go
func RunMigrations(db *sql.DB, backend Backend) error {
	// ... unchanged until the applyOne call ...
		if err := applyOne(db, backend, string(body)); err != nil {
			return fmt.Errorf("apply migration %q: %w", name, err)
		}
	// ...
}

func applyOne(db *sql.DB, backend Backend, body string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if err := backend.ApplyMigration(tx, body); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
```

- [ ] **Step 3: Update other `RunMigrations` callers**

Run: `grep -rn 'RunMigrations(' internal/auth/sqlitestore/` (and the wider tree).
For each caller other than `Open` (e.g. `schema_test.go`), pass a backend. In tests that build a bare schema, pass `sqliteBackend{path: ...}` or refactor through `Open`. Update each to compile.

- [ ] **Step 4: Verify behavior is unchanged**

Run: `go build ./... && go test ./internal/auth/...`
Expected: PASS — the full existing suite, unchanged. This is the assertion that the extraction preserved behavior.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/sqlitestore/backend.go internal/auth/sqlitestore/store.go internal/auth/sqlitestore/schema.go
# plus any updated test callers
git commit -m "M23: Backend seam + sqliteBackend (behavior-preserving extraction)"
```

---

## Task 3: SQL statement splitter

**Files:**
- Create: `internal/auth/sqlitestore/sqlsplit.go`, `internal/auth/sqlitestore/sqlsplit_test.go`

libSQL-over-HTTP may not accept a multi-statement `Exec`, so `libsqlBackend` applies migrations statement-by-statement. The splitter is built and unit-tested here independently of any DB.

- [ ] **Step 1: Write the failing test**

Create `internal/auth/sqlitestore/sqlsplit_test.go`:

```go
package sqlitestore

import (
	"embed"
	"strings"
	"testing"
)

func TestSplitSQLStatements_Basic(t *testing.T) {
	in := `
-- a comment
CREATE TABLE a (x TEXT);

CREATE INDEX a_idx ON a(x);
INSERT INTO a (x) VALUES ('hi');
`
	got := splitSQLStatements(in)
	if len(got) != 3 {
		t.Fatalf("want 3 statements, got %d: %#v", len(got), got)
	}
	for _, s := range got {
		if strings.TrimSpace(s) == "" {
			t.Fatalf("empty statement in %#v", got)
		}
		if strings.HasPrefix(strings.TrimSpace(s), "--") {
			t.Fatalf("comment leaked as statement: %q", s)
		}
	}
}

func TestSplitSQLStatements_AllMigrationsNonEmpty(t *testing.T) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		count++
		body, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		stmts := splitSQLStatements(string(body))
		if len(stmts) == 0 {
			t.Fatalf("%s: split to zero statements", e.Name())
		}
		for _, s := range stmts {
			ts := strings.TrimSpace(s)
			if ts == "" || strings.HasPrefix(ts, "--") {
				t.Fatalf("%s: bad statement %q", e.Name(), s)
			}
		}
	}
	if count != 10 {
		t.Fatalf("expected 10 migration files, saw %d", count)
	}
}

var _ embed.FS // migrationsFS is declared in schema.go
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/auth/sqlitestore/ -run TestSplitSQLStatements`
Expected: FAIL — `splitSQLStatements` undefined.

- [ ] **Step 3: Implement the splitter**

Create `internal/auth/sqlitestore/sqlsplit.go`:

```go
package sqlitestore

import "strings"

// splitSQLStatements splits a migration file body into individual statements
// for backends (libSQL/HTTP) that execute one statement per Exec.
//
// It is intentionally conservative and handles exactly the SQL our migration
// files use: statement-terminating ';', line comments ('-- …'), and string
// literals ('…') which may themselves contain ';'. Our migrations contain NO
// trigger / BEGIN…END blocks (a dollar-quote / compound-statement case this
// splitter does NOT handle); TestSplitSQLStatements_AllMigrationsNonEmpty
// guards that assumption. Trailing/empty/comment-only fragments are dropped.
func splitSQLStatements(body string) []string {
	var stmts []string
	var cur strings.Builder
	inLineComment := false
	inString := false

	runes := []rune(body)
	for i := 0; i < len(runes); i++ {
		c := runes[i]

		if inLineComment {
			cur.WriteRune(c)
			if c == '\n' {
				inLineComment = false
			}
			continue
		}
		if inString {
			cur.WriteRune(c)
			if c == '\'' {
				// '' is an escaped quote inside a string literal.
				if i+1 < len(runes) && runes[i+1] == '\'' {
					cur.WriteRune(runes[i+1])
					i++
					continue
				}
				inString = false
			}
			continue
		}

		switch {
		case c == '-' && i+1 < len(runes) && runes[i+1] == '-':
			inLineComment = true
			cur.WriteRune(c)
		case c == '\'':
			inString = true
			cur.WriteRune(c)
		case c == ';':
			if s := normalizeStmt(cur.String()); s != "" {
				stmts = append(stmts, s)
			}
			cur.Reset()
		default:
			cur.WriteRune(c)
		}
	}
	if s := normalizeStmt(cur.String()); s != "" {
		stmts = append(stmts, s)
	}
	return stmts
}

// normalizeStmt trims whitespace and drops fragments that are empty or consist
// only of line comments (so a comment block between statements is not exec'd).
func normalizeStmt(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	hasSQL := false
	for _, line := range strings.Split(s, "\n") {
		l := strings.TrimSpace(line)
		if l == "" || strings.HasPrefix(l, "--") {
			continue
		}
		hasSQL = true
		break
	}
	if !hasSQL {
		return ""
	}
	return s
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/auth/sqlitestore/ -run TestSplitSQLStatements -v`
Expected: PASS (both tests; the all-migrations test confirms every file splits into ≥1 non-empty, non-comment statement).

- [ ] **Step 5: Commit**

```bash
git add internal/auth/sqlitestore/sqlsplit.go internal/auth/sqlitestore/sqlsplit_test.go
git commit -m "M23: conservative SQL statement splitter for per-statement migration apply"
```

---

## Task 4: `libsqlBackend`

**Files:**
- Create: `internal/auth/sqlitestore/backend_libsql.go`
- Modify: `internal/auth/sqlitestore/store.go` (broaden error matchers if Task 1 found different strings)

- [ ] **Step 1: Implement `libsqlBackend`**

Create `internal/auth/sqlitestore/backend_libsql.go`:

```go
package sqlitestore

import (
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"

	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

// dbAuthTokenEnv is the environment variable holding the libSQL/Turso auth
// token. The token is NEVER passed as a CLI argument (it would leak via ps).
const dbAuthTokenEnv = "BUCKETVCS_DB_AUTH_TOKEN"

// libsqlBackend is the pure-Go libSQL (Turso) backend. Remote only — embedded
// replicas require the cgo go-libsql driver, which would break CGO_ENABLED=0
// cross-compilation.
type libsqlBackend struct {
	dsn string // full libSQL DSN including auth token
}

// newLibsqlBackend builds the backend from the --auth-db URL, resolving the
// auth token from BUCKETVCS_DB_AUTH_TOKEN (preferred) or an authToken query
// param already on the URL. The token is OPTIONAL: self-hosted sqld over
// http(s) commonly runs without auth (and the conformance suite targets such
// an instance), so we do not hard-fail when it is missing. We warn for the
// libsql:// scheme (Turso almost always needs one) and let the connection
// surface a clear auth error if the server actually requires it.
func newLibsqlBackend(rawURL string) (Backend, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("libsql: parse url: %w", err)
	}
	q := u.Query()
	if envTok := os.Getenv(dbAuthTokenEnv); envTok != "" {
		q.Set("authToken", envTok) // env takes precedence over any in-URL token
	}
	if q.Get("authToken") == "" && strings.EqualFold(u.Scheme, "libsql") {
		slog.Default().Warn("libsql URL has no auth token; set "+dbAuthTokenEnv+" if the server requires one",
			"host", u.Host)
	}
	u.RawQuery = q.Encode()
	return libsqlBackend{dsn: u.String()}, nil
}

func (libsqlBackend) Name() string { return "libsql" }

func (b libsqlBackend) Open() (*sql.DB, error) {
	db, err := sql.Open("libsql", b.dsn)
	if err != nil {
		return nil, err
	}
	// Phase A: single connection preserves the single-writer serialization
	// the current store code assumes (quota ring-lock, webhook claim, …).
	// Multi-node concurrency hardening + pooling is Phase B.
	db.SetMaxOpenConns(1)
	// Remote sqld ignores the modernc _pragma DSN syntax; enforce FKs via a
	// statement (sqld honors it — confirmed in Task 1).
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("libsql: enable foreign_keys: %w", err)
	}
	return db, nil
}

// ApplyMigration applies the body one statement at a time within tx, since the
// libSQL HTTP driver may not accept a multi-statement Exec.
func (libsqlBackend) ApplyMigration(tx *sql.Tx, body string) error {
	for _, stmt := range splitSQLStatements(body) {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("libsql: exec statement %q: %w", truncate(stmt, 80), err)
		}
	}
	return nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
```

- [ ] **Step 2: Wire `resolveBackend` to the libSQL backend**

In `internal/auth/sqlitestore/backend.go`, replace the Task-2 stub in `resolveBackend`'s libsql branch:
```go
	if isLibsqlValue(value) {
		return newLibsqlBackend(value)
	}
```
(Remove the `fmt.Errorf("libsql backend not yet available")` stub. If `fmt` is now unused in `backend.go`, drop it from that file's imports.)

- [ ] **Step 3: Broaden error matchers if Task 1 found different strings**

If the Task 1 spike recorded UNIQUE/CHECK error strings that do NOT already contain `"UNIQUE constraint failed"` / `"CHECK constraint failed"`, add the libSQL substrings to the matchers in `store.go`. For example, if libSQL wraps as `"SQLite error: UNIQUE constraint failed: …"`, the existing `strings.Contains(err.Error(), "UNIQUE constraint failed")` already matches — no change. Only if the wording is genuinely different (e.g. `"SQLITE_CONSTRAINT_UNIQUE"`) add it:

```go
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "constraint failed: UNIQUE") ||
		strings.Contains(msg, "SQLITE_CONSTRAINT_UNIQUE") // libSQL (Task 1)
}
```
Apply the same broadening to `isCheckViolation` and `isFingerprintUniqueViolation` only as Task 1's findings require. If Task 1 showed identical strings, make NO change here and note that in the commit. Task 6 is the safety net that proves the mapping on a live libSQL.

- [ ] **Step 4: Build + verify existing suite + cross-compile gate**

Run:
```bash
go build ./... && go test ./internal/auth/...
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o /dev/null ./cmd/bucketvcs && echo "windows cross-build OK"
```
Expected: PASS; windows cross-build OK (re-confirms pure-Go with the driver fully wired).

- [ ] **Step 5: Commit**

```bash
git add internal/auth/sqlitestore/backend_libsql.go internal/auth/sqlitestore/store.go internal/auth/sqlitestore/backend.go
git commit -m "M23: libsqlBackend (pure-Go libSQL: DSN+token, FK pragma, per-statement migrations)"
```

---

## Task 5: `resolveBackend` + token-resolution unit tests

**Files:**
- Create: `internal/auth/sqlitestore/backend_test.go`

- [ ] **Step 1: Write the tests**

Create `internal/auth/sqlitestore/backend_test.go`:

```go
package sqlitestore

import "testing"

func TestResolveBackend_Selection(t *testing.T) {
	cases := []struct {
		value    string
		wantName string
	}{
		{"/var/lib/bucketvcs/auth.db", "sqlite"},
		{"auth.db", "sqlite"},
		{"sqlite:/tmp/a.db", "sqlite"},
		{"file:/tmp/a.db", "sqlite"},
		{`C:\data\auth.db`, "sqlite"}, // Windows drive path, not a URL scheme
		{"libsql://db.turso.io", "libsql"},
		{"https://db.turso.io", "libsql"},
	}
	t.Setenv(dbAuthTokenEnv, "") // token is optional; selection must not depend on it
	for _, c := range cases {
		b, err := resolveBackend(c.value)
		if err != nil {
			t.Fatalf("%s: %v", c.value, err)
		}
		if b.Name() != c.wantName {
			t.Fatalf("%s: backend=%s want %s", c.value, b.Name(), c.wantName)
		}
	}
}

func TestResolveBackend_LibsqlNoTokenOK(t *testing.T) {
	// Token is optional (self-hosted sqld may run without auth); a libsql URL
	// with no token must still resolve, not error.
	t.Setenv(dbAuthTokenEnv, "")
	b, err := resolveBackend("http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("no-token libsql URL should resolve: %v", err)
	}
	if b.Name() != "libsql" {
		t.Fatalf("backend=%s want libsql", b.Name())
	}
}

func TestResolveBackend_TokenFromURLAllowedWithoutEnv(t *testing.T) {
	t.Setenv(dbAuthTokenEnv, "")
	b, err := resolveBackend("libsql://db.turso.io?authToken=abc")
	if err != nil {
		t.Fatalf("token in URL should be accepted: %v", err)
	}
	if b.Name() != "libsql" {
		t.Fatalf("backend=%s", b.Name())
	}
}

func TestSqlitePath_StripsScheme(t *testing.T) {
	if got := sqlitePath("sqlite:/tmp/a.db"); got != "/tmp/a.db" {
		t.Fatalf("sqlitePath sqlite: = %q", got)
	}
	if got := sqlitePath("/tmp/a.db"); got != "/tmp/a.db" {
		t.Fatalf("sqlitePath bare = %q", got)
	}
}
```

- [ ] **Step 2: Run to verify**

Run: `go test ./internal/auth/sqlitestore/ -run 'TestResolveBackend|TestSqlitePath' -v`
Expected: PASS. (If `C:\data\auth.db` resolves to libsql, fix `isLibsqlValue` — a Windows drive letter `C:` parses with scheme `c`; the switch only matches `libsql/http/https`, so it correctly falls through to sqlite. The test guards this.)

- [ ] **Step 3: Commit**

```bash
git add internal/auth/sqlitestore/backend_test.go
git commit -m "M23: backend resolution + token-resolution unit tests"
```

---

## Task 6: Backend conformance suite (gated, libSQL)

**Files:**
- Create: `internal/auth/sqlitestore/conformance_backend_test.go`

A behavioral suite that runs end-to-end against a live libSQL (sqld), proving Turso is a true drop-in. Gated by `//go:build libsql` so it never runs in the per-commit suite; the nightly job (Task 7) supplies `BUCKETVCS_LIBSQL_URL`.

- [ ] **Step 1: Write the suite**

Create `internal/auth/sqlitestore/conformance_backend_test.go`:

```go
//go:build libsql

package sqlitestore

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// openLibsql opens a fresh-schema libSQL store from BUCKETVCS_LIBSQL_URL.
// The CI job points this at a per-run sqld instance (empty DB).
func openLibsql(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("BUCKETVCS_LIBSQL_URL")
	if url == "" {
		t.Skip("BUCKETVCS_LIBSQL_URL not set")
	}
	s, err := Open(url)
	if err != nil {
		t.Fatalf("open libsql: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.backend.Name() != "libsql" {
		t.Fatalf("backend=%s, want libsql", s.backend.Name())
	}
	return s
}

// TestLibsqlConformance exercises the core behaviors end-to-end on libSQL and
// asserts they match the documented SQLite behavior, including the
// error-classification mapping (ErrConflict on UNIQUE, CHECK enforcement) and
// the FK cascade that Phase B's concurrency work will build on.
func TestLibsqlConformance(t *testing.T) {
	s := openLibsql(t)
	ctx := context.Background()

	// Migrations applied: the reserved _oidc user exists.
	if _, err := s.GetUserByName(ctx, "_oidc"); err != nil {
		t.Fatalf("migrations did not apply (no _oidc user): %v", err)
	}

	// User + token lifecycle.
	if _, err := s.CreateUser(ctx, "alice", false); err != nil {
		t.Fatalf("create user: %v", err)
	}
	// UNIQUE on users.name → ErrConflict (proves error classification on libSQL).
	if _, err := s.CreateUser(ctx, "alice", false); !errors.Is(err, auth.ErrConflict) {
		t.Fatalf("dup user: want ErrConflict, got %v", err)
	}

	// Repo register + grant + perm lookup.
	if err := s.RegisterRepo(ctx, "acme", "web"); err != nil {
		t.Fatalf("register repo: %v", err)
	}
	u, err := s.GetUserByName(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Grant(ctx, "alice", "acme", "web", "write"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	actor := &auth.Actor{UserID: u.ID, Name: "alice"}
	perm, err := s.LookupRepoPerm(ctx, actor, "acme", "web")
	if err != nil || perm != auth.PermWrite {
		t.Fatalf("perm=%v err=%v want write", perm, err)
	}

	// Token create + verify.
	tok, id, secret, err := auth.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	hash, err := auth.HashSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateToken(ctx, id, u.ID, hash, "lap", nil, auth.ScopeRepoWrite, "", "", ""); err != nil {
		t.Fatalf("create token: %v", err)
	}
	gotActor, _, _, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "alice", Password: tok})
	if err != nil || gotActor == nil || gotActor.Name != "alice" {
		t.Fatalf("verify: actor=%v err=%v", gotActor, err)
	}

	// CHECK enforcement: scope_perm CHECK on tokens (migration 0010).
	exp := time.Now().Unix() + 900
	err = s.CreateToken(ctx, "BADPERMTOKEN0000000000AA", "_oidc", hash, "x", &exp,
		auth.ScopeRepoRead, "acme", "web", "BOGUS")
	if err == nil {
		t.Fatal("CHECK on scope_perm should reject 'BOGUS'")
	}

	// OIDC mint round-trips (exercises migration 0010 tables + token binding).
	mint, err := s.MintOIDCToken(ctx, MintOIDCParams{
		Tenant: "acme", Repo: "web", Perm: auth.PermWrite,
		Scopes: auth.ScopeRepoWrite, TTLSeconds: 900, Label: "oidc:gh:sub",
	})
	if err != nil {
		t.Fatalf("mint oidc: %v", err)
	}
	if _, _, scope, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "x", Password: mint}); err != nil || scope == nil || scope.Repo != "web" {
		t.Fatalf("verify minted: scope=%v err=%v", scope, err)
	}

	// FK cascade: deleting the repo removes its permission rows.
	if err := s.DeleteRepo(ctx, "acme", "web"); err != nil {
		t.Fatalf("delete repo: %v", err)
	}
	if perm, _ := s.LookupRepoPerm(ctx, actor, "acme", "web"); perm != auth.PermNone {
		t.Fatalf("after repo delete, perm=%v want none (cascade)", perm)
	}
}
```

Note: method names above are verified against the current store (`CreateUser(name,isAdmin)(id,error)`, `RegisterRepo`, `Grant(userName,tenant,repo,permString)`, `LookupRepoPerm`, `DeleteRepo`, `CreateToken`, `MintOIDCToken`, `GetUserByName`). If a signature has since changed, grep `func (s *Store)` in store.go/oidc.go and adjust — the intent (each behavior + error mapping + cascade) is what matters.

- [ ] **Step 2: Verify it builds under the tag and skips without a URL**

Run: `go test -tags libsql ./internal/auth/sqlitestore/ -run TestLibsqlConformance -v`
Expected: it compiles and SKIPs (no `BUCKETVCS_LIBSQL_URL`). Also confirm the default (untagged) build is unaffected: `go test ./internal/auth/sqlitestore/`.

- [ ] **Step 3: Run it against a local sqld (developer check)**

```bash
docker run -d --name bv-sqld -p 8080:8080 ghcr.io/tursodatabase/libsql-server:latest
BUCKETVCS_LIBSQL_URL="http://127.0.0.1:8080" go test -tags libsql ./internal/auth/sqlitestore/ -run TestLibsqlConformance -v
docker rm -f bv-sqld
```
Expected: PASS. If error classification fails here (UNIQUE/CHECK not mapped), return to Task 4 Step 2 and broaden the matchers with the observed strings.

- [ ] **Step 4: Commit**

```bash
git add internal/auth/sqlitestore/conformance_backend_test.go
git commit -m "M23: gated libSQL conformance suite (behavioral parity vs sqld)"
```

---

## Task 7: Nightly CI job for the libSQL conformance

**Files:**
- Modify: `.github/workflows/conformance.yml`

The `conformance` workflow already runs nightly + on demand (M-prev B(b) change). Add a job that boots sqld and runs the gated suite.

- [ ] **Step 1: Add the job**

Append to `.github/workflows/conformance.yml` under `jobs:`:

```yaml
  libsql:
    name: libsql (sqld) conformance
    runs-on: ubuntu-latest
    timeout-minutes: 15
    services:
      sqld:
        image: ghcr.io/tursodatabase/libsql-server:latest
        ports:
          - 8080:8080
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: "1.25"
          cache: true
      - name: Wait for sqld
        run: |
          for i in $(seq 1 30); do
            if curl -fsS http://127.0.0.1:8080/health >/dev/null 2>&1 || curl -fsS http://127.0.0.1:8080 >/dev/null 2>&1; then
              echo "sqld up"; exit 0
            fi
            sleep 1
          done
          echo "sqld did not become ready"; exit 1
      - name: libSQL conformance
        env:
          BUCKETVCS_LIBSQL_URL: "http://127.0.0.1:8080"
        run: go test -tags libsql -count=1 ./internal/auth/sqlitestore/ -run TestLibsqlConformance -v
```

Note: confirm sqld's readiness probe path during implementation (the `/health` endpoint may differ by image version; the fallback `curl http://127.0.0.1:8080` covers the common case). Pin the image to a specific stable tag rather than `:latest` once a working tag is known.

- [ ] **Step 2: Validate the workflow YAML locally**

Run: `python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/conformance.yml')); print('yaml ok')"`
Expected: `yaml ok`.

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/conformance.yml
git commit -m "M23: nightly libSQL (sqld) conformance CI job"
```

---

## Task 8: Operator guide

**Files:**
- Create: `docs/m23-turso-operator-guide.md`

- [ ] **Step 1: Write the guide**

Create `docs/m23-turso-operator-guide.md` covering, in the style of the other `docs/m*-operator-guide.md` files:
- **What it is:** back the metadata DB with Turso/libSQL; Git data stays in object storage; SQLite remains the zero-dependency default.
- **Enabling:** set `--auth-db libsql://<db>.turso.io` (or `https://…`) and export `BUCKETVCS_DB_AUTH_TOKEN=<token>`. Applies to `serve` and every `bucketvcs` subcommand that takes `--auth-db`. A worked example with `turso db create` / `turso db tokens create`.
- **How selection works:** bare path / `sqlite:` / `file:` → SQLite; `libsql://` / `https://` → libSQL. The token is read from the env var, never the command line.
- **Caveats (prominent):** (1) remote libSQL only — no embedded replica / offline mode (pure-Go / `CGO_ENABLED=0`); (2) single-node only for now — multi-node-safe webhook claiming and quota updates are Phase B; (3) `MaxOpenConns(1)` throughput bound; (4) rate-limiter stays per-node.
- **Migrating existing SQLite data:** dump and load out of band (`turso db shell <db> < dump.sql`), then point `--auth-db` at the libSQL URL.
- **Verifying:** `serve` logs `authdb opened backend=libsql` at startup.

- [ ] **Step 2: Commit**

```bash
git add docs/m23-turso-operator-guide.md
git commit -m "M23: Turso/libSQL operator guide"
```

---

## Final verification

- [ ] **Per-commit suite + build + cross-compile**

Run:
```bash
go build ./... && go test ./internal/auth/... && go vet ./internal/auth/...
for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do
  CGO_ENABLED=0 GOOS=${t%/*} GOARCH=${t#*/} go build -o /dev/null ./cmd/bucketvcs && echo "OK $t"; done
```
Expected: all PASS; all five `OK`.

- [ ] **Gated libSQL suite (with sqld)** — as in Task 6 Step 3: PASS.

- [ ] **Update memory index** — add an `m23_…` topic file + a short MEMORY.md line once merged (MEMORY.md is near its size cap; keep the line short).

---

## Self-review notes (for the implementer)

- **Spec coverage:** Backend seam + sqliteBackend (Task 2) ↔ spec §2.1/§3; resolveBackend + token (Tasks 2,5) ↔ §2.2/§4; libsqlBackend driver/DSN/PRAGMA/migration-split (Tasks 3,4) ↔ §4; Task-0 spike (Task 1) ↔ §5; error classification refinement (Task 4 Step 2) ↔ §2.1-refinement; conformance suite (Tasks 6,7) ↔ §9.2; unit tests (Tasks 3,5) ↔ §9.1; cross-compile gate (Tasks 1,4,Final) ↔ §9.3; caveats + guide (Task 8) ↔ §8; acceptance ↔ §10.
- **The error-classification deviation from spec §2.1 is intentional and documented** (free functions broadened, not Backend methods) — confirmed less invasive and validated by Task 6.
- **`Open` signature is unchanged**, so no store consumer changes — re-verify with `grep -rn 'sqlitestore.Open(' --include='*.go'` after Task 2 (all should still compile).
- **Known follow-on (Phase B):** Postgres backend + SQL-dialect layer; multi-node concurrency hardening; pooling > 1; embedded replicas remain out (cgo).
