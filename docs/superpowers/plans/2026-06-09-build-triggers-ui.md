# Build Triggers UI — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a repo-settings **Triggers** tab (and CLI/backend support) that lets repo admins manage M30/M31 build triggers from the browser, with paginated delivery history and bounded replay.

**Architecture:** Extend the existing Phase-3 settings chassis (`reposettings.go` tab switch) with a new `triggers` tab, exactly mirroring the webhooks tab's handler/template/ownership patterns. Add four methods to `buildtrigger.Service` (`Edit`, `RotateSecret`, `ListDeliveriesPage`, `RecentDeliveryIDs`) plus a connector-names helper; wire the service and connector names into `web.Deps` via `serve.go`. Interactivity uses htmx progressive enhancement with a no-JS fallback, per the existing `browse.go` ref-switcher precedent.

**Tech Stack:** Go, `database/sql` over the M4 authdb (sqlite/postgres via `sqlitestore.Querier`), `html/template`, htmx (already bundled), the existing `internal/web` chassis helpers.

**Spec:** `docs/superpowers/specs/2026-06-09-build-triggers-ui-design.md`

**Reference reading before starting (do not skip):**
- `internal/web/reposettings_webhooks.go` — the handler pattern this tab clones (dispatcher, `ownEndpointOr404`, secret-once, deliveries, replay ownership two-step).
- `internal/web/reposettings.go` — `parseSettingsPath`, `handleRepoSettings` tab switch, `settingsRoute`.
- `internal/buildtrigger/store.go` + `delivery.go` + `types.go` — Service shape, `scanTrigger`/`scanDelivery`, validation in `Create`.
- `internal/web/services.go` + `internal/web/handler.go` (lines 30–120) — `Deps`/`server` wiring pattern.
- `cmd/bucketvcs/build.go` — CLI dispatch + `runBuildTriggerAdd` flag shape.

**Route-shape note (deviation from spec §2):** `settingsRoute` parses at most 5 path segments (`/{t}/{r}/settings/{tab}/{action}`), so the replay POST is a **flat** action `…/settings/triggers/replay` (with hidden `trigger_id` + `delivery_id`), NOT nested under `…/deliveries/replay`. This matches the webhooks tab exactly. The deliveries GET stays `…/settings/triggers/deliveries?trigger=<id>`.

---

## Task 1: `buildtrigger.Service.Edit` + `EditInput`

Adds in-place editing of safe fields (kind/config/secret untouched).

**Files:**
- Modify: `internal/buildtrigger/types.go` (add `EditInput`)
- Modify: `internal/buildtrigger/store.go` (add `Edit`)
- Test: `internal/buildtrigger/edit_test.go` (create)

- [ ] **Step 1: Write the failing test**

Create `internal/buildtrigger/edit_test.go`. Use the same in-memory authdb harness the existing store tests use (copy the `newTestService(t)` helper call from `store_test.go`; if it is named differently, match the existing test file's setup verbatim).

```go
package buildtrigger

import (
	"context"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func TestEdit_UpdatesSafeFieldsLeavesKindAndSecret(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t) // same helper used by store_test.go

	created, err := s.Create(ctx, TriggerInput{
		Tenant: "acme", Repo: "widgets", Name: "ci", Kind: KindCloudBuild,
		Config: Config{URL: "https://example.com/h"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	updated, err := s.Edit(ctx, created.ID, EditInput{
		Name:        "ci-renamed",
		RefInclude:  []string{"refs/heads/main"},
		RefExclude:  []string{},
		TokenMode:   TokenInject,
		TokenScopes: auth.ScopeRepoRead,
		TokenTTL:    30 * time.Minute,
		Active:      false,
	})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if updated.Name != "ci-renamed" {
		t.Errorf("name not updated: %q", updated.Name)
	}
	if updated.Kind != KindCloudBuild {
		t.Errorf("kind changed: %q", updated.Kind)
	}
	if updated.Active {
		t.Errorf("active not updated")
	}
	if len(updated.RefInclude) != 1 || updated.RefInclude[0] != "refs/heads/main" {
		t.Errorf("ref_include not updated: %v", updated.RefInclude)
	}
	if updated.TokenTTL != 30*time.Minute {
		t.Errorf("ttl not updated: %v", updated.TokenTTL)
	}
	// Secret is preserved (still has a preview), and never returned on edit.
	if updated.SecretPreview == "" {
		t.Errorf("secret preview lost after edit")
	}
}

func TestEdit_DuplicateNameConflict(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	if _, err := s.Create(ctx, TriggerInput{Tenant: "acme", Repo: "w", Name: "a", Kind: KindGeneric, Config: Config{URL: "https://x"}}); err != nil {
		t.Fatal(err)
	}
	b, err := s.Create(ctx, TriggerInput{Tenant: "acme", Repo: "w", Name: "b", Kind: KindGeneric, Config: Config{URL: "https://y"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Edit(ctx, b.ID, EditInput{Name: "a", RefInclude: []string{}, RefExclude: []string{}, Active: true})
	if err == nil || !errorsIs(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
}

func TestEdit_TTLOverCeilingRejected(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	c, err := s.Create(ctx, TriggerInput{Tenant: "acme", Repo: "w", Name: "a", Kind: KindGeneric, Config: Config{URL: "https://x"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Edit(ctx, c.ID, EditInput{Name: "a", RefInclude: []string{}, RefExclude: []string{}, TokenTTL: 2 * time.Hour, Active: true})
	if err == nil || !errorsIs(err, ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput, got %v", err)
	}
}

func TestEdit_NotFound(t *testing.T) {
	s := newTestService(t)
	_, err := s.Edit(context.Background(), "bvbt_missing", EditInput{Name: "x", RefInclude: []string{}, RefExclude: []string{}, Active: true})
	if err == nil || !errorsIs(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// errorsIs is a tiny local alias to avoid importing errors in every test file;
// if store_test.go already imports "errors" and uses errors.Is directly, delete
// this and use errors.Is instead.
func errorsIs(err, target error) bool { return errors.Is(err, target) }
```

If `store_test.go` already uses `errors.Is`, replace the `errorsIs` helper + calls with direct `errors.Is` and add the `errors` import. Verify the real helper name for service construction (`newTestService`) by reading `internal/buildtrigger/store_test.go` first.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/buildtrigger/ -run TestEdit -v`
Expected: FAIL — `s.Edit undefined` / `EditInput` undefined.

- [ ] **Step 3: Add `EditInput` to `types.go`**

Append after `TriggerInput` (around line 89 of `internal/buildtrigger/types.go`):

```go
// EditInput is the operator-supplied data for Edit. Kind, Config (url/secret),
// Tenant, and Repo are immutable on edit — change kind by delete+recreate.
type EditInput struct {
	Name        string
	RefInclude  []string
	RefExclude  []string
	TokenMode   TokenMode
	TokenScopes auth.TokenScope
	TokenTTL    time.Duration
	Active      bool
}
```

- [ ] **Step 4: Implement `Edit` in `store.go`**

Add after `Get` (around line 222 of `internal/buildtrigger/store.go`):

```go
// Edit updates the safe (non-kind, non-config) fields of an existing trigger:
// name, ref globs, token mode/scopes/ttl, active. Kind, URL, and secret are
// left untouched (change kind by delete+recreate). Normalization mirrors
// Create (ttl 0 => default, mode "" => kind-aware default, scopes 0 =>
// repo:read|lfs:read). Returns the updated Trigger (Secret hidden), ErrNotFound
// if no row matched, ErrConflict on a name collision, ErrInvalidInput on bad
// input.
func (s *Service) Edit(ctx context.Context, id string, in EditInput) (Trigger, error) {
	existing, err := s.Get(ctx, id) // ErrNotFound propagates
	if err != nil {
		return Trigger{}, err
	}
	if in.Name == "" || !routenames.ValidateName(in.Name) {
		return Trigger{}, fmt.Errorf("%w: invalid name %q", ErrInvalidInput, in.Name)
	}
	ttl := in.TokenTTL
	if ttl < 0 || ttl > TokenCeiling {
		return Trigger{}, fmt.Errorf("%w: token ttl %v out of range (0, %v]", ErrInvalidInput, ttl, TokenCeiling)
	}
	if ttl == 0 {
		ttl = defaultTokenTTL
	}
	mode := in.TokenMode
	if mode == "" {
		if existing.Kind == KindCodeBuild || existing.Kind == KindAzurePipelines {
			mode = TokenInject
		} else {
			mode = TokenNone
		}
	}
	switch mode {
	case TokenNone, TokenInject:
	default:
		return Trigger{}, fmt.Errorf("%w: unknown token mode %q", ErrInvalidInput, mode)
	}
	scopes := in.TokenScopes
	if scopes == 0 {
		scopes = auth.ScopeRepoRead | auth.ScopeLFSRead
	}
	for _, pat := range in.RefInclude {
		if err := policy.ValidatePathPattern(pat); err != nil {
			return Trigger{}, fmt.Errorf("%w: ref_include %q: %s", ErrInvalidInput, pat, err.Error())
		}
	}
	for _, pat := range in.RefExclude {
		if err := policy.ValidatePathPattern(pat); err != nil {
			return Trigger{}, fmt.Errorf("%w: ref_exclude %q: %s", ErrInvalidInput, pat, err.Error())
		}
	}
	refIncJSON, err := json.Marshal(nonNil(in.RefInclude))
	if err != nil {
		return Trigger{}, fmt.Errorf("buildtrigger: marshal ref_include: %w", err)
	}
	refExcJSON, err := json.Marshal(nonNil(in.RefExclude))
	if err != nil {
		return Trigger{}, fmt.Errorf("buildtrigger: marshal ref_exclude: %w", err)
	}
	active := 0
	if in.Active {
		active = 1
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE build_triggers
		   SET name=?, ref_include=?, ref_exclude=?,
		       token_mode=?, token_scopes=?, token_ttl_seconds=?, active=?
		 WHERE id=?`,
		in.Name, refIncJSON, refExcJSON,
		string(mode), int64(scopes), int64(ttl/time.Second), active, id)
	if err != nil {
		if s.db.IsUniqueViolation(err) {
			return Trigger{}, ErrConflict
		}
		return Trigger{}, fmt.Errorf("buildtrigger: edit %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Trigger{}, fmt.Errorf("buildtrigger: edit %s rows affected: %w", id, err)
	}
	if n == 0 {
		return Trigger{}, ErrNotFound
	}
	return s.Get(ctx, id)
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/buildtrigger/ -run TestEdit -v`
Expected: PASS (all four).

- [ ] **Step 6: Commit**

```bash
git add internal/buildtrigger/types.go internal/buildtrigger/store.go internal/buildtrigger/edit_test.go
git commit -m "feat(buildtrigger): Service.Edit for safe-field in-place edits"
```

---

## Task 2: `buildtrigger.Service.RotateSecret`

Regenerates the HMAC secret for `generic`/`cloudbuild`; errors on other kinds.

**Files:**
- Modify: `internal/buildtrigger/store.go`
- Test: `internal/buildtrigger/rotate_test.go` (create)

- [ ] **Step 1: Write the failing test**

```go
package buildtrigger

import (
	"context"
	"errors"
	"testing"
)

func TestRotateSecret_GenericReturnsNewSecret(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	c, err := s.Create(ctx, TriggerInput{Tenant: "acme", Repo: "w", Name: "a", Kind: KindGeneric, Config: Config{URL: "https://x"}})
	if err != nil {
		t.Fatal(err)
	}
	oldPreview := c.Secret[:6]
	secret, err := s.RotateSecret(ctx, c.ID)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if secret == "" || secret[:6] == oldPreview {
		t.Errorf("secret not rotated: old=%s new=%s", oldPreview, secret)
	}
	got, err := s.Get(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SecretPreview != secret[:6] {
		t.Errorf("stored preview %q != new secret prefix %q", got.SecretPreview, secret[:6])
	}
}

func TestRotateSecret_CodeBuildRejected(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	c, err := s.Create(ctx, TriggerInput{Tenant: "acme", Repo: "w", Name: "a", Kind: KindCodeBuild, Config: Config{AWSRegion: "us-east-1", AWSProject: "p"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RotateSecret(ctx, c.ID); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput, got %v", err)
	}
}

func TestRotateSecret_NotFound(t *testing.T) {
	s := newTestService(t)
	if _, err := s.RotateSecret(context.Background(), "bvbt_x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/buildtrigger/ -run TestRotateSecret -v`
Expected: FAIL — `s.RotateSecret undefined`.

- [ ] **Step 3: Implement `RotateSecret`**

Add after `Edit` in `store.go`:

```go
// RotateSecret regenerates the HMAC shared secret for a generic/cloudbuild
// trigger and returns it once. Errors with ErrInvalidInput for kinds with no
// server-owned secret (codebuild/azurepipelines use connector creds;
// azurewebhook's secret is operator-supplied). Returns ErrNotFound if absent.
func (s *Service) RotateSecret(ctx context.Context, id string) (string, error) {
	var kind string
	var configJSON []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT kind, config_json FROM build_triggers WHERE id=?`, id).Scan(&kind, &configJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("buildtrigger: rotate %s read: %w", id, err)
	}
	if Kind(kind) != KindGeneric && Kind(kind) != KindCloudBuild {
		return "", fmt.Errorf("%w: rotate-secret only applies to generic/cloudbuild triggers", ErrInvalidInput)
	}
	var cfg Config
	if len(configJSON) > 0 {
		if err := json.Unmarshal(configJSON, &cfg); err != nil {
			return "", fmt.Errorf("buildtrigger: rotate %s decode config: %w", id, err)
		}
	}
	secret, err := generateSecret()
	if err != nil {
		return "", fmt.Errorf("buildtrigger: generate secret: %w", err)
	}
	cfg.Secret = secret
	newJSON, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("buildtrigger: rotate %s marshal config: %w", id, err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE build_triggers SET config_json=? WHERE id=?`, newJSON, id); err != nil {
		return "", fmt.Errorf("buildtrigger: rotate %s update: %w", id, err)
	}
	return secret, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/buildtrigger/ -run TestRotateSecret -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/buildtrigger/store.go internal/buildtrigger/rotate_test.go
git commit -m "feat(buildtrigger): Service.RotateSecret for generic/cloudbuild"
```

---

## Task 3: `buildtrigger.Service.ListDeliveriesPage` (keyset pagination)

New keyset-paginated sibling of `ListDeliveries`. Leaves the existing method untouched (CLI depends on it).

**Files:**
- Modify: `internal/buildtrigger/delivery.go`
- Test: `internal/buildtrigger/delivery_page_test.go` (create)

- [ ] **Step 1: Write the failing test**

The deliveries table is populated by the enqueue/worker path, which is awkward to drive in a unit test. Insert rows directly via the service's db handle using a small test helper. Read `internal/buildtrigger/enqueue_test.go` (or `delivery_test.go`) first to copy how existing tests insert delivery rows; if they expose a helper, reuse it. Otherwise use this direct-insert helper:

```go
package buildtrigger

import (
	"context"
	"testing"
	"time"
)

// insertDelivery is a test-only helper that writes a delivery row with an
// explicit created_at so keyset ordering is deterministic.
func insertDelivery(t *testing.T, s *Service, id, triggerID, status string, createdAt int64) {
	t.Helper()
	_, err := s.db.ExecContext(context.Background(),
		`INSERT INTO build_trigger_deliveries
		   (id, trigger_id, status, attempts, next_attempt_at, created_at)
		 VALUES (?, ?, ?, 0, ?, ?)`,
		id, triggerID, status, createdAt, createdAt)
	if err != nil {
		t.Fatalf("insert delivery: %v", err)
	}
}

func TestListDeliveriesPage_KeysetOrderAndLimit(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	base := time.Now().Unix()
	// 5 rows, created_at ascending d1..d5
	for i, id := range []string{"d1", "d2", "d3", "d4", "d5"} {
		insertDelivery(t, s, id, "bvbt_t", "delivered", base+int64(i))
	}
	// First page: newest 2 → d5, d4
	page1, err := s.ListDeliveriesPage(ctx, "bvbt_t", "", time.Time{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 2 || page1[0].ID != "d5" || page1[1].ID != "d4" {
		t.Fatalf("page1 = %v", ids(page1))
	}
	// Second page: before d4's created_at → d3, d2
	cursor := page1[1].CreatedAt
	page2, err := s.ListDeliveriesPage(ctx, "bvbt_t", "", cursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 2 || page2[0].ID != "d3" || page2[1].ID != "d2" {
		t.Fatalf("page2 = %v", ids(page2))
	}
}

func TestListDeliveriesPage_StatusFilter(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	base := time.Now().Unix()
	insertDelivery(t, s, "a", "bvbt_t", "delivered", base+1)
	insertDelivery(t, s, "b", "bvbt_t", "dead_letter", base+2)
	got, err := s.ListDeliveriesPage(ctx, "bvbt_t", "dead_letter", time.Time{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("status filter = %v", ids(got))
	}
}

func ids(ds []Delivery) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.ID
	}
	return out
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/buildtrigger/ -run TestListDeliveriesPage -v`
Expected: FAIL — `s.ListDeliveriesPage undefined`.

- [ ] **Step 3: Implement `ListDeliveriesPage`**

Add after `ListDeliveries` in `delivery.go`:

```go
// ListDeliveriesPage returns deliveries for one trigger, newest first, using
// keyset pagination. When before is non-zero, only rows strictly older than it
// (created_at < before, with id as the tie-break) are returned. status narrows
// when non-empty; limit caps the row count (must be > 0). Stable under
// concurrent inserts (no OFFSET).
func (s *Service) ListDeliveriesPage(ctx context.Context, triggerID, status string, before time.Time, limit int) ([]Delivery, error) {
	if limit <= 0 {
		limit = 20
	}
	q := `SELECT id, trigger_id, status, attempts, next_attempt_at,
	             last_attempt_at, last_status_code, last_error, created_at, delivered_at
	      FROM build_trigger_deliveries
	      WHERE trigger_id=?`
	args := []any{triggerID}
	if status != "" {
		q += " AND status=?"
		args = append(args, status)
	}
	if !before.IsZero() {
		// Keyset: rows strictly before the cursor's created_at, or equal
		// created_at with a smaller id (id DESC tie-break).
		q += " AND (created_at < ? OR (created_at = ? AND id < ?))"
		args = append(args, before.Unix(), before.Unix(), "")
		// NOTE: the id tie-break needs the cursor id; see handler — when the
		// caller has only a time cursor it passes id="" which makes the second
		// clause inert. Callers that need exact tie-break pass the id via the
		// 3-arg form below.
	}
	q += " ORDER BY created_at DESC, id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("buildtrigger: list deliveries page: %w", err)
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
```

The id tie-break is only fully exact when created_at values collide AND the cursor id is known. For the UI's 20/page over second-granularity timestamps, a pure `created_at < cursor` is sufficient and simplest. Simplify the `before` clause to:

```go
	if !before.IsZero() {
		q += " AND created_at < ?"
		args = append(args, before.Unix())
	}
```

Use the simplified form (delete the 3-arg version and the NOTE). The test above (distinct created_at per row) passes with it.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/buildtrigger/ -run TestListDeliveriesPage -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/buildtrigger/delivery.go internal/buildtrigger/delivery_page_test.go
git commit -m "feat(buildtrigger): keyset-paginated ListDeliveriesPage"
```

---

## Task 4: `buildtrigger.Service.RecentDeliveryIDs`

Backs the bounded-replay authority check (most-recent-N ids for a trigger).

**Files:**
- Modify: `internal/buildtrigger/delivery.go`
- Test: `internal/buildtrigger/recent_test.go` (create)

- [ ] **Step 1: Write the failing test**

```go
package buildtrigger

import (
	"context"
	"testing"
	"time"
)

func TestRecentDeliveryIDs(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	base := time.Now().Unix()
	for i, id := range []string{"d1", "d2", "d3", "d4"} {
		insertDelivery(t, s, id, "bvbt_t", "delivered", base+int64(i))
	}
	got, err := s.RecentDeliveryIDs(ctx, "bvbt_t", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "d4" || got[1] != "d3" {
		t.Fatalf("recent = %v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/buildtrigger/ -run TestRecentDeliveryIDs -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Implement `RecentDeliveryIDs`**

Add after `ListDeliveriesPage`:

```go
// RecentDeliveryIDs returns up to n most-recent delivery ids for a trigger,
// newest first. Used by the UI to bound which deliveries may be replayed.
func (s *Service) RecentDeliveryIDs(ctx context.Context, triggerID string, n int) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM build_trigger_deliveries
		 WHERE trigger_id=?
		 ORDER BY created_at DESC, id DESC LIMIT ?`, triggerID, n)
	if err != nil {
		return nil, fmt.Errorf("buildtrigger: recent delivery ids: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/buildtrigger/ -run TestRecentDeliveryIDs -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/buildtrigger/delivery.go internal/buildtrigger/recent_test.go
git commit -m "feat(buildtrigger): RecentDeliveryIDs for bounded replay"
```

---

## Task 5: Connector-names helper

Exposes configured connector names (no secrets) for the UI dropdowns.

**Files:**
- Modify: `internal/buildtrigger/config.go` (add `SortedConnectorNames`)
- Test: `internal/buildtrigger/connectornames_test.go` (create)

- [ ] **Step 1: Write the failing test**

```go
package buildtrigger

import (
	"reflect"
	"testing"
)

func TestSortedConnectorNames(t *testing.T) {
	aws := map[string]AWSConnector{"prod": {}, "dev": {}}
	azure := map[string]AzureConnector{"main": {}}
	awsNames, azureNames := SortedConnectorNames(aws, azure)
	if !reflect.DeepEqual(awsNames, []string{"dev", "prod"}) {
		t.Errorf("aws = %v", awsNames)
	}
	if !reflect.DeepEqual(azureNames, []string{"main"}) {
		t.Errorf("azure = %v", azureNames)
	}
	// nil maps → empty (non-nil) slices.
	a, z := SortedConnectorNames(nil, nil)
	if a == nil || z == nil || len(a) != 0 || len(z) != 0 {
		t.Errorf("nil maps should give empty non-nil slices: %v %v", a, z)
	}
}
```

Confirm the exact field/type names `AWSConnector` and `AzureConnector` by grepping `internal/buildtrigger/config.go` before writing (they are referenced in `serve.go` as `buildtrigger.AWSConnector` / `buildtrigger.AzureConnector`).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/buildtrigger/ -run TestSortedConnectorNames -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Implement `SortedConnectorNames`**

Add to `config.go`:

```go
import "sort" // add to the existing import block if not present

// SortedConnectorNames returns the connector names (keys) of the two connector
// maps, each sorted ascending. Names only — never secrets. nil maps yield
// empty, non-nil slices.
func SortedConnectorNames(aws map[string]AWSConnector, azure map[string]AzureConnector) (awsNames, azureNames []string) {
	awsNames = make([]string, 0, len(aws))
	for k := range aws {
		awsNames = append(awsNames, k)
	}
	azureNames = make([]string, 0, len(azure))
	for k := range azure {
		azureNames = append(azureNames, k)
	}
	sort.Strings(awsNames)
	sort.Strings(azureNames)
	return awsNames, azureNames
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/buildtrigger/ -run TestSortedConnectorNames -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/buildtrigger/config.go internal/buildtrigger/connectornames_test.go
git commit -m "feat(buildtrigger): SortedConnectorNames helper for UI dropdowns"
```

---

## Task 6: CLI `bucketvcs trigger edit`

**Files:**
- Modify: `cmd/bucketvcs/build.go`
- Test: `cmd/bucketvcs/build_test.go`

- [ ] **Step 1: Write the failing test**

Append to `cmd/bucketvcs/build_test.go` (follow the existing `runBuild(...)` invocation harness used by the add/list tests):

```go
func TestTriggerEdit(t *testing.T) {
	authDB := newTestAuthDB(t) // same helper the existing build_test.go tests use
	// create a trigger
	var addOut, addErr bytes.Buffer
	if code := runBuild(context.Background(), []string{
		"trigger", "add", "--auth-db=" + authDB, "--tenant=acme", "--repo=w",
		"--name=ci", "--kind=cloudbuild", "--url=https://example.com/h",
	}, &addOut, &addErr); code != 0 {
		t.Fatalf("add: code=%d err=%s", code, addErr.String())
	}
	id := extractKV(t, addOut.String(), "trigger_id=")
	// edit it
	var out, errb bytes.Buffer
	if code := runBuild(context.Background(), []string{
		"trigger", "edit", "--auth-db=" + authDB, "--id=" + id,
		"--name=ci2", "--ref-include=refs/heads/main", "--active=false",
	}, &out, &errb); code != 0 {
		t.Fatalf("edit: code=%d err=%s", code, errb.String())
	}
	// list and confirm the new name + inactive
	var listOut, listErr bytes.Buffer
	if code := runBuild(context.Background(), []string{
		"trigger", "list", "--auth-db=" + authDB, "--tenant=acme", "--repo=w",
	}, &listOut, &listErr); code != 0 {
		t.Fatalf("list: code=%d err=%s", code, listErr.String())
	}
	if !strings.Contains(listOut.String(), "ci2") {
		t.Errorf("edited name not in list: %s", listOut.String())
	}
}
```

Reuse the real helper names (`newTestAuthDB`, `extractKV`) from the existing file; adjust if they differ.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/bucketvcs/ -run TestTriggerEdit -v`
Expected: FAIL — unknown action `edit` (exit 2) → test fails at the edit step.

- [ ] **Step 3: Wire the `edit` action**

In `runBuildTrigger`'s switch (around line 64 of `build.go`), add a case before `default`:

```go
	case "edit":
		return runBuildTriggerEdit(ctx, args[1:], stdout, stderr)
	case "rotate-secret":
		return runBuildTriggerRotateSecret(ctx, args[1:], stdout, stderr)
```

(`rotate-secret` is implemented in Task 7; add both cases now so the switch is written once.)

Update the action-required message string to include the new actions:
```go
	fmt.Fprintln(stderr, "build trigger: action required (add|list|edit|remove|enable|disable|rotate-secret)")
```

- [ ] **Step 4: Implement `runBuildTriggerEdit`**

Add a new function (mirror `runBuildTriggerAdd`'s authdb-open boilerplate — read it to copy the exact `openAuthDB`/service-construction lines used there). The `--active` flag uses the same `flag.Bool` default-true-with-explicit-detection trick only if you need to distinguish "omitted"; for edit we require the caller to pass the full desired state, so a plain bool default true is fine.

```go
func runBuildTriggerEdit(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("build trigger edit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	id := fs.String("id", "", "Trigger id (required)")
	name := fs.String("name", "", "New trigger name (required)")
	refInclude := fs.String("ref-include", "", "Ref include globs (csv)")
	refExclude := fs.String("ref-exclude", "", "Ref exclude globs (csv)")
	tokenMode := fs.String("token-mode", "", "Token mode: none|inject")
	tokenScopes := fs.String("token-scopes", "", "Token scopes (csv|all|repo:*|lfs:*)")
	tokenTTL := fs.String("token-ttl", "", "Token TTL (Go duration)")
	active := fs.Bool("active", true, "Whether the trigger is active")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" || *id == "" || *name == "" {
		fmt.Fprintln(stderr, "build trigger edit: --auth-db, --id, --name required")
		return 2
	}
	svc, closeFn, code := openTriggerService(*authDB, stderr) // see note below
	if code != 0 {
		return code
	}
	defer closeFn()

	in := buildtrigger.EditInput{
		Name:       *name,
		RefInclude: splitCSV(*refInclude),
		RefExclude: splitCSV(*refExclude),
		TokenMode:  buildtrigger.TokenMode(*tokenMode),
		Active:     *active,
	}
	if *tokenScopes != "" {
		scopes, err := auth.ParseScopes(*tokenScopes)
		if err != nil {
			fmt.Fprintf(stderr, "build trigger edit: --token-scopes: %v\n", err)
			return 2
		}
		in.TokenScopes = scopes
	}
	if *tokenTTL != "" {
		d, err := time.ParseDuration(*tokenTTL)
		if err != nil {
			fmt.Fprintf(stderr, "build trigger edit: --token-ttl: %v\n", err)
			return 2
		}
		in.TokenTTL = d
	}
	tr, err := svc.Edit(ctx, *id, in)
	if err != nil {
		return triggerMutateErrCode(err, stderr, "edit") // see note below
	}
	fmt.Fprintf(stdout, "trigger_id=%s name=%s active=%t\n", tr.ID, tr.Name, tr.Active)
	return 0
}
```

**Note on `openTriggerService` / `triggerMutateErrCode`:** `runBuildTriggerAdd`/`Remove` already contain inline authdb-open + service-construction + error-to-exit-code logic. Rather than copy it, extract two small helpers from the existing add/remove functions and reuse them in add/remove/edit/rotate-secret:

```go
// openTriggerService opens the authdb and returns a buildtrigger.Service plus a
// close func. On failure it prints to stderr and returns a non-zero exit code.
func openTriggerService(authDBPath string, stderr io.Writer) (*buildtrigger.Service, func(), int) {
	// Copy the EXACT open sequence used at the top of runBuildTriggerAdd
	// (sqlitestore.Open / auth.Open — match whatever it does), then:
	//   return buildtrigger.New(store.DB()), func(){ store.Close() }, 0
	// Keep the error messages identical to the originals.
}

// triggerMutateErrCode maps a service error to the documented exit codes:
// ErrNotFound/operational → 1, ErrInvalidInput/ErrConflict → 2.
func triggerMutateErrCode(err error, stderr io.Writer, action string) int {
	switch {
	case errors.Is(err, buildtrigger.ErrInvalidInput), errors.Is(err, buildtrigger.ErrConflict):
		fmt.Fprintf(stderr, "build trigger %s: %v\n", action, err)
		return 2
	default:
		fmt.Fprintf(stderr, "build trigger %s: %v\n", action, err)
		return 1
	}
}
```

Refactor `runBuildTriggerAdd` and `runBuildTriggerRemove` to use these two helpers in the same commit (DRY), keeping their observable output identical (run the existing `TestTrigger*` tests to confirm no regression).

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./cmd/bucketvcs/ -run TestTrigger -v`
Expected: PASS (new `TestTriggerEdit` plus all existing trigger tests still green).

- [ ] **Step 6: Update the CLI usage string**

In `buildUsage` (top of `build.go`), under the `trigger` section, add:
```
  trigger edit          --auth-db=<path> --id=<id> --name=<n> [--ref-include=… --ref-exclude=… --token-mode=… --token-scopes=… --token-ttl=… --active=true|false]
  trigger rotate-secret --auth-db=<path> --id=<id>
```

- [ ] **Step 7: Commit**

```bash
git add cmd/bucketvcs/build.go cmd/bucketvcs/build_test.go
git commit -m "feat(cli): bucketvcs trigger edit + shared service/err helpers"
```

---

## Task 7: CLI `bucketvcs trigger rotate-secret`

**Files:**
- Modify: `cmd/bucketvcs/build.go`
- Test: `cmd/bucketvcs/build_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestTriggerRotateSecret(t *testing.T) {
	authDB := newTestAuthDB(t)
	var addOut, addErr bytes.Buffer
	if code := runBuild(context.Background(), []string{
		"trigger", "add", "--auth-db=" + authDB, "--tenant=acme", "--repo=w",
		"--name=ci", "--kind=generic", "--url=https://example.com/h",
	}, &addOut, &addErr); code != 0 {
		t.Fatalf("add: %s", addErr.String())
	}
	id := extractKV(t, addOut.String(), "trigger_id=")
	var out, errb bytes.Buffer
	if code := runBuild(context.Background(), []string{
		"trigger", "rotate-secret", "--auth-db=" + authDB, "--id=" + id,
	}, &out, &errb); code != 0 {
		t.Fatalf("rotate: code=%d err=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "secret=") {
		t.Errorf("rotate output missing secret=: %s", out.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/bucketvcs/ -run TestTriggerRotateSecret -v`
Expected: FAIL — `runBuildTriggerRotateSecret` undefined (the switch case from Task 6 references it).

- [ ] **Step 3: Implement `runBuildTriggerRotateSecret`**

```go
func runBuildTriggerRotateSecret(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("build trigger rotate-secret", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	id := fs.String("id", "", "Trigger id (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" || *id == "" {
		fmt.Fprintln(stderr, "build trigger rotate-secret: --auth-db, --id required")
		return 2
	}
	svc, closeFn, code := openTriggerService(*authDB, stderr)
	if code != 0 {
		return code
	}
	defer closeFn()
	secret, err := svc.RotateSecret(ctx, *id)
	if err != nil {
		return triggerMutateErrCode(err, stderr, "rotate-secret")
	}
	fmt.Fprintf(stdout, "trigger_id=%s secret=%s\n", *id, secret)
	return 0
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/bucketvcs/ -run TestTrigger -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/bucketvcs/build.go cmd/bucketvcs/build_test.go
git commit -m "feat(cli): bucketvcs trigger rotate-secret"
```

---

## Task 8: Web wiring — `TriggerAdmin` interface, `ConnectorNames`, Deps/server/serve

No handler yet — just make the service reachable from the web package and assert it compiles.

**Files:**
- Modify: `internal/web/services.go`
- Modify: `internal/web/handler.go`
- Modify: `cmd/bucketvcs/serve.go`
- Test: `internal/web/services_test.go` (add compile assertion)

- [ ] **Step 1: Add the interface + connector-names type to `services.go`**

```go
// at top: add buildtrigger + time imports as needed
import (
	// ... existing ...
	"github.com/bucketvcs/bucketvcs/internal/buildtrigger"
)

// TriggerAdmin is the slice of *buildtrigger.Service the settings UI needs.
type TriggerAdmin interface {
	Create(ctx context.Context, in buildtrigger.TriggerInput) (buildtrigger.Trigger, error)
	List(ctx context.Context, tenant, repo string) ([]buildtrigger.Trigger, error)
	Get(ctx context.Context, id string) (buildtrigger.Trigger, error)
	Edit(ctx context.Context, id string, in buildtrigger.EditInput) (buildtrigger.Trigger, error)
	RotateSecret(ctx context.Context, id string) (string, error)
	Enable(ctx context.Context, id string) error
	Disable(ctx context.Context, id string) error
	Remove(ctx context.Context, id string) error
	ListDeliveriesPage(ctx context.Context, triggerID, status string, before time.Time, limit int) ([]buildtrigger.Delivery, error)
	RecentDeliveryIDs(ctx context.Context, triggerID string, n int) ([]string, error)
	ReplayDelivery(ctx context.Context, id string) error
}

// ConnectorNames are the configured connector names (no secrets) surfaced in the
// trigger create/edit form's connector dropdowns. Empty when no --build-config.
type ConnectorNames struct {
	AWS   []string
	Azure []string
}
```

- [ ] **Step 2: Add a compile-time assertion test**

In `internal/web/services_test.go` add:

```go
func TestTriggerServiceSatisfiesTriggerAdmin(t *testing.T) {
	var _ TriggerAdmin = (*buildtrigger.Service)(nil)
}
```

(import `github.com/bucketvcs/bucketvcs/internal/buildtrigger`)

- [ ] **Step 3: Run to verify it fails (interface mismatch is a compile error)**

Run: `go test ./internal/web/ -run TestTriggerServiceSatisfiesTriggerAdmin -v`
Expected: PASS if Tasks 1–4 are correct. If it FAILS to compile, the method signatures drifted — reconcile against Tasks 1–4. (This is the guard that the interface matches the concrete service.)

- [ ] **Step 4: Add `Triggers` + `Connectors` to `Deps` and `server`**

In `handler.go` `Deps` (after `RenameCheck`):
```go
	Triggers   TriggerAdmin   // nil => triggers tab renders "not enabled"
	Connectors ConnectorNames // configured connector names (no secrets)
```
In `server` struct (after `renameCheck`):
```go
	triggers   TriggerAdmin
	connectors ConnectorNames
```
In `NewHandler`'s `&server{...}` literal (after `renameCheck: d.RenameCheck,`):
```go
		triggers:   d.Triggers,
		connectors: d.Connectors,
```

- [ ] **Step 5: Wire serve.go**

In `cmd/bucketvcs/serve.go`, find the `webDeps := web.Deps{...}` literal (around line 909). `buildSvc`, `buildConnectors`, `buildAzureConnectors` are already in scope. Add:

```go
				Triggers:   buildSvc, // nil when build triggers disabled
				Connectors: func() web.ConnectorNames {
					aws, azure := buildtrigger.SortedConnectorNames(buildConnectors, buildAzureConnectors)
					return web.ConnectorNames{AWS: aws, Azure: azure}
				}(),
```

If a second `web.Deps` literal exists (replica/multi-region serve mode, ~line 1045 area builds a different struct — confirm whether it is a `web.Deps`; the grep showed `Webhooks:`/`BuildTriggers:` keys there which may belong to a *gateway* config, not `web.Deps`). Only add the two fields to literals whose type is `web.Deps`. Leave non-web literals alone.

- [ ] **Step 6: Build everything**

Run: `go build ./... && go test ./internal/web/ -run TestTriggerServiceSatisfiesTriggerAdmin`
Expected: builds clean; test PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/web/services.go internal/web/services_test.go internal/web/handler.go cmd/bucketvcs/serve.go
git commit -m "feat(web): wire buildtrigger service + connector names into web.Deps"
```

---

## Task 9: Triggers tab — dispatch, list page, template, nav

**Files:**
- Modify: `internal/web/reposettings.go` (add `case "triggers"`)
- Modify: `internal/web/templates/_partials.html` (nav link)
- Create: `internal/web/reposettings_triggers.go`
- Create: `internal/web/templates/reposettings_triggers.html`
- Test: `internal/web/reposettings_triggers_test.go`

- [ ] **Step 1: Add the dispatch case**

In `reposettings.go` `handleRepoSettings` switch, after `case "hooks":` block, add:
```go
	case "triggers":
		s.repoSettingsTriggers(w, r, sr)
```

- [ ] **Step 2: Add the nav link**

In `_partials.html` `reposettingsnav` (lines 33–39), change the policy line to insert triggers before the admin-gated hooks:
```html
  <a href="/{{.Tenant}}/{{.Repo}}/settings/policy">policy</a> ·
  <a href="/{{.Tenant}}/{{.Repo}}/settings/triggers">triggers</a>{{if .IsAdmin}} ·
  <a href="/{{.Tenant}}/{{.Repo}}/settings/hooks">hooks</a>{{end}}
```

- [ ] **Step 3: Write the failing handler test**

Create `internal/web/reposettings_triggers_test.go`. Read `internal/web/reposettings_webhooks_test.go` first to copy the test server + admin-session helpers (`newTestServerWithAdmin`, request helpers — match the real names). Minimal first test:

```go
package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTriggersPage_ListsTriggers(t *testing.T) {
	// Build a server whose Triggers dep is a fake returning one trigger.
	srv := newTriggersTestServer(t, &fakeTriggers{
		list: []buildtriggerTrigger{{ID: "bvbt_1", Name: "ci", Kind: "cloudbuild", Active: true}},
	})
	req := httptest.NewRequest(http.MethodGet, "/acme/widgets/settings/triggers", nil)
	req = withAdminSession(t, req) // same helper the webhooks test uses
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ci") || !strings.Contains(rec.Body.String(), "cloudbuild") {
		t.Errorf("list missing trigger: %s", rec.Body.String())
	}
}

func TestTriggersPage_NotEnabled(t *testing.T) {
	srv := newTriggersTestServer(t, nil) // Triggers dep nil
	req := httptest.NewRequest(http.MethodGet, "/acme/widgets/settings/triggers", nil)
	req = withAdminSession(t, req)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "not enabled") {
		t.Fatalf("want enabled-notice 200, got %d %s", rec.Code, rec.Body.String())
	}
}
```

Implement `fakeTriggers` (a `TriggerAdmin` stub) and `newTriggersTestServer` in the test file. The `fakeTriggers` returns canned data and records calls. `buildtriggerTrigger` in the sketch above stands for `buildtrigger.Trigger` — import the real type and use it; the fake's `List` returns `[]buildtrigger.Trigger`. Model `newTriggersTestServer` on the webhooks test's server constructor, passing `Deps{..., Triggers: fake, Connectors: web.ConnectorNames{...}}` (and a non-admin repo grant so `canAdminRepo` passes — copy from webhooks test).

- [ ] **Step 4: Run to verify it fails**

Run: `go test ./internal/web/ -run TestTriggersPage -v`
Expected: FAIL — `repoSettingsTriggers` undefined / template missing.

- [ ] **Step 5: Implement the dispatcher + list page**

Create `internal/web/reposettings_triggers.go`:

```go
package web

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/buildtrigger"
)

func (sr settingsRoute) triggersBase() string {
	return "/" + sr.tenant + "/" + sr.repo + "/settings/triggers"
}

func (sr settingsRoute) triggerDeliveriesURL(triggerID string) string {
	return fmt.Sprintf("/%s/%s/settings/triggers/deliveries?trigger=%s", sr.tenant, sr.repo, triggerID)
}

// triggerRow is the per-row view-model for the triggers list.
type triggerRow struct {
	buildtrigger.Trigger
	RefSummary string // "+inc -exc" compact display
	LastFireOK *bool  // nil=never, true=last delivered, false=last failed
	LastFireRel string
	CanRotate  bool // generic/cloudbuild only
}

type triggersData struct {
	base
	Tenant, Repo string
	IsAdmin      bool
	Enabled      bool
	Triggers     []triggerRow
}

// repoSettingsTriggers dispatches the triggers tab. The chassis already
// authorized the actor (repo PermAdmin) and probed repo existence.
func (s *server) repoSettingsTriggers(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	switch sr.action {
	case "":
		s.triggersPage(w, r, sr)
	case "new":
		s.triggersNewForm(w, r, sr) // Task 10
	case "add":
		s.triggersAdd(w, r, sr) // Task 10
	case "edit":
		s.triggersEdit(w, r, sr) // Task 11
	case "enable":
		s.triggersSetActive(w, r, sr, true) // Task 11
	case "disable":
		s.triggersSetActive(w, r, sr, false) // Task 11
	case "remove":
		s.triggersRemove(w, r, sr) // Task 11
	case "rotate-secret":
		s.triggersRotateSecret(w, r, sr) // Task 11
	case "deliveries":
		s.triggersDeliveries(w, r, sr) // Task 12
	case "replay":
		s.triggersReplay(w, r, sr) // Task 12
	default:
		s.renderError(w, r, http.StatusNotFound, "not found")
	}
}

func (s *server) triggersPage(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if r.Method != http.MethodGet {
		s.renderError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	d := triggersData{
		base:    base{Session: SessionFromContext(r.Context()), CSRF: issueCSRF(w, requestIsTLS(r, s.trustProxy)), Flash: takeFlash(w, r)},
		Tenant:  sr.tenant,
		Repo:    sr.repo,
		IsAdmin: isGlobalAdmin(r),
		Enabled: s.triggers != nil,
	}
	if s.triggers != nil {
		trs, err := s.triggers.List(r.Context(), sr.tenant, sr.repo)
		if err != nil {
			s.logger.Error("triggers: list", "tenant", sr.tenant, "repo", sr.repo, "err", err)
			s.renderError(w, r, http.StatusInternalServerError, "internal error")
			return
		}
		rows := make([]triggerRow, 0, len(trs))
		for _, tr := range trs {
			row := triggerRow{
				Trigger:    tr,
				RefSummary: refSummary(tr.RefInclude, tr.RefExclude),
				CanRotate:  tr.Kind == buildtrigger.KindGeneric || tr.Kind == buildtrigger.KindCloudBuild,
			}
			// last-fire status: newest delivery for this trigger
			recent, derr := s.triggers.ListDeliveriesPage(r.Context(), tr.ID, "", time.Time{}, 1)
			if derr != nil {
				s.logger.Warn("triggers: last-fire probe", "trigger_id", tr.ID, "err", derr)
			} else if len(recent) == 1 {
				ok := recent[0].Status == "delivered"
				row.LastFireOK = &ok
				row.LastFireRel = relTimeAt(time.Now(), recent[0].CreatedAt.Unix())
			}
			rows = append(rows, row)
		}
		d.Triggers = rows
	}
	if err := s.renderBuffered(w, "reposettings_triggers.html", d); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "render error")
		return
	}
	EmitRequestMetric(r.Context(), s.logger, "reposettings_triggers", http.StatusOK)
}

// refSummary renders include/exclude globs compactly: "+a +b -c".
func refSummary(inc, exc []string) string {
	var b []byte
	for i, p := range inc {
		if i > 0 || len(b) > 0 {
			b = append(b, ' ')
		}
		b = append(b, '+')
		b = append(b, p...)
	}
	for _, p := range exc {
		if len(b) > 0 {
			b = append(b, ' ')
		}
		b = append(b, '-')
		b = append(b, p...)
	}
	if len(b) == 0 {
		return "(all)"
	}
	return string(b)
}

// silence unused import until Task 11 wires slog usage; remove if slog used here.
var _ = slog.String
```

(Delete the `var _ = slog.String` line once Task 11 adds real `slog` usage; it only keeps Task 9 compiling in isolation. If subagent-driven and Task 11 lands in the same session, you can omit the placeholder and add the `slog` import in Task 11.)

The handlers referenced for later tasks (`triggersNewForm`, `triggersAdd`, etc.) don't exist yet — to keep Task 9 compiling, add **stub** methods at the bottom of this file that return 404, to be replaced in Tasks 10–12:

```go
func (s *server) triggersNewForm(w http.ResponseWriter, r *http.Request, sr settingsRoute) { s.renderError(w, r, http.StatusNotFound, "not found") }
func (s *server) triggersAdd(w http.ResponseWriter, r *http.Request, sr settingsRoute)      { s.renderError(w, r, http.StatusNotFound, "not found") }
func (s *server) triggersEdit(w http.ResponseWriter, r *http.Request, sr settingsRoute)     { s.renderError(w, r, http.StatusNotFound, "not found") }
func (s *server) triggersSetActive(w http.ResponseWriter, r *http.Request, sr settingsRoute, active bool) { s.renderError(w, r, http.StatusNotFound, "not found") }
func (s *server) triggersRemove(w http.ResponseWriter, r *http.Request, sr settingsRoute)   { s.renderError(w, r, http.StatusNotFound, "not found") }
func (s *server) triggersRotateSecret(w http.ResponseWriter, r *http.Request, sr settingsRoute) { s.renderError(w, r, http.StatusNotFound, "not found") }
func (s *server) triggersDeliveries(w http.ResponseWriter, r *http.Request, sr settingsRoute) { s.renderError(w, r, http.StatusNotFound, "not found") }
func (s *server) triggersReplay(w http.ResponseWriter, r *http.Request, sr settingsRoute)   { s.renderError(w, r, http.StatusNotFound, "not found") }
```

- [ ] **Step 6: Create the list template**

Create `internal/web/templates/reposettings_triggers.html`:

```html
{{define "title"}}{{.Tenant}}/{{.Repo}} triggers · bucketvcs{{end}}
{{define "content"}}
<div class="settings">
  <div class="repohdr"><span class="path"><a href="/{{.Tenant}}/{{.Repo}}">{{.Tenant}}/{{.Repo}}</a></span></div>
  {{template "reposettingsnav" .}}
  <h2>triggers</h2>
  {{if not .Enabled}}
  <p class="hint">build triggers are not enabled on this server.</p>
  {{else}}
  <p>
    <a href="/{{.Tenant}}/{{.Repo}}/settings/triggers/new"
       hx-get="/{{.Tenant}}/{{.Repo}}/settings/triggers/new"
       hx-target="#trigger-modal" hx-swap="innerHTML">+ add trigger</a>
  </p>
  <div id="trigger-modal"></div>
  {{if .Triggers}}
  <table>
    <thead>
      <tr><th>active</th><th>name</th><th>kind</th><th>refs</th><th>token</th><th>last fire</th><th>actions</th></tr>
    </thead>
    <tbody>
    {{range .Triggers}}
    <tr>
      <td>{{if .Active}}<span class="on">✓</span>{{else}}<span class="off">✗</span>{{end}}</td>
      <td>{{.Name}}</td>
      <td>{{.Kind}}</td>
      <td class="mono">{{.RefSummary}}</td>
      <td>{{.TokenMode}}</td>
      <td>
        {{if .LastFireOK}}{{if (deref .LastFireOK)}}<span class="on">✓</span>{{else}}<span class="off">✗</span>{{end}} {{.LastFireRel}}{{else}}<span class="dim">never</span>{{end}}
      </td>
      <td class="acts">
        <a href="{{$.Tenant}}/{{$.Repo}}/settings/triggers/deliveries?trigger={{.ID}}" >deliveries</a>
        {{if .Active}}
        <a href="#" hx-post="/{{$.Tenant}}/{{$.Repo}}/settings/triggers/disable" hx-vals='{"csrf_token":"{{$.CSRF}}","id":"{{.ID}}"}'>disable</a>
        {{else}}
        <a href="#" hx-post="/{{$.Tenant}}/{{$.Repo}}/settings/triggers/enable" hx-vals='{"csrf_token":"{{$.CSRF}}","id":"{{.ID}}"}'>enable</a>
        {{end}}
        <a href="/{{$.Tenant}}/{{$.Repo}}/settings/triggers/new?id={{.ID}}"
           hx-get="/{{$.Tenant}}/{{$.Repo}}/settings/triggers/new?id={{.ID}}"
           hx-target="#trigger-modal" hx-swap="innerHTML">edit</a>
        {{if .CanRotate}}
        <a href="#" hx-post="/{{$.Tenant}}/{{$.Repo}}/settings/triggers/rotate-secret" hx-vals='{"csrf_token":"{{$.CSRF}}","id":"{{.ID}}"}'>rotate-secret</a>
        {{end}}
        <a href="#" hx-post="/{{$.Tenant}}/{{$.Repo}}/settings/triggers/remove" hx-vals='{"csrf_token":"{{$.CSRF}}","id":"{{.ID}}"}'>remove</a>
      </td>
    </tr>
    {{end}}
    </tbody>
  </table>
  {{else}}
  <p class="empty">no triggers configured.</p>
  {{end}}
  {{end}}
</div>
{{end}}
{{template "base" .}}
```

**Important — no-JS fallback:** the `hx-post` links above must also work without JS. The webhooks tab uses real `<form>` POSTs for mutations (see `reposettings_webhooks.html`). To stay consistent and JS-optional, replace each `hx-post` `<a>` with an inline `<form method="post">` + `<button>` exactly like the webhooks template's enable/disable/remove forms (CSRF hidden input + `id` hidden input). Use the webhooks template's markup verbatim, swapping the action paths to `.../triggers/...`. Keep the bracket styling via the `.acts` CSS added below. (Drop the `hx-vals` approach — forms degrade cleanly and match the existing code; htmx can still enhance forms via `hx-boost` globally if desired, but plain forms already work.)

- [ ] **Step 7: Add the `deref` template func + `.acts`/glyph CSS**

`deref` (turn `*bool` into `bool` for the template) — check `render.go`'s `FuncMap` for existing helpers and add:
```go
"deref": func(b *bool) bool { return b != nil && *b },
```
Add to `internal/web/static/style.css`:
```css
.settings .on { color: var(--accent); }
.settings .off { color: #d98f8f; }
.acts form { display: inline; }
.acts a, .acts button { margin-right: .9rem; }
.acts a::before, .acts button::before { content: "["; color: var(--dim); }
.acts a::after, .acts button::after { content: "]"; color: var(--dim); }
.acts button { background: transparent; border: none; color: var(--accent); padding: 0; font: inherit; cursor: pointer; }
.kindfields { border-left: 2px solid var(--dim); padding-left: .8rem; margin: .4rem 0 .4rem .2rem; }
```

- [ ] **Step 8: Run tests**

Run: `go test ./internal/web/ -run TestTriggersPage -v`
Expected: PASS. Also run `go build ./...`.

- [ ] **Step 9: Commit**

```bash
git add internal/web/reposettings.go internal/web/reposettings_triggers.go internal/web/templates/reposettings_triggers.html internal/web/templates/_partials.html internal/web/static/style.css internal/web/render.go internal/web/reposettings_triggers_test.go
git commit -m "feat(web): triggers tab dispatch + list page + nav"
```

---

## Task 10: Create form (`/new`) + add handler + kind-fields fragment

**Files:**
- Modify: `internal/web/reposettings_triggers.go` (replace `triggersNewForm`/`triggersAdd` stubs)
- Create: `internal/web/templates/reposettings_triggers_form.html`
- Test: `internal/web/reposettings_triggers_test.go`

- [ ] **Step 1: Write failing tests**

```go
func TestTriggersNewForm_DefaultAndKindSwap(t *testing.T) {
	srv := newTriggersTestServer(t, &fakeTriggers{}, withConnectors(ConnectorNames{AWS: []string{"prod"}}))
	// default form
	rec := doGet(t, srv, "/acme/widgets/settings/triggers/new")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "add trigger") {
		t.Fatalf("new form: %d %s", rec.Code, rec.Body.String())
	}
	// codebuild kind → connector dropdown shows configured names
	rec = doGet(t, srv, "/acme/widgets/settings/triggers/new?kind=codebuild")
	if !strings.Contains(rec.Body.String(), "prod") {
		t.Errorf("codebuild form missing connector option: %s", rec.Body.String())
	}
}

func TestTriggersAdd_GenericShowsSecretOnce(t *testing.T) {
	fake := &fakeTriggers{createSecret: "supersecretvalue"}
	srv := newTriggersTestServer(t, fake)
	form := url.Values{"csrf_token": {testCSRF}, "name": {"ci"}, "kind": {"generic"}, "url": {"https://e/h"}}
	rec := doPostForm(t, srv, "/acme/widgets/settings/triggers/add", form)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "supersecretvalue") {
		t.Fatalf("expected secret-once page, got %d %s", rec.Code, rec.Body.String())
	}
	if fake.created == nil || fake.created.Kind != "generic" {
		t.Errorf("create not called with generic kind")
	}
}

func TestTriggersAdd_InvalidKindFlash(t *testing.T) {
	srv := newTriggersTestServer(t, &fakeTriggers{createErr: buildtrigger.ErrInvalidInput})
	form := url.Values{"csrf_token": {testCSRF}, "name": {"ci"}, "kind": {"generic"}, "url": {""}}
	rec := doPostForm(t, srv, "/acme/widgets/settings/triggers/add", form)
	// redirect-flash → 303 to triggers base
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want 303 flash, got %d %s", rec.Code, rec.Body.String())
	}
}
```

Add the test helpers `doGet`, `doPostForm`, `testCSRF`, `withConnectors`, and extend `fakeTriggers` with `createSecret`/`createErr`/`created` fields. Mirror the webhooks test's POST helper for CSRF cookie+token pairing (read `reposettings_webhooks_test.go` for the exact CSRF test plumbing — CSRF is double-submit cookie; the helper must set both).

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/web/ -run 'TestTriggersNewForm|TestTriggersAdd' -v`
Expected: FAIL — stubs return 404.

- [ ] **Step 3: Implement `triggersNewForm`**

Replace the stub:

```go
type triggerFormData struct {
	base
	Tenant, Repo string
	Kind         string
	Connectors   ConnectorNames
	Editing      bool
	Trigger      buildtrigger.Trigger // populated when editing
	HXFragment   bool                 // true => render fragment only (htmx)
}

func (s *server) triggersNewForm(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if s.triggers == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodGet {
		s.renderError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	d := triggerFormData{
		base:       base{Session: SessionFromContext(r.Context()), CSRF: issueCSRF(w, requestIsTLS(r, s.trustProxy)), Flash: takeFlash(w, r)},
		Tenant:     sr.tenant,
		Repo:       sr.repo,
		Kind:       r.URL.Query().Get("kind"),
		Connectors: s.connectors,
		HXFragment: r.Header.Get("HX-Request") == "true",
	}
	if d.Kind == "" {
		d.Kind = "generic"
	}
	if id := r.URL.Query().Get("id"); id != "" {
		tr, err := s.ownTriggerOr404(w, r, sr, id) // Task 11 helper
		if !ok(tr, err) {                          // see helper note
			return
		}
		d.Editing = true
		d.Trigger = tr
		d.Kind = string(tr.Kind)
	}
	tmpl := "reposettings_triggers_form.html"
	if err := s.renderBuffered(w, tmpl, d); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "render error")
		return
	}
	EmitRequestMetric(r.Context(), s.logger, "reposettings_triggers_form", http.StatusOK)
}
```

**Dependency note:** this references `s.ownTriggerOr404` (defined in Task 11). To keep Task 10 compiling, add a minimal version of `ownTriggerOr404` now (Task 11 will reuse it):

```go
// ownTriggerOr404 fetches a trigger by id and confirms it belongs to this
// (tenant, repo). Any mismatch yields a uniform 404. Returns ok=false after
// writing the error response.
func (s *server) ownTriggerOr404(w http.ResponseWriter, r *http.Request, sr settingsRoute, id string) (buildtrigger.Trigger, bool) {
	if id == "" {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return buildtrigger.Trigger{}, false
	}
	tr, err := s.triggers.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, buildtrigger.ErrNotFound) {
			s.renderError(w, r, http.StatusNotFound, "not found")
			return buildtrigger.Trigger{}, false
		}
		s.logger.Error("triggers: get for ownership", "id", id, "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return buildtrigger.Trigger{}, false
	}
	if tr.Tenant != sr.tenant || tr.Repo != sr.repo {
		// anti-enumeration: foreign trigger looks identical to missing
		s.renderError(w, r, http.StatusNotFound, "not found")
		return buildtrigger.Trigger{}, false
	}
	return tr, true
}
```

Replace the `if !ok(tr, err)` sketch with:
```go
		tr, okGet := s.ownTriggerOr404(w, r, sr, id)
		if !okGet {
			return
		}
```

- [ ] **Step 4: Implement `triggersAdd`**

```go
func (s *server) triggersAdd(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if s.triggers == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	in, ferr := parseTriggerInput(r, sr)
	if ferr != "" {
		EmitAdminActionMetric(r.Context(), s.logger, "trigger", "add", "invalid")
		s.redirectFlash(w, r, sr.triggersBase(), ferr)
		return
	}
	tr, err := s.triggers.Create(r.Context(), in)
	if err != nil {
		if errors.Is(err, buildtrigger.ErrConflict) {
			EmitAdminActionMetric(r.Context(), s.logger, "trigger", "add", "invalid")
			s.redirectFlash(w, r, sr.triggersBase(), "a trigger with that name already exists")
			return
		}
		if errors.Is(err, buildtrigger.ErrInvalidInput) {
			EmitAdminActionMetric(r.Context(), s.logger, "trigger", "add", "invalid")
			s.redirectFlash(w, r, sr.triggersBase(), err.Error())
			return
		}
		s.logger.Error("triggers: create", "tenant", sr.tenant, "repo", sr.repo, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "trigger", "add", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "buildtrigger.created",
		slog.String("tenant", sr.tenant), slog.String("repo", sr.repo),
		slog.String("trigger_id", tr.ID), slog.String("kind", string(tr.Kind)))
	EmitAdminActionMetric(r.Context(), s.logger, "trigger", "add", "ok")
	// Secret-once only when the service generated one (generic/cloudbuild, blank
	// secret submitted). Otherwise straight back to the list.
	if tr.Secret != "" {
		s.renderSecretOnce(w, r, "build trigger created", tr.Secret, sr.triggersBase())
		return
	}
	s.redirectFlash(w, r, sr.triggersBase(), "trigger created")
}

// parseTriggerInput maps the POST form to a TriggerInput. Returns a non-empty
// error string for client-correctable problems (caught before the service).
func parseTriggerInput(r *http.Request, sr settingsRoute) (buildtrigger.TriggerInput, string) {
	kind := buildtrigger.Kind(r.PostFormValue("kind"))
	in := buildtrigger.TriggerInput{
		Tenant: sr.tenant,
		Repo:   sr.repo,
		Name:   r.PostFormValue("name"),
		Kind:   kind,
		Config: buildtrigger.Config{
			URL:             r.PostFormValue("url"),
			Secret:          r.PostFormValue("secret"),
			AWSRegion:       r.PostFormValue("aws_region"),
			AWSProject:      r.PostFormValue("aws_project"),
			AWSConnector:    r.PostFormValue("aws_connector"),
			AzureWebhookURL: r.PostFormValue("azure_webhook_url"),
			AzureSigHeader:  r.PostFormValue("azure_sig_header"),
			AzureConnector:  r.PostFormValue("azure_connector"),
			AzureProject:    r.PostFormValue("azure_project"),
		},
		RefInclude: splitCSVField(r.PostFormValue("ref_include")),
		RefExclude: splitCSVField(r.PostFormValue("ref_exclude")),
		TokenMode:  buildtrigger.TokenMode(r.PostFormValue("token_mode")),
	}
	if pid := r.PostFormValue("azure_pipeline_id"); pid != "" {
		n, err := strconv.Atoi(pid)
		if err != nil {
			return buildtrigger.TriggerInput{}, "azure pipeline id must be a number"
		}
		in.Config.AzurePipelineID = n
	}
	if sc := r.PostFormValue("token_scopes"); sc != "" {
		scopes, err := auth.ParseScopes(sc)
		if err != nil {
			return buildtrigger.TriggerInput{}, "invalid token scopes: " + err.Error()
		}
		in.TokenScopes = scopes
	}
	if ttl := r.PostFormValue("token_ttl"); ttl != "" {
		d, err := time.ParseDuration(ttl)
		if err != nil {
			return buildtrigger.TriggerInput{}, "invalid token ttl: " + err.Error()
		}
		in.TokenTTL = d
	}
	return in, ""
}

// splitCSVField splits a comma/space separated form value into trimmed,
// non-empty tokens.
func splitCSVField(v string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }) {
		f = strings.TrimSpace(f)
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}
```

Add imports: `strconv`, `strings`, `time`, `errors`, `log/slog`, and `github.com/bucketvcs/bucketvcs/internal/auth`. Remove the `var _ = slog.String` placeholder from Task 9.

- [ ] **Step 5: Create the form template**

Create `internal/web/templates/reposettings_triggers_form.html`. It renders a `<dialog open>`-style modal panel (works as a plain panel without JS too). The kind `<select>` swaps `#kindfields` via htmx; no-JS falls back to navigating `/new?kind=X`.

```html
{{define "title"}}{{.Tenant}}/{{.Repo}} new trigger · bucketvcs{{end}}
{{define "kindfields"}}
<div class="kindfields" id="kindfields">
  {{if or (eq .Kind "generic") (eq .Kind "cloudbuild")}}
  <label>url <input type="text" name="url" required value="{{.Trigger.Config.URL}}" placeholder="https://..."></label>
  <p class="dim">secret is generated and shown once after create (leave blank)</p>
  <label>secret <input type="text" name="secret" placeholder="(optional; blank = generated)"></label>
  {{else if eq .Kind "azurewebhook"}}
  <label>azure webhook url <input type="text" name="azure_webhook_url" required value="{{.Trigger.Config.AzureWebhookURL}}" placeholder="https://dev.azure.com/.../_apis/public/distributedtask/webhooks/..."></label>
  <label>secret <input type="text" name="secret" placeholder="operator-supplied; blank = unsigned"></label>
  <label>sig header <input type="text" name="azure_sig_header" value="{{.Trigger.Config.AzureSigHeader}}" placeholder="X-Hub-Signature"></label>
  {{else if eq .Kind "codebuild"}}
  <label>connector
    <select name="aws_connector">
      {{range .Connectors.AWS}}<option value="{{.}}">{{.}}</option>{{end}}
    </select>
  </label>
  {{if not .Connectors.AWS}}<p class="dim">no connectors configured; ask your operator</p>{{end}}
  <label>aws region <input type="text" name="aws_region" required value="{{.Trigger.Config.AWSRegion}}" placeholder="us-east-1"></label>
  <label>aws project <input type="text" name="aws_project" required value="{{.Trigger.Config.AWSProject}}"></label>
  {{else if eq .Kind "azurepipelines"}}
  <label>connector
    <select name="azure_connector">
      {{range .Connectors.Azure}}<option value="{{.}}">{{.}}</option>{{end}}
    </select>
  </label>
  {{if not .Connectors.Azure}}<p class="dim">no connectors configured; ask your operator</p>{{end}}
  <label>azure project <input type="text" name="azure_project" required value="{{.Trigger.Config.AzureProject}}"></label>
  <label>pipeline id <input type="text" name="azure_pipeline_id" required placeholder="42"></label>
  {{end}}
</div>
{{end}}

{{define "content"}}
<div class="settings">
  <div class="repohdr"><span class="path"><a href="/{{.Tenant}}/{{.Repo}}/settings/triggers">{{.Tenant}}/{{.Repo}} · triggers</a></span></div>
  <h2>{{if .Editing}}edit trigger{{else}}add trigger{{end}}</h2>
  <form method="post" action="/{{.Tenant}}/{{.Repo}}/settings/triggers/{{if .Editing}}edit{{else}}add{{end}}">
    <input type="hidden" name="csrf_token" value="{{.CSRF}}">
    {{if .Editing}}<input type="hidden" name="id" value="{{.Trigger.ID}}">{{end}}
    <label>name <input type="text" name="name" required value="{{.Trigger.Name}}" placeholder="ci-main"></label>
    {{if .Editing}}
    <p class="dim">kind: <strong>{{.Kind}}</strong> (immutable — delete and recreate to change kind)</p>
    {{else}}
    <label>kind
      <select name="kind"
              hx-get="/{{.Tenant}}/{{.Repo}}/settings/triggers/new"
              hx-target="#kindfields" hx-swap="outerHTML" hx-select="#kindfields"
              hx-trigger="change" hx-include="this">
        <option value="generic" {{if eq .Kind "generic"}}selected{{end}}>generic</option>
        <option value="cloudbuild" {{if eq .Kind "cloudbuild"}}selected{{end}}>cloudbuild</option>
        <option value="codebuild" {{if eq .Kind "codebuild"}}selected{{end}}>codebuild</option>
        <option value="azurewebhook" {{if eq .Kind "azurewebhook"}}selected{{end}}>azurewebhook</option>
        <option value="azurepipelines" {{if eq .Kind "azurepipelines"}}selected{{end}}>azurepipelines</option>
      </select>
    </label>
    <noscript><button type="submit" formmethod="get" formaction="/{{.Tenant}}/{{.Repo}}/settings/triggers/new">show fields for selected kind</button></noscript>
    {{end}}
    {{if not .Editing}}{{template "kindfields" .}}{{end}}
    <label>ref include <input type="text" name="ref_include" value="{{range $i, $r := .Trigger.RefInclude}}{{if $i}}, {{end}}{{$r}}{{end}}" placeholder="refs/heads/main, refs/tags/v*"></label>
    <label>ref exclude <input type="text" name="ref_exclude" value="{{range $i, $r := .Trigger.RefExclude}}{{if $i}}, {{end}}{{$r}}{{end}}"></label>
    <label>token mode
      <select name="token_mode">
        <option value="none" {{if eq (printf "%s" .Trigger.TokenMode) "none"}}selected{{end}}>none</option>
        <option value="inject" {{if eq (printf "%s" .Trigger.TokenMode) "inject"}}selected{{end}}>inject</option>
      </select>
    </label>
    <label>token scopes <input type="text" name="token_scopes" placeholder="repo:read"></label>
    <label>token ttl <input type="text" name="token_ttl" placeholder="15m (max 1h)"></label>
    <button type="submit">{{if .Editing}}save{{else}}add trigger{{end}}</button>
  </form>
</div>
{{end}}
{{if not .HXFragment}}{{template "base" .}}{{end}}
```

When `HXFragment` is true the template renders the inner `content` fragment for htmx to swap into `#trigger-modal`; when false it wraps in `base` for the full-page no-JS path. For the kind-swap htmx call, htmx requests `/new?kind=X` (HX-Request true) and `hx-select="#kindfields"` extracts just that block. Confirm `renderBuffered` renders the `content` define when `base` is not invoked — if the renderer requires a top-level template name, render the `kindfields` define directly for the `HX-Request` + has-`kind`-only case via `s.render.renderPartial` (see `render.go:197`). Simplest robust split: in `triggersNewForm`, if `HX-Request` AND a `kind` query param is present AND no full-modal requested, call `renderPartial("kindfields", d)`; else render the full form. Adjust the handler accordingly:

```go
	if d.HXFragment && r.URL.Query().Get("kind") != "" && r.URL.Query().Get("id") == "" {
		if err := s.render.renderPartial(w, "kindfields", d); err != nil {
			s.renderError(w, r, http.StatusInternalServerError, "render error")
		}
		return
	}
```

(Match `renderPartial`'s real signature from `render.go`.)

- [ ] **Step 6: Run tests**

Run: `go test ./internal/web/ -run 'TestTriggersNewForm|TestTriggersAdd' -v`
Expected: PASS. Run `go build ./...`.

- [ ] **Step 7: Commit**

```bash
git add internal/web/reposettings_triggers.go internal/web/templates/reposettings_triggers_form.html internal/web/reposettings_triggers_test.go
git commit -m "feat(web): trigger create form (per-kind fields) + add handler"
```

---

## Task 11: Edit, enable/disable, remove, rotate-secret handlers

**Files:**
- Modify: `internal/web/reposettings_triggers.go` (replace remaining stubs)
- Test: `internal/web/reposettings_triggers_test.go`

- [ ] **Step 1: Write failing tests**

```go
func TestTriggersEdit_KindImmutable(t *testing.T) {
	fake := &fakeTriggers{getTrigger: buildtrigger.Trigger{ID: "bvbt_1", Tenant: "acme", Repo: "widgets", Name: "ci", Kind: "cloudbuild"}}
	srv := newTriggersTestServer(t, fake)
	form := url.Values{"csrf_token": {testCSRF}, "id": {"bvbt_1"}, "name": {"ci2"}, "token_mode": {"none"}, "active": {"on"}}
	rec := doPostForm(t, srv, "/acme/widgets/settings/triggers/edit", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d %s", rec.Code, rec.Body.String())
	}
	if fake.editID != "bvbt_1" || fake.editIn.Name != "ci2" {
		t.Errorf("edit not called correctly: %+v", fake.editIn)
	}
}

func TestTriggersRemove_OwnershipEnforced(t *testing.T) {
	// trigger belongs to a DIFFERENT repo → 404, Remove never called
	fake := &fakeTriggers{getTrigger: buildtrigger.Trigger{ID: "bvbt_x", Tenant: "other", Repo: "repo", Name: "ci"}}
	srv := newTriggersTestServer(t, fake)
	form := url.Values{"csrf_token": {testCSRF}, "id": {"bvbt_x"}}
	rec := doPostForm(t, srv, "/acme/widgets/settings/triggers/remove", form)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 for foreign trigger, got %d", rec.Code)
	}
	if fake.removed {
		t.Errorf("Remove called on foreign trigger")
	}
}

func TestTriggersRotateSecret_ShownOnce(t *testing.T) {
	fake := &fakeTriggers{getTrigger: buildtrigger.Trigger{ID: "bvbt_1", Tenant: "acme", Repo: "widgets", Kind: "cloudbuild"}, rotateSecret: "rotatedvalue"}
	srv := newTriggersTestServer(t, fake)
	form := url.Values{"csrf_token": {testCSRF}, "id": {"bvbt_1"}}
	rec := doPostForm(t, srv, "/acme/widgets/settings/triggers/rotate-secret", form)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "rotatedvalue") {
		t.Fatalf("want secret-once page, got %d %s", rec.Code, rec.Body.String())
	}
}
```

Extend `fakeTriggers` with `getTrigger`, `editID`, `editIn`, `removed`, `rotateSecret` recording fields and matching method impls.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/web/ -run 'TestTriggersEdit|TestTriggersRemove|TestTriggersRotate' -v`
Expected: FAIL — stubs 404.

- [ ] **Step 3: Implement the handlers** (replace stubs)

```go
func (s *server) triggersEdit(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if s.triggers == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	tr, ok := s.ownTriggerOr404(w, r, sr, r.PostFormValue("id"))
	if !ok {
		return
	}
	in := buildtrigger.EditInput{
		Name:       r.PostFormValue("name"),
		RefInclude: splitCSVField(r.PostFormValue("ref_include")),
		RefExclude: splitCSVField(r.PostFormValue("ref_exclude")),
		TokenMode:  buildtrigger.TokenMode(r.PostFormValue("token_mode")),
		Active:     r.PostFormValue("active") == "on",
	}
	if sc := r.PostFormValue("token_scopes"); sc != "" {
		scopes, err := auth.ParseScopes(sc)
		if err != nil {
			EmitAdminActionMetric(r.Context(), s.logger, "trigger", "edit", "invalid")
			s.redirectFlash(w, r, sr.triggersBase(), "invalid token scopes: "+err.Error())
			return
		}
		in.TokenScopes = scopes
	}
	if ttl := r.PostFormValue("token_ttl"); ttl != "" {
		d, err := time.ParseDuration(ttl)
		if err != nil {
			EmitAdminActionMetric(r.Context(), s.logger, "trigger", "edit", "invalid")
			s.redirectFlash(w, r, sr.triggersBase(), "invalid token ttl: "+err.Error())
			return
		}
		in.TokenTTL = d
	}
	if _, err := s.triggers.Edit(r.Context(), tr.ID, in); err != nil {
		if errors.Is(err, buildtrigger.ErrConflict) {
			EmitAdminActionMetric(r.Context(), s.logger, "trigger", "edit", "invalid")
			s.redirectFlash(w, r, sr.triggersBase(), "a trigger with that name already exists")
			return
		}
		if errors.Is(err, buildtrigger.ErrInvalidInput) {
			EmitAdminActionMetric(r.Context(), s.logger, "trigger", "edit", "invalid")
			s.redirectFlash(w, r, sr.triggersBase(), err.Error())
			return
		}
		s.logger.Error("triggers: edit", "id", tr.ID, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "trigger", "edit", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "buildtrigger.edited",
		slog.String("tenant", sr.tenant), slog.String("repo", sr.repo), slog.String("trigger_id", tr.ID))
	EmitAdminActionMetric(r.Context(), s.logger, "trigger", "edit", "ok")
	s.redirectFlash(w, r, sr.triggersBase(), "trigger updated")
}

func (s *server) triggersSetActive(w http.ResponseWriter, r *http.Request, sr settingsRoute, active bool) {
	if s.triggers == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	tr, ok := s.ownTriggerOr404(w, r, sr, r.PostFormValue("id"))
	if !ok {
		return
	}
	action, verb := "disable", s.triggers.Disable
	if active {
		action, verb = "enable", s.triggers.Enable
	}
	if err := verb(r.Context(), tr.ID); err != nil {
		if errors.Is(err, buildtrigger.ErrNotFound) {
			s.renderError(w, r, http.StatusNotFound, "not found")
			return
		}
		s.logger.Error("triggers: set active", "id", tr.ID, "active", active, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "trigger", action, "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "buildtrigger."+action+"d",
		slog.String("tenant", sr.tenant), slog.String("repo", sr.repo), slog.String("trigger_id", tr.ID))
	EmitAdminActionMetric(r.Context(), s.logger, "trigger", action, "ok")
	s.redirectFlash(w, r, sr.triggersBase(), "trigger "+action+"d")
}

func (s *server) triggersRemove(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if s.triggers == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	tr, ok := s.ownTriggerOr404(w, r, sr, r.PostFormValue("id"))
	if !ok {
		return
	}
	if err := s.triggers.Remove(r.Context(), tr.ID); err != nil {
		if errors.Is(err, buildtrigger.ErrNotFound) {
			s.renderError(w, r, http.StatusNotFound, "not found")
			return
		}
		s.logger.Error("triggers: remove", "id", tr.ID, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "trigger", "remove", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "buildtrigger.removed",
		slog.String("tenant", sr.tenant), slog.String("repo", sr.repo), slog.String("trigger_id", tr.ID))
	EmitAdminActionMetric(r.Context(), s.logger, "trigger", "remove", "ok")
	s.redirectFlash(w, r, sr.triggersBase(), "trigger removed")
}

func (s *server) triggersRotateSecret(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if s.triggers == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	tr, ok := s.ownTriggerOr404(w, r, sr, r.PostFormValue("id"))
	if !ok {
		return
	}
	secret, err := s.triggers.RotateSecret(r.Context(), tr.ID)
	if err != nil {
		if errors.Is(err, buildtrigger.ErrInvalidInput) {
			EmitAdminActionMetric(r.Context(), s.logger, "trigger", "rotate_secret", "invalid")
			s.redirectFlash(w, r, sr.triggersBase(), "this trigger kind has no rotatable secret")
			return
		}
		if errors.Is(err, buildtrigger.ErrNotFound) {
			s.renderError(w, r, http.StatusNotFound, "not found")
			return
		}
		s.logger.Error("triggers: rotate secret", "id", tr.ID, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "trigger", "rotate_secret", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "buildtrigger.secret_rotated",
		slog.String("tenant", sr.tenant), slog.String("repo", sr.repo), slog.String("trigger_id", tr.ID))
	EmitAdminActionMetric(r.Context(), s.logger, "trigger", "rotate_secret", "ok")
	s.renderSecretOnce(w, r, "build trigger secret rotated", secret, sr.triggersBase())
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/web/ -run 'TestTriggers' -v`
Expected: PASS (all triggers tests). Run `go build ./...`.

- [ ] **Step 5: Commit**

```bash
git add internal/web/reposettings_triggers.go internal/web/reposettings_triggers_test.go
git commit -m "feat(web): trigger edit/enable/disable/remove/rotate-secret handlers"
```

---

## Task 12: Deliveries page (paginated) + bounded replay

**Files:**
- Modify: `internal/web/reposettings_triggers.go` (replace `triggersDeliveries`/`triggersReplay` stubs)
- Create: `internal/web/templates/reposettings_triggers_deliveries.html`
- Test: `internal/web/reposettings_triggers_test.go`

- [ ] **Step 1: Write failing tests**

```go
func TestTriggersDeliveries_PageAndReplayWindow(t *testing.T) {
	now := time.Now()
	fake := &fakeTriggers{
		getTrigger: buildtrigger.Trigger{ID: "bvbt_1", Tenant: "acme", Repo: "widgets", Name: "ci"},
		page: []buildtrigger.Delivery{
			{ID: "d2", TriggerID: "bvbt_1", Status: "delivered", CreatedAt: now},
			{ID: "d1", TriggerID: "bvbt_1", Status: "dead_letter", CreatedAt: now.Add(-time.Hour)},
		},
		recent: []string{"d2", "d1"}, // both replayable
	}
	srv := newTriggersTestServer(t, fake)
	rec := doGet(t, srv, "/acme/widgets/settings/triggers/deliveries?trigger=bvbt_1")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "dead_letter") {
		t.Fatalf("deliveries page: %d %s", rec.Code, rec.Body.String())
	}
}

func TestTriggersReplay_OutOfWindowRejected(t *testing.T) {
	fake := &fakeTriggers{
		getTrigger: buildtrigger.Trigger{ID: "bvbt_1", Tenant: "acme", Repo: "widgets"},
		getDelivery: buildtrigger.Delivery{ID: "old", TriggerID: "bvbt_1"},
		recent:      []string{"d2", "d1"}, // "old" is NOT in the recent set
	}
	srv := newTriggersTestServer(t, fake)
	form := url.Values{"csrf_token": {testCSRF}, "trigger_id": {"bvbt_1"}, "delivery_id": {"old"}}
	rec := doPostForm(t, srv, "/acme/widgets/settings/triggers/replay", form)
	if rec.Code != http.StatusSeeOther { // flash redirect
		t.Fatalf("want 303 flash, got %d", rec.Code)
	}
	if fake.replayed {
		t.Errorf("replay called for out-of-window delivery")
	}
}

func TestTriggersReplay_CrossTriggerRejected(t *testing.T) {
	fake := &fakeTriggers{
		getTrigger:  buildtrigger.Trigger{ID: "bvbt_1", Tenant: "acme", Repo: "widgets"},
		getDelivery: buildtrigger.Delivery{ID: "d2", TriggerID: "bvbt_OTHER"}, // belongs to another trigger
		recent:      []string{"d2"},
	}
	srv := newTriggersTestServer(t, fake)
	form := url.Values{"csrf_token": {testCSRF}, "trigger_id": {"bvbt_1"}, "delivery_id": {"d2"}}
	rec := doPostForm(t, srv, "/acme/widgets/settings/triggers/replay", form)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 for cross-trigger delivery, got %d", rec.Code)
	}
	if fake.replayed {
		t.Errorf("replay called across triggers")
	}
}
```

Extend `fakeTriggers`: `page []buildtrigger.Delivery`, `recent []string`, `getDelivery buildtrigger.Delivery`, `replayed bool`; implement `ListDeliveriesPage`, `RecentDeliveryIDs`, `Get` (delivery is `GetDelivery` — note the interface name), `ReplayDelivery`.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/web/ -run 'TestTriggersDeliveries|TestTriggersReplay' -v`
Expected: FAIL — stubs 404.

- [ ] **Step 3: Add `GetDelivery` to the interface**

The replay ownership check needs `GetDelivery`. Add to `TriggerAdmin` (services.go):
```go
	GetDelivery(ctx context.Context, id string) (buildtrigger.Delivery, error)
```
Re-run the Task 8 compile assertion to confirm `*buildtrigger.Service` still satisfies it (it has `GetDelivery`).

- [ ] **Step 4: Implement `triggersDeliveries`**

```go
const (
	triggerDeliveriesPageSize = 20
	triggerReplayWindow       = 10
)

type triggerDeliveryRow struct {
	buildtrigger.Delivery
	CreatedRel     string
	LastErrorTrunc string
	Replayable     bool
}

type triggerDeliveriesData struct {
	base
	Tenant, Repo string
	IsAdmin      bool
	Trigger      buildtrigger.Trigger
	Status       string
	Deliveries   []triggerDeliveryRow
	NextCursor   string // unix seconds of the last row's created_at, "" if no more
}

func (s *server) triggersDeliveries(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if s.triggers == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodGet {
		s.renderError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tr, ok := s.ownTriggerOr404(w, r, sr, r.URL.Query().Get("trigger"))
	if !ok {
		return
	}
	status := r.URL.Query().Get("status")
	switch status {
	case "", "pending", "in_flight", "delivered", "dead_letter":
	default:
		status = "" // ignore unknown filters
	}
	var before time.Time
	if c := r.URL.Query().Get("before"); c != "" {
		if sec, err := strconv.ParseInt(c, 10, 64); err == nil {
			before = time.Unix(sec, 0)
		}
	}
	ds, err := s.triggers.ListDeliveriesPage(r.Context(), tr.ID, status, before, triggerDeliveriesPageSize)
	if err != nil {
		s.logger.Error("triggers: list deliveries", "trigger_id", tr.ID, "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	recent, err := s.triggers.RecentDeliveryIDs(r.Context(), tr.ID, triggerReplayWindow)
	if err != nil {
		s.logger.Error("triggers: recent ids", "trigger_id", tr.ID, "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	recentSet := make(map[string]struct{}, len(recent))
	for _, id := range recent {
		recentSet[id] = struct{}{}
	}
	rows := make([]triggerDeliveryRow, 0, len(ds))
	for _, d := range ds {
		_, replayable := recentSet[d.ID]
		rows = append(rows, triggerDeliveryRow{
			Delivery:       d,
			CreatedRel:     relTimeAt(time.Now(), d.CreatedAt.Unix()),
			LastErrorTrunc: truncateRunes(d.LastError, 80),
			Replayable:     replayable,
		})
	}
	data := triggerDeliveriesData{
		base:       base{Session: SessionFromContext(r.Context()), CSRF: issueCSRF(w, requestIsTLS(r, s.trustProxy)), Flash: takeFlash(w, r)},
		Tenant:     sr.tenant,
		Repo:       sr.repo,
		IsAdmin:    isGlobalAdmin(r),
		Trigger:    tr,
		Status:     status,
		Deliveries: rows,
	}
	if len(ds) == triggerDeliveriesPageSize {
		data.NextCursor = strconv.FormatInt(ds[len(ds)-1].CreatedAt.Unix(), 10)
	}
	if err := s.renderBuffered(w, "reposettings_triggers_deliveries.html", data); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "render error")
		return
	}
	EmitRequestMetric(r.Context(), s.logger, "reposettings_triggers_deliveries", http.StatusOK)
}
```

- [ ] **Step 5: Implement `triggersReplay`** (bounded + cross-trigger guard)

```go
func (s *server) triggersReplay(w http.ResponseWriter, r *http.Request, sr settingsRoute) {
	if s.triggers == nil {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	// 1. The posted trigger must belong to this (tenant, repo).
	tr, ok := s.ownTriggerOr404(w, r, sr, r.PostFormValue("trigger_id"))
	if !ok {
		return
	}
	deliveryID := r.PostFormValue("delivery_id")
	if deliveryID == "" {
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	// 2. The delivery must belong to that trigger (cross-trigger guard).
	d, err := s.triggers.GetDelivery(r.Context(), deliveryID)
	if err != nil || d.TriggerID != tr.ID {
		if err != nil && !errors.Is(err, buildtrigger.ErrNotFound) {
			s.logger.Error("triggers replay: get delivery", "delivery_id", deliveryID, "err", err)
		}
		s.renderError(w, r, http.StatusNotFound, "not found")
		return
	}
	backURL := sr.triggerDeliveriesURL(tr.ID)
	// 3. The delivery must be within the recent-N replay window (authoritative
	// server-side gate; the UI only hides the link for older rows).
	recent, err := s.triggers.RecentDeliveryIDs(r.Context(), tr.ID, triggerReplayWindow)
	if err != nil {
		s.logger.Error("triggers replay: recent ids", "trigger_id", tr.ID, "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	inWindow := false
	for _, id := range recent {
		if id == deliveryID {
			inWindow = true
			break
		}
	}
	if !inWindow {
		EmitAdminActionMetric(r.Context(), s.logger, "trigger", "delivery_replay", "invalid")
		s.redirectFlash(w, r, backURL, "only the 10 most recent deliveries can be replayed")
		return
	}
	if err := s.triggers.ReplayDelivery(r.Context(), deliveryID); err != nil {
		EmitAdminActionMetric(r.Context(), s.logger, "trigger", "delivery_replay", "error")
		if errors.Is(err, buildtrigger.ErrReplayInFlight) {
			s.redirectFlash(w, r, backURL, "delivery attempt in progress; try again shortly")
			return
		}
		if errors.Is(err, buildtrigger.ErrNotFound) {
			s.renderError(w, r, http.StatusNotFound, "not found")
			return
		}
		s.logger.Error("triggers: replay delivery", "delivery_id", deliveryID, "err", err)
		s.redirectFlash(w, r, backURL, "replay failed (internal error); see server log")
		return
	}
	s.emitAdmin(r.Context(), "buildtrigger.delivery_replayed",
		slog.String("tenant", sr.tenant), slog.String("repo", sr.repo),
		slog.String("trigger_id", tr.ID), slog.String("delivery_id", deliveryID))
	EmitAdminActionMetric(r.Context(), s.logger, "trigger", "delivery_replay", "ok")
	s.redirectFlash(w, r, backURL, "delivery queued for replay")
}
```

- [ ] **Step 6: Create the deliveries template**

Create `internal/web/templates/reposettings_triggers_deliveries.html`:

```html
{{define "title"}}{{.Tenant}}/{{.Repo}} · {{.Trigger.Name}} deliveries · bucketvcs{{end}}
{{define "content"}}
<div class="settings">
  <div class="repohdr"><span class="path"><a href="/{{.Tenant}}/{{.Repo}}/settings/triggers">{{.Tenant}}/{{.Repo}} · triggers</a></span></div>
  <h2>{{.Trigger.Name}} — deliveries</h2>
  <p class="tabs">
    <a href="?trigger={{.Trigger.ID}}">all</a> ·
    <a href="?trigger={{.Trigger.ID}}&status=pending">pending</a> ·
    <a href="?trigger={{.Trigger.ID}}&status=in_flight">in_flight</a> ·
    <a href="?trigger={{.Trigger.ID}}&status=delivered">delivered</a> ·
    <a href="?trigger={{.Trigger.ID}}&status=dead_letter">dead_letter</a>
  </p>
  {{if .Deliveries}}
  <table>
    <thead>
      <tr><th>created</th><th>status</th><th>attempts</th><th>http</th><th>last error</th><th>delivered</th><th></th></tr>
    </thead>
    <tbody>
    {{range .Deliveries}}
    <tr>
      <td title="{{abstime .CreatedAt.Unix}}">{{.CreatedRel}}</td>
      <td>{{.Status}}</td>
      <td>{{.Attempts}}</td>
      <td>{{if .LastStatusCode}}{{.LastStatusCode}}{{else}}—{{end}}</td>
      <td class="mono">{{if .LastErrorTrunc}}{{.LastErrorTrunc}}{{else}}—{{end}}</td>
      <td>{{if .DeliveredAt}}<span class="on">✓</span>{{else}}—{{end}}</td>
      <td class="acts">
        {{if .Replayable}}
        <form method="post" action="/{{$.Tenant}}/{{$.Repo}}/settings/triggers/replay" class="inline">
          <input type="hidden" name="csrf_token" value="{{$.CSRF}}">
          <input type="hidden" name="trigger_id" value="{{$.Trigger.ID}}">
          <input type="hidden" name="delivery_id" value="{{.ID}}">
          <button type="submit">replay</button>
        </form>
        {{end}}
      </td>
    </tr>
    {{end}}
    </tbody>
  </table>
  <div class="pager">
    {{if .NextCursor}}<a href="?trigger={{.Trigger.ID}}{{if .Status}}&status={{.Status}}{{end}}&before={{.NextCursor}}">older</a>{{end}}
  </div>
  {{else}}
  <p class="empty">no deliveries{{if .Status}} with status {{.Status}}{{end}}.</p>
  {{end}}
</div>
{{end}}
{{template "base" .}}
```

(Confirm `abstime` is a registered template func — it is used in `reposettings_webhooks.html`.)

- [ ] **Step 7: Run tests**

Run: `go test ./internal/web/ -run 'TestTriggers' -v`
Expected: PASS. Run `go build ./... && go vet ./internal/web/ ./internal/buildtrigger/`.

- [ ] **Step 8: Commit**

```bash
git add internal/web/reposettings_triggers.go internal/web/services.go internal/web/templates/reposettings_triggers_deliveries.html internal/web/reposettings_triggers_test.go
git commit -m "feat(web): trigger deliveries page (paginated) + bounded replay"
```

---

## Task 13: Smoke script + operator guide

**Files:**
- Create: `scripts/smoke-triggers-ui.sh` (model on an existing smoke script, e.g. the webhooks/phase-3 one)
- Modify: the web-ui operator guide under `docs/operator-guides/`

- [ ] **Step 1: Find the closest existing smoke script**

Run: `ls scripts/ | grep -iE 'smoke|web|phase3|webhook'` and read the most similar one to copy the serve-bootstrap (`bucketvcs serve --ui ... --build-triggers ... --build-config ...`), login, and curl-with-CSRF helpers.

- [ ] **Step 2: Write the smoke script**

Create `scripts/smoke-triggers-ui.sh` performing, against a localfs serve with `--build-triggers=true` and a `--build-config` defining one AWS + one Azure connector:
1. bootstrap admin + a repo (reuse existing helpers).
2. `GET /{t}/{r}/settings/triggers` → assert 200 + "no triggers configured".
3. `POST …/triggers/add` kind=generic url=… → assert the secret-once page (grep a base64-ish secret).
4. `POST …/triggers/add` kind=codebuild with the configured connector → assert 303 + "trigger created".
5. `GET …/triggers` → assert both names present.
6. Push a commit matching a trigger's ref to fire it (reuse the repo-push helper); poll `GET …/triggers/deliveries?trigger=<id>` until a row appears.
7. `POST …/triggers/replay` for that row → assert "queued for replay".
8. `POST …/triggers/replay` with a fabricated old delivery_id → assert the "only the 10 most recent" flash (303) and no state change.
9. `POST …/triggers/edit` rename → assert "trigger updated".
10. `POST …/triggers/disable` then `…/remove` → assert flashes; final `GET …/triggers` shows "no triggers configured".

Make it executable: `chmod +x scripts/smoke-triggers-ui.sh`. Keep assertions as `grep -q` with explicit failure messages, matching the existing smoke style.

- [ ] **Step 3: Run the smoke script**

Run: `./scripts/smoke-triggers-ui.sh`
Expected: prints step-by-step OK lines and exits 0. Fix any handler/template mismatch surfaced here.

- [ ] **Step 4: Operator guide**

Add a "Build Triggers" subsection to the web-ui operator guide (find it: `grep -rl 'web ui\|Web UI\|reposettings\|webhooks tab' docs/operator-guides/`). Document: the Triggers tab is repo-admin; the five kinds and their fields; that `codebuild`/`azurepipelines` require an operator-configured connector in `--build-config` (the dropdown lists configured names; empty ⇒ none configured); secret-once for generic/cloudbuild; rotate-secret applies only to those two; delivery history shows 20/page and only the 10 most recent deliveries are replayable.

- [ ] **Step 5: Commit**

```bash
git add scripts/smoke-triggers-ui.sh docs/operator-guides/
git commit -m "docs(triggers-ui): smoke script + operator guide section"
```

---

## Final verification

- [ ] **Run the full suite:** `go test ./... 2>&1 | tail -30` — all green.
- [ ] **Vet/build:** `go build ./... && go vet ./...`.
- [ ] **gofmt:** `gofmt -l internal/web internal/buildtrigger cmd/bucketvcs` — no output (no drift).
- [ ] **Manual browser pass (optional):** serve with `--ui --build-triggers=true --build-config=<yaml>`, open `/{t}/{r}/settings/triggers`, exercise create (each kind), edit, deliveries, replay — confirm it works WITH JS disabled too (forms + `/new?kind=X` full-page fallback).

---

## Notes on conventions used throughout

- **Anti-enumeration:** foreign/missing triggers and deliveries always return an identical 404; ownership is checked via `ownTriggerOr404` (Get + tenant/repo compare) before any mutate or display.
- **Result enum:** `EmitAdminActionMetric(..., result)` uses only `ok` | `invalid` | `error`. User-correctable problems (bad input, conflict, out-of-window) are `invalid`; backend faults are `error`.
- **Flash vs 500:** client-correctable → `redirectFlash` (303); server faults → `renderError` 500 with the detail logged, never leaked.
- **Secret hygiene:** secrets only ever surface via `renderSecretOnce` (Cache-Control no-store); list/edit show only `SecretPreview`.
- **Audit names:** `buildtrigger.created|edited|enabled|disabled|removed|secret_rotated|delivery_replayed`, `source=web` via `s.emitAdmin`. If the M30 worker already emits any `buildtrigger.*` audit names, keep these web-origin names distinct (they carry the web actor) — do not rename the worker's.
```
