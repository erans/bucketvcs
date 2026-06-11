# Sessions CLI + Audit-Reader Date-Partition Walk Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `bucketvcs session list/revoke` CLI and replace the audit Reader's full-prefix listing with a backward day-partition walk.

**Architecture:** The CLI is a new top-level `session` command group over existing sqlitestore methods (`ListAllSessions`, `DeleteSessionByHash`, `DeleteSessionsForUser`, `GetUserByName`) — no new store methods. The Reader change is internal to `internal/auditlog`: `Page` keeps its signature and cursor semantics but discovers keys by listing one `…/activity/YYYY/MM/DD/` prefix at a time, walking backward from the cursor's day (or today) to a floor day learned from one `List(prefix, MaxKeys:1)` probe. A new documented ordering guarantee on `ObjectStore.List` (lexicographically ascending) backs the probe, with a conformance assertion.

**Tech Stack:** Go stdlib only (flag, encoding/json, time). Spec: `docs/superpowers/specs/2026-06-11-sessions-cli-audit-day-walk-design.md`.

---

### Task 1: Document + conformance-test the List ordering guarantee

**Files:**
- Modify: `internal/storage/objectstore.go` (the `List` method doc, ~line 68)
- Modify: `internal/storage/conformance/correctness.go` (extend `testListPagination`, ~line 387)

- [ ] **Step 1: Extend the conformance test to assert ascending order across pages**

In `internal/storage/conformance/correctness.go`, `testListPagination` currently collects keys into a `map[string]bool`. Add an ordered slice and an ascending assertion. Replace the function body's collection loop with:

```go
func testListPagination(t *testing.T, f Factory) {
	s := newStore(t, f)
	const total = 25
	for i := 0; i < total; i++ {
		if _, err := s.PutIfAbsent(ctx(), Key("p/page", i), bytes.NewReader([]byte{byte(i)}), nil); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	got := map[string]bool{}
	var ordered []string
	token := ""
	for iter := 0; iter < 100; iter++ {
		page, err := s.List(ctx(), "p/page/", &storage.ListOptions{MaxKeys: 7, ContinuationToken: token})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, md := range page.Objects {
			if got[md.Key] {
				t.Errorf("duplicate key in pagination: %s", md.Key)
			}
			got[md.Key] = true
			ordered = append(ordered, md.Key)
		}
		if page.NextToken == "" {
			break
		}
		if len(page.Objects) > 7 {
			t.Errorf("page returned %d objects, want <= 7", len(page.Objects))
		}
		token = page.NextToken
	}
	if len(got) != total {
		t.Errorf("paginated total = %d, want %d", len(got), total)
	}
	// Ordering contract: List returns keys lexicographically ascending,
	// within a page AND across pages. internal/auditlog's day-walk floor
	// probe (List MaxKeys:1 -> oldest key) depends on this.
	if !sort.StringsAreSorted(ordered) {
		t.Errorf("List keys not lexicographically ascending across pages: %v", ordered)
	}
}
```

Add `"sort"` to the file's imports if not present.

- [ ] **Step 2: Run the localfs conformance suite**

Run: `go test ./internal/storage/localfs/ -run 'Conformance' -v 2>&1 | tail -5`
Expected: PASS (localfs sorts at `localfs.go:558`).

- [ ] **Step 3: Document the guarantee on the interface**

In `internal/storage/objectstore.go`, replace the `List` doc comment:

```go
	// List returns one page of objects under prefix. Keys are returned in
	// lexicographically ascending order, both within a page and across
	// pages of one logical listing (S3/GCS/Azure guarantee this natively;
	// localfs sorts). Callers may rely on the first key of an unfiltered
	// listing being the lexicographic minimum under the prefix.
	List(ctx context.Context, prefix string, opts *ListOptions) (*ListPage, error)
```

- [ ] **Step 4: Build + commit**

```bash
go build ./... && go test ./internal/storage/... 2>&1 | tail -3
git add internal/storage/objectstore.go internal/storage/conformance/correctness.go
git commit -m "feat(storage): document + conformance-test List ascending-order guarantee"
```

---

### Task 2: Reader date-partition walk — update fixtures, add failing tests

**Files:**
- Modify: `internal/auditlog/reader_test.go`

The existing tests seed flat keys (`sys/logs/activity/120000`) that never occur in production — shiplog writes `…/activity/YYYY/MM/DD/HHMMSS-<instance>-<seq>.ndjson.gz` (see `internal/shiplog/ship.go:31` `bucketKeyForPending`). The day-walk only discovers date-sharded keys, so the fixtures must move to the real layout first.

- [ ] **Step 1: Mechanically update existing fixtures to date-sharded keys**

In `internal/auditlog/reader_test.go`, replace every fixture key. The old keys encode ordering in a flat segment; map them onto one day so all ordering-sensitive tests keep their relative order:

```
sys/logs/activity/120000  ->  sys/logs/activity/2026/05/22/120000-aa-000001.ndjson.gz
sys/logs/activity/130000  ->  sys/logs/activity/2026/05/22/130000-aa-000002.ndjson.gz
sys/logs/activity/140000  ->  sys/logs/activity/2026/05/22/140000-aa-000003.ndjson.gz
```

Apply with:

```bash
sed -i 's|sys/logs/activity/120000|sys/logs/activity/2026/05/22/120000-aa-000001.ndjson.gz|g; s|sys/logs/activity/130000|sys/logs/activity/2026/05/22/130000-aa-000002.ndjson.gz|g; s|sys/logs/activity/140000|sys/logs/activity/2026/05/22/140000-aa-000003.ndjson.gz|g' internal/auditlog/reader_test.go
```

Then check no other flat keys remain: `grep -n 'activity/[0-9]' internal/auditlog/reader_test.go` — expected: no matches outside the date-sharded form.

- [ ] **Step 2: Run existing Reader tests — must still pass against the OLD implementation**

Run: `go test ./internal/auditlog/ -run TestReaderPage 2>&1 | tail -3`
Expected: PASS (the old full-prefix lister is layout-agnostic; this proves the fixture change is behavior-neutral before the rewrite).

Commit the fixture move on its own:

```bash
git add internal/auditlog/reader_test.go
git commit -m "test(auditlog): move Reader fixtures to the production date-sharded key layout"
```

- [ ] **Step 3: Add the new failing tests**

Append to `internal/auditlog/reader_test.go`. `countingListStore` wraps `fakeStore` to count `List` calls so tests can prove the walk does NOT enumerate the full prefix:

```go
// countingListStore counts List calls (and records prefixes) to prove the
// day-walk lists only the partitions it needs.
type countingListStore struct {
	*fakeStore
	listCalls    int
	listPrefixes []string
}

func (s *countingListStore) List(ctx context.Context, prefix string, opts *storage.ListOptions) (*storage.ListPage, error) {
	s.listCalls++
	s.listPrefixes = append(s.listPrefixes, prefix)
	return s.fakeStore.List(ctx, prefix, opts)
}

func dayKey(day, hhmmss string, seq int) string {
	return fmt.Sprintf("sys/logs/activity/%s/%s-aa-%06d.ndjson.gz", day, hhmmss, seq)
}

func TestReaderPage_WalksDaysNewestFirst(t *testing.T) {
	inner := newFakeStore()
	inner.put(dayKey("2026/06/01", "120000", 1), gzLines(
		`{"ts":"2026-06-01T12:00:00Z","event":"a","tenant":"acme","repo":"app"}`,
	))
	inner.put(dayKey("2026/06/03", "120000", 2), gzLines(
		`{"ts":"2026-06-03T12:00:00Z","event":"b","tenant":"acme","repo":"app"}`,
	))
	inner.put(dayKey("2026/06/05", "120000", 3), gzLines(
		`{"ts":"2026-06-05T12:00:00Z","event":"c","tenant":"acme","repo":"app"}`,
	))
	store := &countingListStore{fakeStore: inner}

	r := auditlog.NewReader(store, "")
	// Page 1 capped at 2 objects -> c, b; cursor points at b's object.
	r.ObjectsPerPage = 2
	evs, next, err := r.Page(context.Background(), auditlog.Filter{}, "")
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if got := eventNames(evs); len(got) != 2 || got[0] != "c" || got[1] != "b" {
		t.Fatalf("page1: got %v want [c b]", got)
	}
	if next == "" {
		t.Fatal("page1: want non-empty cursor")
	}

	// Page 2 resumes across the day gap to a; floor reached -> empty cursor.
	evs2, next2, err := r.Page(context.Background(), auditlog.Filter{}, next)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if got := eventNames(evs2); len(got) != 1 || got[0] != "a" {
		t.Fatalf("page2: got %v want [a]", got)
	}
	if next2 != "" {
		t.Fatalf("page2: want empty cursor, got %q", next2)
	}
}

func TestReaderPage_DoesNotListFullPrefix(t *testing.T) {
	inner := newFakeStore()
	// Objects on 3 days spread over a year; page size 1 must NOT touch
	// every day between them.
	inner.put(dayKey("2025/07/01", "120000", 1), gzLines(
		`{"ts":"2025-07-01T12:00:00Z","event":"old","tenant":"acme","repo":"app"}`,
	))
	inner.put(dayKey("2026/06/04", "120000", 2), gzLines(
		`{"ts":"2026-06-04T12:00:00Z","event":"mid","tenant":"acme","repo":"app"}`,
	))
	inner.put(dayKey("2026/06/05", "120000", 3), gzLines(
		`{"ts":"2026-06-05T12:00:00Z","event":"new","tenant":"acme","repo":"app"}`,
	))
	store := &countingListStore{fakeStore: inner}

	r := auditlog.NewReader(store, "")
	r.ObjectsPerPage = 1
	// Cursor at the newest object's key: page 2 starts on 2026/06/05.
	evs, next, err := r.Page(context.Background(), auditlog.Filter{}, "")
	if err != nil || len(evs) != 1 || evs[0].Event != "new" {
		t.Fatalf("page1: evs=%v err=%v", eventNames(evs), err)
	}
	calls := store.listCalls
	evs2, _, err := r.Page(context.Background(), auditlog.Filter{}, next)
	if err != nil || len(evs2) != 1 || evs2[0].Event != "mid" {
		t.Fatalf("page2: evs=%v err=%v", eventNames(evs2), err)
	}
	// Page 2: 1 floor probe + day lists for 06/05 (cursor day, filtered
	// empty) + 06/04. It must NOT walk the ~340 empty days down to the
	// floor once the page is full.
	if got := store.listCalls - calls; got > 4 {
		t.Fatalf("page2 used %d List calls, want <= 4 (no full-prefix walk)", got)
	}
}

func TestReaderPage_SinceUntilNarrowWalkRange(t *testing.T) {
	inner := newFakeStore()
	inner.put(dayKey("2026/06/01", "120000", 1), gzLines(
		`{"ts":"2026-06-01T12:00:00Z","event":"a","tenant":"acme","repo":"app"}`,
	))
	inner.put(dayKey("2026/06/03", "120000", 2), gzLines(
		`{"ts":"2026-06-03T12:00:00Z","event":"b","tenant":"acme","repo":"app"}`,
	))
	inner.put(dayKey("2026/06/05", "120000", 3), gzLines(
		`{"ts":"2026-06-05T12:00:00Z","event":"c","tenant":"acme","repo":"app"}`,
	))
	store := &countingListStore{fakeStore: inner}

	f := auditlog.Filter{
		Since: time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC),
		Until: time.Date(2026, 6, 4, 23, 59, 59, 0, time.UTC),
	}
	r := auditlog.NewReader(store, "")
	evs, next, err := r.Page(context.Background(), f, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := eventNames(evs); len(got) != 1 || got[0] != "b" {
		t.Fatalf("got %v want [b]", got)
	}
	if next != "" {
		t.Fatalf("want empty cursor (since-floor reached), got %q", next)
	}
	// The walk must not list days outside [since.day, until.day]:
	// floor probe (full prefix, MaxKeys 1) + 06/04 + 06/03 + 06/02 = 4.
	for _, p := range store.listPrefixes {
		if strings.Contains(p, "2026/06/05") || strings.Contains(p, "2026/06/01") {
			t.Fatalf("walk listed out-of-range day prefix %q", p)
		}
	}
}

func TestReaderPage_DayBudgetSyntheticCursor(t *testing.T) {
	inner := newFakeStore()
	// One object far in the past; the gap from today exceeds the per-page
	// day-list budget, so page 1 returns 0 events + a synthetic cursor,
	// and a later page eventually reaches the object.
	inner.put(dayKey("2020/01/01", "120000", 1), gzLines(
		`{"ts":"2020-01-01T12:00:00Z","event":"ancient","tenant":"acme","repo":"app"}`,
	))
	inner.put(dayKey("2026/06/05", "120000", 2), gzLines(
		`{"ts":"2026-06-05T12:00:00Z","event":"new","tenant":"acme","repo":"app"}`,
	))
	store := &countingListStore{fakeStore: inner}

	r := auditlog.NewReader(store, "")
	evs, next, err := r.Page(context.Background(), auditlog.Filter{}, "")
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if got := eventNames(evs); len(got) != 1 || got[0] != "new" {
		t.Fatalf("page1: got %v want [new]", got)
	}
	if next == "" {
		t.Fatal("page1: want a cursor (older object exists)")
	}

	// Keep paging; each page burns up to maxDayListsPerPage day-lists.
	// 2026-06-05 .. 2020-01-01 is ~2350 days -> must terminate well within
	// 30 pages, ending with the ancient event and an empty cursor.
	cursor := next
	var found bool
	for i := 0; i < 30; i++ {
		evs, cursor, err = r.Page(context.Background(), auditlog.Filter{}, cursor)
		if err != nil {
			t.Fatalf("page %d: %v", i+2, err)
		}
		if len(evs) == 1 && evs[0].Event == "ancient" {
			found = true
			if cursor != "" {
				t.Fatalf("after ancient: want empty cursor, got %q", cursor)
			}
			break
		}
		if len(evs) != 0 {
			t.Fatalf("page %d: unexpected events %v", i+2, eventNames(evs))
		}
		if cursor == "" {
			t.Fatal("cursor drained before reaching the ancient event")
		}
	}
	if !found {
		t.Fatal("never reached the ancient event within 30 pages")
	}
}

func TestReaderPage_UnparseableKeyFailsLoudly(t *testing.T) {
	inner := newFakeStore()
	inner.put("sys/logs/activity/garbage.txt", []byte("junk"))
	r := auditlog.NewReader(inner, "")
	_, _, err := r.Page(context.Background(), auditlog.Filter{}, "")
	if err == nil {
		t.Fatal("want error for non-date-sharded key under the activity prefix")
	}
}
```

Add `"fmt"` and `"time"` to the test file imports if missing (`strings`, `context` already imported).

- [ ] **Step 4: Run the new tests — verify they fail**

Run: `go test ./internal/auditlog/ -run 'WalksDays|DoesNotListFullPrefix|SinceUntilNarrow|DayBudget|UnparseableKey' 2>&1 | tail -6`
Expected: FAIL — `DoesNotListFullPrefix` and `SinceUntilNarrow` fail on List-call counts (old code lists the full prefix once, so counts pass trivially? No: old `listKeys` makes exactly 1 List call, so the `<= 4` bound passes, but `SinceUntilNarrow` fails because the single full-prefix List has prefix `sys/logs/activity/` which does not contain an out-of-range day — ALSO passes). The discriminating failures are `UnparseableKeyFailsLoudly` (old code happily skips garbage) and `DayBudgetSyntheticCursor` (old code returns the ancient event on page 2 directly — the loop then sees `len(evs)==1` with event `ancient` and `cursor==""`, which PASSES too).

**Reality check:** the old implementation passes most of these because it is functionally equivalent on small stores. The genuinely failing ones now: `UnparseableKeyFailsLoudly`. That is the TDD anchor; the List-count assertions become meaningful only with the new implementation (they pin the no-full-walk property against future regressions).

- [ ] **Step 5: Commit the tests**

```bash
git add internal/auditlog/reader_test.go
git commit -m "test(auditlog): day-walk Reader tests (partition walk, budget cursor, range narrowing)"
```

---

### Task 3: Reader date-partition walk — implementation

**Files:**
- Modify: `internal/auditlog/reader.go`

- [ ] **Step 1: Replace `listKeys` with the day-walk**

In `internal/auditlog/reader.go`:

(a) Add `"fmt"` and `"time"` to imports.

(b) Add the per-page day-list budget constant after the imports:

```go
// maxDayListsPerPage bounds how many day-partition List calls one Page()
// makes. A very sparse prefix (long empty gaps between partitions) stops at
// the budget and returns a synthetic day-boundary cursor so pagination always
// terminates; the next page resumes the walk.
const maxDayListsPerPage = 100
```

(c) Delete `listKeys` and its `TODO(v2)` comment entirely. Replace with:

```go
// dayLayout is the partition segment of shipped activity keys
// (<prefix><YYYY>/<MM>/<DD>/<HHMMSS>-<instance>-<seq>.ndjson.gz, written by
// internal/shiplog bucketKeyForPending).
const dayLayout = "2006/01/02"

// dayOf extracts the "YYYY/MM/DD" partition from a key under r.prefix.
func (r *Reader) dayOf(key string) (string, bool) {
	rest, ok := strings.CutPrefix(key, r.prefix)
	if !ok || len(rest) < len(dayLayout)+1 {
		return "", false
	}
	day := rest[:len(dayLayout)]
	if _, err := time.Parse(dayLayout, day); err != nil {
		return "", false
	}
	return day, true
}

// oldestDay probes the prefix with one MaxKeys=1 List. List returns keys in
// ascending lexicographic order (ObjectStore.List contract), so the first key
// is the oldest partition. Returns ("", nil) on an empty prefix and an error
// for a key that does not match the shiplog layout (junk under the log
// prefix must fail loudly, not be silently unreachable).
func (r *Reader) oldestDay(ctx context.Context) (string, error) {
	page, err := r.store.List(ctx, r.prefix, &storage.ListOptions{MaxKeys: 1})
	if err != nil {
		return "", err
	}
	if len(page.Objects) == 0 {
		return "", nil
	}
	day, ok := r.dayOf(page.Objects[0].Key)
	if !ok {
		return "", fmt.Errorf("auditlog: key %q under %q does not match the date-sharded activity layout", page.Objects[0].Key, r.prefix)
	}
	return day, nil
}

// listDay returns all keys under one day partition, descending (newest
// first), paging the store listing until exhausted.
func (r *Reader) listDay(ctx context.Context, day string) ([]string, error) {
	var keys []string
	token := ""
	for {
		page, err := r.store.List(ctx, r.prefix+day+"/", &storage.ListOptions{ContinuationToken: token})
		if err != nil {
			return nil, err
		}
		for _, o := range page.Objects {
			keys = append(keys, o.Key)
		}
		if page.NextToken == "" {
			break
		}
		token = page.NextToken
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	return keys, nil
}

// prevDay steps a "YYYY/MM/DD" partition back one calendar day.
func prevDay(day string) string {
	t, _ := time.Parse(dayLayout, day)
	return t.AddDate(0, 0, -1).Format(dayLayout)
}
```

(d) Rewrite `Page`. Keep the doc comment's cursor-metadata and best-effort-skip paragraphs; replace the body:

```go
func (r *Reader) Page(ctx context.Context, f Filter, cursor string) ([]Event, string, error) {
	floorDay, err := r.oldestDay(ctx)
	if err != nil {
		return nil, "", err
	}
	if floorDay == "" {
		return nil, "", nil
	}

	// Start at the cursor's partition (resume) or today; date filters
	// narrow both ends of the walk. Day strings compare lexicographically
	// = chronologically.
	startDay := time.Now().UTC().Format(dayLayout)
	if cursor != "" {
		if d, ok := r.dayOf(cursor); ok {
			startDay = d
		} else {
			return nil, "", fmt.Errorf("auditlog: cursor %q does not match the date-sharded activity layout", cursor)
		}
	}
	if !f.Until.IsZero() {
		if u := f.Until.UTC().Format(dayLayout); u < startDay {
			startDay = u
		}
	}
	if !f.Since.IsZero() {
		if s := f.Since.UTC().Format(dayLayout); s > floorDay {
			floorDay = s
		}
	}
	if startDay < floorDay {
		return nil, "", nil
	}

	perPage := r.ObjectsPerPage
	if perPage <= 0 {
		perPage = 20
	}

	var events []Event
	consumed := 0
	var bytesUsed int64
	oldestKey := ""     // oldest object consumed on this page
	dayLists := 0       // budget against maxDayListsPerPage
	day := startDay
	capped := false     // an object/byte/event cap fired
	lastOfFloor := false // cap fired on the floor day's oldest key -> nothing older

walk:
	for day >= floorDay {
		if dayLists >= maxDayListsPerPage {
			// Sparse prefix: terminate the page with a synthetic cursor.
			// day has NOT been listed yet, so the cursor must sort ABOVE
			// every key in day (resume includes it) and BELOW day+1:
			// "~" (0x7E) sorts above the key charset ([0-9a-z.-]).
			return r.finish(events, r.prefix+day+"/~")
		}
		keys, err := r.listDay(ctx, day)
		if err != nil {
			return nil, "", err
		}
		dayLists++
		for i, key := range keys {
			if cursor != "" && key >= cursor {
				continue // resume day: strictly-older only
			}
			obj, err := r.store.Get(ctx, key, nil)
			if err != nil {
				r.logSkip(key, "get", err)
				oldestKey = key
				consumed++
			} else {
				evs, skippedLines, decErr := DecodeGz(obj.Body)
				size := obj.Metadata.Size
				obj.Body.Close()
				if decErr != nil {
					r.logSkip(key, "decode", decErr)
					oldestKey = key
					consumed++
				} else {
					if skippedLines > 0 && r.Logger != nil {
						r.Logger.Warn("auditlog: skipped malformed lines in activity object",
							"key", key, "skipped_lines", skippedLines)
					}
					for _, e := range evs {
						if f.Match(e) {
							events = append(events, e)
						}
					}
					bytesUsed += size
					oldestKey = key
					consumed++
				}
			}
			if consumed >= perPage ||
				(r.MaxBytesPerPage > 0 && bytesUsed >= r.MaxBytesPerPage) ||
				(r.MaxEventsPerPage > 0 && len(events) >= r.MaxEventsPerPage) {
				capped = true
				// A cap on the floor day's last (oldest) key means the
				// walk is complete anyway -> empty next cursor, matching
				// the previous implementation's oldestIdx>0 rule.
				lastOfFloor = day == floorDay && i == len(keys)-1
				break walk
			}
		}
		if day == floorDay {
			break
		}
		day = prevDay(day)
	}

	next := ""
	if capped && !lastOfFloor && oldestKey != "" {
		next = oldestKey
	}
	return r.finish(events, next)
}

// finish applies the stable newest-first sort and returns the page.
func (r *Reader) finish(events []Event, next string) ([]Event, string, error) {
	sort.SliceStable(events, func(a, b int) bool {
		return events[a].Ts.After(events[b].Ts)
	})
	return events, next, nil
}
```

With `lastOfFloor`, a cap firing on the globally-oldest object returns `next == ""` exactly like the old `oldestIdx > 0` rule — `TestReaderPage_ByteCapBreaksPage`'s page-3 `next3 == ""` assertion pins this.

(e) Update the `Reader` struct's doc comment (top of file) — replace the "Pagination is object-cursor based" paragraph with:

```go
// Pagination is object-cursor based: the cursor is the storage key of the
// oldest object included on the previous page; the next page consumes objects
// strictly older than that key. Key discovery walks the date-sharded
// partition layout (<prefix>/YYYY/MM/DD/) backward from the cursor's day
// (or today) to a floor day learned from one MaxKeys=1 probe, so a page
// costs a handful of small partition listings instead of a full-prefix scan.
```

- [ ] **Step 2: Run the full auditlog suite**

Run: `go test ./internal/auditlog/ -v 2>&1 | tail -10`
Expected: PASS — all pre-existing tests (on the date-sharded fixtures) plus the five new ones. If `TestReaderPage_DeletedCursorResumesAtPosition` fails: the deleted-cursor key still parses to a day, the walk lists that day, and `key >= cursor` filtering handles the absence — re-check the resume filter, not the test.

- [ ] **Step 3: Run web tests (handler integration unchanged)**

Run: `go test ./internal/web/ 2>&1 | tail -2`
Expected: PASS — `fakeAuditReader` in web tests fakes the interface, so nothing changes; this guards accidental signature drift.

- [ ] **Step 4: Commit**

```bash
git add internal/auditlog/reader.go
git commit -m "feat(auditlog): replace full-prefix listing with backward date-partition walk"
```

---

### Task 4: `bucketvcs session list`

**Files:**
- Create: `cmd/bucketvcs/session.go`
- Create: `cmd/bucketvcs/session_test.go`

- [ ] **Step 1: Write the failing tests**

Create `cmd/bucketvcs/session_test.go`. Look at `cmd/bucketvcs/token_test.go` for the bootstrap helpers (`t.TempDir()` auth DB via `sqlitestore.Open`); sessions need a store with users + sessions seeded:

```go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

// seedSessionDB creates an auth DB with two users and three sessions
// (2x alice, 1x bob) and returns its path plus alice's raw session ids.
func seedSessionDB(t *testing.T) (dbPath string, aliceRaws []string) {
	t.Helper()
	dbPath = filepath.Join(t.TempDir(), "auth.db")
	s, err := sqlitestore.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	aliceID, err := s.CreateUser(ctx, "alice", false)
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	bobID, err := s.CreateUser(ctx, "bob", false)
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}
	for i := 0; i < 2; i++ {
		raw, err := s.CreateSession(ctx, aliceID, "password", time.Hour)
		if err != nil {
			t.Fatalf("alice session %d: %v", i, err)
		}
		aliceRaws = append(aliceRaws, raw)
	}
	if _, err := s.CreateSession(ctx, bobID, "oidc", time.Hour); err != nil {
		t.Fatalf("bob session: %v", err)
	}
	return dbPath, aliceRaws
}

func TestSessionList_NDJSON(t *testing.T) {
	db, _ := seedSessionDB(t)
	var out, errb bytes.Buffer
	code := runSession(context.Background(), []string{"list", "--auth-db", db}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3:\n%s", len(lines), out.String())
	}
	users := map[string]int{}
	for _, ln := range lines {
		var row map[string]any
		if err := json.Unmarshal([]byte(ln), &row); err != nil {
			t.Fatalf("bad NDJSON line %q: %v", ln, err)
		}
		for _, k := range []string{"id_hash", "user_id", "user", "provider", "created_at", "expires_at", "last_seen"} {
			if _, ok := row[k]; !ok {
				t.Fatalf("line missing %q: %s", k, ln)
			}
		}
		users[row["user"].(string)]++
	}
	if users["alice"] != 2 || users["bob"] != 1 {
		t.Fatalf("user counts = %v, want alice:2 bob:1", users)
	}
}

func TestSessionList_UserFilter(t *testing.T) {
	db, _ := seedSessionDB(t)
	var out, errb bytes.Buffer
	code := runSession(context.Background(), []string{"list", "--auth-db", db, "--user", "bob"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], `"user":"bob"`) {
		t.Fatalf("filtered output:\n%s", out.String())
	}
}

func TestSessionList_UsageErrors(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runSession(context.Background(), nil, &out, &errb); code != 2 {
		t.Fatalf("no subcommand: exit %d, want 2", code)
	}
	if code := runSession(context.Background(), []string{"list"}, &out, &errb); code != 2 {
		t.Fatalf("missing --auth-db: exit %d, want 2", code)
	}
}
```

- [ ] **Step 2: Verify they fail to compile**

Run: `go test ./cmd/bucketvcs/ -run TestSessionList 2>&1 | head -3`
Expected: FAIL — `undefined: runSession`.

- [ ] **Step 3: Implement `session list`**

Create `cmd/bucketvcs/session.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
)

func runSession(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: bucketvcs session <list|revoke>")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return sessionList(ctx, rest, stdout, stderr)
	case "revoke":
		return sessionRevoke(ctx, rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "session: unknown subcommand %q\n", sub)
		return 2
	}
}

// sessionList prints every web session as NDJSON, newest-first by last_seen
// (the store's ordering). The CLI is the escape hatch past the admin page's
// 500-row display cap.
func sessionList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("session list", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	user := fs.String("user", "", "only sessions belonging to this user name")
	fs.SetOutput(stderr)
	if err := fs.Parse(reorderFlagsFirst(args, nil)); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *authDB == "" {
		fmt.Fprintln(stderr, "usage: bucketvcs session list --auth-db=<path> [--user=<name>]")
		return 2
	}
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()
	rows, _, err := s.ListAllSessions(ctx, 0)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	enc := json.NewEncoder(stdout)
	for _, row := range rows {
		if *user != "" && row.UserName != *user {
			continue
		}
		_ = enc.Encode(map[string]any{
			"id_hash":    row.IDHash,
			"user_id":    row.UserID,
			"user":       row.UserName,
			"provider":   row.Provider,
			"created_at": row.CreatedAt,
			"expires_at": row.ExpiresAt,
			"last_seen":  row.LastSeen,
		})
	}
	return 0
}
```

(Add `"flag"` to the import block. `sessionRevoke` does not exist yet — add a stub so this task compiles, replaced in Task 5:)

```go
func sessionRevoke(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fmt.Fprintln(stderr, "session revoke: not implemented")
	return 2
}
```

- [ ] **Step 4: Run the list tests**

Run: `go test ./cmd/bucketvcs/ -run TestSessionList 2>&1 | tail -3`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/bucketvcs/session.go cmd/bucketvcs/session_test.go
git commit -m "feat(cli): bucketvcs session list (NDJSON, --user filter)"
```

---

### Task 5: `bucketvcs session revoke`

**Files:**
- Modify: `cmd/bucketvcs/session.go`
- Modify: `cmd/bucketvcs/session_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `cmd/bucketvcs/session_test.go` (`auth` import needed: `"github.com/bucketvcs/bucketvcs/internal/auth"`):

```go
func TestSessionRevoke_ByHash(t *testing.T) {
	db, aliceRaws := seedSessionDB(t)
	hash := auth.HashSessionID(aliceRaws[0])
	var out, errb bytes.Buffer
	code := runSession(context.Background(), []string{"revoke", "--auth-db", db, "--id-hash", hash}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "revoked=1") {
		t.Fatalf("stdout %q, want revoked=1", out.String())
	}
	// Idempotent: second revoke reports 0 and still exits 0.
	out.Reset()
	if code := runSession(context.Background(), []string{"revoke", "--auth-db", db, "--id-hash", hash}, &out, &errb); code != 0 {
		t.Fatalf("re-revoke exit %d", code)
	}
	if !strings.Contains(out.String(), "revoked=0") {
		t.Fatalf("stdout %q, want revoked=0", out.String())
	}
}

func TestSessionRevoke_ByUser(t *testing.T) {
	db, _ := seedSessionDB(t)
	var out, errb bytes.Buffer
	code := runSession(context.Background(), []string{"revoke", "--auth-db", db, "--user", "alice"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "revoked=2") {
		t.Fatalf("stdout %q, want revoked=2 (both alice sessions)", out.String())
	}
	// bob's session survives.
	out.Reset()
	if code := runSession(context.Background(), []string{"list", "--auth-db", db}, &out, &errb); code != 0 {
		t.Fatalf("list exit %d", code)
	}
	if got := strings.Count(out.String(), `"user":"bob"`); got != 1 {
		t.Fatalf("bob sessions after alice revoke = %d, want 1", got)
	}
	if strings.Contains(out.String(), `"user":"alice"`) {
		t.Fatal("alice sessions remain after revoke --user")
	}
}

func TestSessionRevoke_UsageErrors(t *testing.T) {
	db, aliceRaws := seedSessionDB(t)
	hash := auth.HashSessionID(aliceRaws[0])
	var out, errb bytes.Buffer
	// both flags
	if code := runSession(context.Background(), []string{"revoke", "--auth-db", db, "--id-hash", hash, "--user", "alice"}, &out, &errb); code != 2 {
		t.Fatalf("both flags: exit %d, want 2", code)
	}
	// neither flag
	if code := runSession(context.Background(), []string{"revoke", "--auth-db", db}, &out, &errb); code != 2 {
		t.Fatalf("neither flag: exit %d, want 2", code)
	}
	// unknown user
	if code := runSession(context.Background(), []string{"revoke", "--auth-db", db, "--user", "nobody"}, &out, &errb); code != 1 {
		t.Fatalf("unknown user: exit %d, want 1", code)
	}
}
```

- [ ] **Step 2: Verify the new tests fail**

Run: `go test ./cmd/bucketvcs/ -run TestSessionRevoke 2>&1 | tail -3`
Expected: FAIL — the stub exits 2 for every input (`ByHash`/`ByUser` fail).

- [ ] **Step 3: Replace the stub with the implementation**

In `cmd/bucketvcs/session.go`, replace `sessionRevoke`:

```go
// sessionRevoke deletes sessions by stored id hash or by owning user name.
// Idempotent: deleting an already-gone session prints revoked=0 and exits 0.
// The audit trail for CLI revocations is stderr-only (CLI emitters are not
// shipped; see the observability operator guide).
func sessionRevoke(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("session revoke", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	idHash := fs.String("id-hash", "", "stored session id hash (from session list / the admin page)")
	user := fs.String("user", "", "revoke ALL sessions of this user name")
	fs.SetOutput(stderr)
	if err := fs.Parse(reorderFlagsFirst(args, nil)); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *authDB == "" || (*idHash == "") == (*user == "") {
		fmt.Fprintln(stderr, "usage: bucketvcs session revoke --auth-db=<path> (--id-hash=<hex> | --user=<name>)")
		return 2
	}
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()

	var n int64
	if *idHash != "" {
		n, err = s.DeleteSessionByHash(ctx, *idHash)
	} else {
		u, uerr := s.GetUserByName(ctx, *user)
		if uerr != nil {
			fmt.Fprintf(stderr, "%v\n", uerr)
			return 1
		}
		n, err = s.DeleteSessionsForUser(ctx, u.ID, "")
	}
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	// stderr-only audit line (CLI events are not shipped).
	slog.Info("auth.session.admin_revoked",
		"audit", true, "event", "auth.session.admin_revoked",
		"actor", "cli", "id_hash", *idHash, "target_user", *user, "count", n)
	fmt.Fprintf(stdout, "revoked=%d\n", n)
	return 0
}
```

Add `"log/slog"` to the imports.

- [ ] **Step 4: Run all session CLI tests**

Run: `go test ./cmd/bucketvcs/ -run 'TestSession' 2>&1 | tail -3`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/bucketvcs/session.go cmd/bucketvcs/session_test.go
git commit -m "feat(cli): bucketvcs session revoke (--id-hash | --user, idempotent)"
```

---

### Task 6: Wire dispatch + help text

**Files:**
- Modify: `cmd/bucketvcs/main.go` (dispatch switch ~line 43; help usage ~line 96)

- [ ] **Step 1: Add the dispatch case**

In the subcommand switch (after `case "user":` at main.go:43):

```go
	case "session":
		return runSession(ctx, rest, stdout, stderr)
```

- [ ] **Step 2: Add the help line**

In the usage text (next to `token` at main.go:97):

```
  session            Manage web sessions (list/revoke)
```

- [ ] **Step 3: Build + spot-check + commit**

```bash
go build ./... && go run ./cmd/bucketvcs session 2>&1 | head -2
```
Expected: `usage: bucketvcs session <list|revoke>` and exit status 2.

```bash
git add cmd/bucketvcs/main.go
git commit -m "feat(cli): register session command group"
```

---

### Task 7: Smoke + docs

**Files:**
- Modify: `scripts/smoke-observability.sh` (Step 5 block, after the `/admin/sessions` assertions ~line 261)
- Modify: `internal/web/templates/admin_sessions.html` (truncation hint, line 7)
- Modify: `docs/operator-guides/web-ui.md` (§10.1 "query the auth DB" sentence, ~line 842)
- Modify: `docs/operator-guides/observability.md` (§6 CLI-emitted bullet — add session revoke; §4 index if it lists per-feature pointers)

- [ ] **Step 1: Smoke — assert the CLI sees the live sessions**

In `scripts/smoke-observability.sh`, after the `/admin/sessions lists user 'admin'` echo (~line 261), insert:

```bash
# ---- step 5b — sessions CLI agrees with the web view --------------------------
echo ""
echo "== Step 5b: bucketvcs session list (CLI) =="

cli_sessions=$("$BUCKETVCS" session list --auth-db "$AUTH_DB")
n_cli=$(printf '%s\n' "$cli_sessions" | grep -c '"user":"admin"' || true)
assert_eq "$n_cli" "1" "CLI lists exactly the one live admin session"
echo "  session list CLI OK"
```

(At this point in the smoke exactly one session is live: jar B was revoked in step 4. sqlite concurrent read while serve holds the DB is fine.)

- [ ] **Step 2: Template hint**

`internal/web/templates/admin_sessions.html` line 7:

```html
  {{if .Truncated}}<p class="hint">showing first {{len .Sessions}} of {{.Total}} sessions; use `bucketvcs session list` for the full list.</p>{{end}}
```

- [ ] **Step 3: Operator guides**

`docs/operator-guides/web-ui.md` §10.1 — replace the parenthetical that points at querying the auth DB:

```markdown
first 500 sessions; on a larger deployment use `bucketvcs session list`
(NDJSON; `session revoke --id-hash=<h>` / `--user=<name>` for CLI
revocation). Admin
```

`docs/operator-guides/observability.md` §6, the "CLI-emitted audit events are not shipped" bullet — add `session revoke` to the example list:

```markdown
- **CLI-emitted audit events are not shipped.** `gc.*`, `maintenance.*`,
  `lfs.gc.*`, `lfs.quota.reconcile`, `repo.renamed`, and `session revoke`
  reach stderr only,
```

- [ ] **Step 4: Run the smoke**

Run: `./scripts/smoke-observability.sh 2>&1 | tail -4`
Expected: `ALL OBSERVABILITY SMOKE CHECKS PASSED` including the new `session list CLI OK` line.

- [ ] **Step 5: Commit**

```bash
git add scripts/smoke-observability.sh internal/web/templates/admin_sessions.html docs/operator-guides/
git commit -m "feat(cli)+docs: session CLI in smoke, template hint, operator guides"
```

---

### Task 8: Final verification

- [ ] **Step 1: Full suite**

Run: `go test ./... 2>&1 | grep -v '^ok\|no test files'` — expected: empty output.

- [ ] **Step 2: Static checks**

Run: `go build ./... && go vet ./... && gofmt -l internal/auditlog internal/storage cmd/bucketvcs internal/web` — expected: no output after build (pre-existing `internal/buildtrigger` gofmt flags are NOT in these dirs and stay out of scope).

- [ ] **Step 3: Smoke**

Run: `./scripts/smoke-observability.sh 2>&1 | tail -2` — expected: ALL PASSED.

- [ ] **Step 4: Review + merge flow**

Run the roborev branch review loop (review → fix → re-review until findings are cosmetic), then PR + CI + squash merge, per project convention.
