package auditlog_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auditlog"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// fakeStore is an in-memory ObjectStore slice (List+Get) for Reader tests.
type fakeStore struct {
	objs map[string][]byte
}

func newFakeStore() *fakeStore {
	return &fakeStore{objs: map[string][]byte{}}
}

func (s *fakeStore) put(key string, body []byte) {
	s.objs[key] = body
}

// fixedNow pins the reader's clock so fixture dates never age out of the
// walk-from-today window (the per-page day-list budget would otherwise make
// these tests fail when the calendar advances past fixtureDay+100).
func fixedNow(r *auditlog.Reader) *auditlog.Reader {
	auditlog.SetNow(r, func() time.Time {
		return time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	})
	return r
}

// List returns keys under prefix ascending, honoring MaxKeys (default 1000)
// and ContinuationToken (resume strictly after the token key) so the
// Reader's pagination loops are exercised. Page size 2 in tests via MaxKeys.
func (s *fakeStore) List(ctx context.Context, prefix string, opts *storage.ListOptions) (*storage.ListPage, error) {
	var keys []string
	for k := range s.objs {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	maxKeys := 1000
	token := ""
	if opts != nil {
		if opts.MaxKeys > 0 {
			maxKeys = opts.MaxKeys
		}
		token = opts.ContinuationToken
	}
	if token != "" {
		i := sort.SearchStrings(keys, token)
		if i < len(keys) && keys[i] == token {
			i++
		}
		keys = keys[i:]
	}
	page := &storage.ListPage{}
	for _, k := range keys {
		if len(page.Objects) == maxKeys {
			page.NextToken = page.Objects[len(page.Objects)-1].Key
			break
		}
		page.Objects = append(page.Objects, storage.ObjectMetadata{
			Key:  k,
			Size: int64(len(s.objs[k])),
		})
	}
	return page, nil
}

func (s *fakeStore) Get(ctx context.Context, key string, opts *storage.GetOptions) (*storage.Object, error) {
	body, ok := s.objs[key]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return &storage.Object{
		Body: io.NopCloser(bytes.NewReader(body)),
		Metadata: storage.ObjectMetadata{
			Key:  key,
			Size: int64(len(body)),
		},
	}, nil
}

func eventNames(events []auditlog.Event) []string {
	var names []string
	for _, e := range events {
		names = append(names, e.Event)
	}
	return names
}

func TestReaderPage_NewestFirstAndCursor(t *testing.T) {
	store := newFakeStore()
	// Three objects, keyed so ascending sort = oldest..newest.
	store.put("sys/logs/activity/2026/05/22/120000-aa-000001.ndjson.gz", gzLines(
		`{"ts":"2026-05-22T12:00:00Z","event":"a","tenant":"acme","repo":"app"}`,
	))
	store.put("sys/logs/activity/2026/05/22/130000-aa-000002.ndjson.gz", gzLines(
		`{"ts":"2026-05-22T13:00:00Z","event":"b","tenant":"acme","repo":"app"}`,
	))
	store.put("sys/logs/activity/2026/05/22/140000-aa-000003.ndjson.gz", gzLines(
		`{"ts":"2026-05-22T14:00:00Z","event":"c","tenant":"acme","repo":"app"}`,
	))

	r := fixedNow(auditlog.NewReader(store, ""))
	r.ObjectsPerPage = 2

	// Page 1: newest two objects (140000, 130000), events newest-first: c, b.
	events, next, err := r.Page(context.Background(), auditlog.Filter{}, "")
	if err != nil {
		t.Fatalf("page1: unexpected error: %v", err)
	}
	if got := eventNames(events); len(got) != 2 || got[0] != "c" || got[1] != "b" {
		t.Fatalf("page1 events: got %v want [c b]", got)
	}
	if next == "" {
		t.Fatalf("page1: expected non-empty next cursor")
	}

	// Page 2: with the cursor, the oldest object (a), and empty next cursor.
	events2, next2, err := r.Page(context.Background(), auditlog.Filter{}, next)
	if err != nil {
		t.Fatalf("page2: unexpected error: %v", err)
	}
	if got := eventNames(events2); len(got) != 1 || got[0] != "a" {
		t.Fatalf("page2 events: got %v want [a]", got)
	}
	if next2 != "" {
		t.Fatalf("page2: expected empty next cursor, got %q", next2)
	}
}

func TestReaderPage_CrossTenantFilterExcludes(t *testing.T) {
	store := newFakeStore()
	// One object with two lines: tenant acme + tenant other.
	store.put("sys/logs/activity/2026/05/22/120000-aa-000001.ndjson.gz", gzLines(
		`{"ts":"2026-05-22T12:00:00Z","event":"acme-evt","tenant":"acme","repo":"app"}`,
		`{"ts":"2026-05-22T12:00:01Z","event":"other-evt","tenant":"other","repo":"app"}`,
	))

	r := fixedNow(auditlog.NewReader(store, ""))
	events, _, err := r.Page(context.Background(), auditlog.Filter{Tenant: "acme", Repo: "app"}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := eventNames(events); len(got) != 1 || got[0] != "acme-evt" {
		t.Fatalf("cross-tenant filter: got %v want [acme-evt]", got)
	}
}

func TestReaderPage_EmptyStore(t *testing.T) {
	store := newFakeStore()
	r := fixedNow(auditlog.NewReader(store, ""))
	events, next, err := r.Page(context.Background(), auditlog.Filter{}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events, got %d", len(events))
	}
	if next != "" {
		t.Fatalf("expected empty cursor, got %q", next)
	}
}

// TestReaderPage_ByteCapBreaksPage verifies that MaxBytesPerPage causes the
// page loop to break after consuming one object and that the cursor advances
// correctly so all objects are reachable across successive pages with no
// events lost or duplicated.
//
// MaxBytesPerPage=1 guarantees a break after the first object on every page
// (gzip output is always >= 1 byte), isolating the byte-cap path from the
// ObjectsPerPage path.
func TestReaderPage_ByteCapBreaksPage(t *testing.T) {
	store := newFakeStore()
	// Three objects with distinct keys (ascending = oldest..newest).
	store.put("sys/logs/activity/2026/05/22/120000-aa-000001.ndjson.gz", gzLines(
		`{"ts":"2026-05-22T12:00:00Z","event":"ev-a","tenant":"acme","repo":"app"}`,
	))
	store.put("sys/logs/activity/2026/05/22/130000-aa-000002.ndjson.gz", gzLines(
		`{"ts":"2026-05-22T13:00:00Z","event":"ev-b","tenant":"acme","repo":"app"}`,
	))
	store.put("sys/logs/activity/2026/05/22/140000-aa-000003.ndjson.gz", gzLines(
		`{"ts":"2026-05-22T14:00:00Z","event":"ev-c","tenant":"acme","repo":"app"}`,
	))

	r := fixedNow(auditlog.NewReader(store, ""))
	r.ObjectsPerPage = 10 // high — object cap must not trigger
	r.MaxBytesPerPage = 1 // byte cap triggers after every single object

	// Page 1: newest object (140000) → event ev-c; cursor non-empty.
	evs1, next1, err := r.Page(context.Background(), auditlog.Filter{}, "")
	if err != nil {
		t.Fatalf("page1: unexpected error: %v", err)
	}
	if got := eventNames(evs1); len(got) != 1 || got[0] != "ev-c" {
		t.Fatalf("page1 events: got %v, want [ev-c]", got)
	}
	if next1 == "" {
		t.Fatalf("page1: expected non-empty next cursor (byte cap should have broken early)")
	}

	// Page 2: next object (130000) → event ev-b; cursor non-empty.
	evs2, next2, err := r.Page(context.Background(), auditlog.Filter{}, next1)
	if err != nil {
		t.Fatalf("page2: unexpected error: %v", err)
	}
	if got := eventNames(evs2); len(got) != 1 || got[0] != "ev-b" {
		t.Fatalf("page2 events: got %v, want [ev-b]", got)
	}
	if next2 == "" {
		t.Fatalf("page2: expected non-empty next cursor")
	}

	// Page 3: oldest object (120000) → event ev-a; cursor empty (no older objects).
	evs3, next3, err := r.Page(context.Background(), auditlog.Filter{}, next2)
	if err != nil {
		t.Fatalf("page3: unexpected error: %v", err)
	}
	if got := eventNames(evs3); len(got) != 1 || got[0] != "ev-a" {
		t.Fatalf("page3 events: got %v, want [ev-a]", got)
	}
	if next3 != "" {
		t.Fatalf("page3: expected empty next cursor, got %q", next3)
	}

	// Confirm total coverage: ev-c, ev-b, ev-a — no duplicates, no losses.
	var all []string
	all = append(all, eventNames(evs1)...)
	all = append(all, eventNames(evs2)...)
	all = append(all, eventNames(evs3)...)
	if len(all) != 3 || all[0] != "ev-c" || all[1] != "ev-b" || all[2] != "ev-a" {
		t.Fatalf("full walk: got %v, want [ev-c ev-b ev-a]", all)
	}
}

func TestReaderPage_DeletedCursorResumesAtPosition(t *testing.T) {
	store := newFakeStore()
	store.put("sys/logs/activity/2026/05/22/120000-aa-000001.ndjson.gz", gzLines(
		`{"ts":"2026-05-22T12:00:00Z","event":"a","tenant":"acme","repo":"app"}`,
	))
	store.put("sys/logs/activity/2026/05/22/140000-aa-000003.ndjson.gz", gzLines(
		`{"ts":"2026-05-22T14:00:00Z","event":"c","tenant":"acme","repo":"app"}`,
	))

	// Cursor names an object that no longer exists (retention swept 130000
	// between page views). Pagination must resume with the keys strictly older
	// than the cursor, not dead-end.
	r := fixedNow(auditlog.NewReader(store, ""))
	events, next, err := r.Page(context.Background(), auditlog.Filter{}, "sys/logs/activity/2026/05/22/130000-aa-000002.ndjson.gz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := eventNames(events); len(got) != 1 || got[0] != "a" {
		t.Fatalf("deleted cursor: got %v want [a]", got)
	}
	if next != "" {
		t.Fatalf("expected empty next cursor, got %q", next)
	}
}

// brokenGetStore wraps fakeStore so one key's Get fails: skipped objects must be
// logged (not silently dropped) when a Logger is configured.
type brokenGetStore struct {
	*fakeStore
	failKey string
}

func (s *brokenGetStore) Get(ctx context.Context, key string, opts *storage.GetOptions) (*storage.Object, error) {
	if key == s.failKey {
		return nil, errors.New("simulated get failure")
	}
	return s.fakeStore.Get(ctx, key, opts)
}

func TestReaderPage_SkippedObjectIsLogged(t *testing.T) {
	inner := newFakeStore()
	inner.put("sys/logs/activity/2026/05/22/120000-aa-000001.ndjson.gz", gzLines(
		`{"ts":"2026-05-22T12:00:00Z","event":"a","tenant":"acme","repo":"app"}`,
	))
	inner.put("sys/logs/activity/2026/05/22/130000-aa-000002.ndjson.gz", gzLines(
		`{"ts":"2026-05-22T13:00:00Z","event":"b","tenant":"acme","repo":"app"}`,
	))
	store := &brokenGetStore{fakeStore: inner, failKey: "sys/logs/activity/2026/05/22/130000-aa-000002.ndjson.gz"}

	var buf bytes.Buffer
	r := fixedNow(auditlog.NewReader(store, ""))
	r.Logger = slog.New(slog.NewTextHandler(&buf, nil))

	events, _, err := r.Page(context.Background(), auditlog.Filter{}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := eventNames(events); len(got) != 1 || got[0] != "a" {
		t.Fatalf("events: got %v want [a] (broken object skipped)", got)
	}
	out := buf.String()
	if !strings.Contains(out, "sys/logs/activity/2026/05/22/130000-aa-000002.ndjson.gz") || !strings.Contains(out, "simulated get failure") {
		t.Fatalf("skipped object not logged with key+error; log:\n%s", out)
	}
}

func TestReaderPage_ZeroObjectsPerPageDefaults(t *testing.T) {
	store := newFakeStore()
	store.put("sys/logs/activity/2026/05/22/120000-aa-000001.ndjson.gz", gzLines(
		`{"ts":"2026-05-22T12:00:00Z","event":"a","tenant":"acme","repo":"app"}`,
	))

	// A directly-constructed Reader (zero ObjectsPerPage) must not silently
	// return an empty page — the guard falls back to the default page size.
	r := &auditlog.Reader{}
	*r = *auditlog.NewReader(store, "")
	fixedNow(r)
	r.ObjectsPerPage = 0

	events, _, err := r.Page(context.Background(), auditlog.Filter{}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := eventNames(events); len(got) != 1 || got[0] != "a" {
		t.Fatalf("zero ObjectsPerPage: got %v want [a]", got)
	}
}

// TestReaderPage_EventCapBreaksPage: once MaxEventsPerPage matched events have
// accumulated, the page stops consuming further objects; the cursor still
// advances so all events remain reachable across successive pages.
func TestReaderPage_EventCapBreaksPage(t *testing.T) {
	store := newFakeStore()
	store.put("sys/logs/activity/2026/05/22/120000-aa-000001.ndjson.gz", gzLines(
		`{"ts":"2026-05-22T12:00:00Z","event":"ev-a","tenant":"acme","repo":"app"}`,
	))
	store.put("sys/logs/activity/2026/05/22/130000-aa-000002.ndjson.gz", gzLines(
		`{"ts":"2026-05-22T13:00:00Z","event":"ev-b","tenant":"acme","repo":"app"}`,
	))

	r := fixedNow(auditlog.NewReader(store, ""))
	r.MaxEventsPerPage = 1

	evs1, next1, err := r.Page(context.Background(), auditlog.Filter{}, "")
	if err != nil {
		t.Fatalf("page1: unexpected error: %v", err)
	}
	if got := eventNames(evs1); len(got) != 1 || got[0] != "ev-b" {
		t.Fatalf("page1 events: got %v, want [ev-b]", got)
	}
	if next1 == "" {
		t.Fatal("page1: expected non-empty next cursor (event cap should have broken early)")
	}

	evs2, next2, err := r.Page(context.Background(), auditlog.Filter{}, next1)
	if err != nil {
		t.Fatalf("page2: unexpected error: %v", err)
	}
	if got := eventNames(evs2); len(got) != 1 || got[0] != "ev-a" {
		t.Fatalf("page2 events: got %v, want [ev-a]", got)
	}
	if next2 != "" {
		t.Fatalf("page2: expected empty next cursor, got %q", next2)
	}
}

// TestReaderPage_PartialCorruptionLogged: an object that decodes but drops
// malformed lines must produce an operator signal, same as whole-object skips.
func TestReaderPage_PartialCorruptionLogged(t *testing.T) {
	store := newFakeStore()
	store.put("sys/logs/activity/2026/05/22/120000-aa-000001.ndjson.gz", gzLines(
		`{"ts":"2026-05-22T12:00:00Z","event":"good","tenant":"acme","repo":"app"}`,
		`not-valid-json{{`,
	))

	var buf bytes.Buffer
	r := fixedNow(auditlog.NewReader(store, ""))
	r.Logger = slog.New(slog.NewTextHandler(&buf, nil))

	events, _, err := r.Page(context.Background(), auditlog.Filter{}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := eventNames(events); len(got) != 1 || got[0] != "good" {
		t.Fatalf("events: got %v want [good]", got)
	}
	out := buf.String()
	if !strings.Contains(out, "sys/logs/activity/2026/05/22/120000-aa-000001.ndjson.gz") || !strings.Contains(out, "skipped_lines=1") {
		t.Fatalf("partial corruption not logged with key+count; log:\n%s", out)
	}
}

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

	r := fixedNow(auditlog.NewReader(store, ""))
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

	r := fixedNow(auditlog.NewReader(store, ""))
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
	r := fixedNow(auditlog.NewReader(store, ""))
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
	// The walk lists [since.day, until.day+1]: the until bound is padded one
	// day because partition keys carry SHIP time and an event late on the
	// until day can ship after UTC midnight into the next partition. Only
	// days outside the padded range must stay unlisted:
	// 2026/06/01 (below since) and 2026/06/06 (above until+1).
	for _, p := range store.listPrefixes {
		if strings.Contains(p, "2026/06/06") || strings.Contains(p, "2026/06/01") {
			t.Fatalf("walk listed out-of-range day prefix %q", p)
		}
	}
}

// TestReaderPage_UntilPadCatchesLateShippedEvents: an event timestamped on the
// until day but shipped after midnight (next-day partition) must still appear.
func TestReaderPage_UntilPadCatchesLateShippedEvents(t *testing.T) {
	store := newFakeStore()
	// Event at 23:50 on 06/04, shipped 00:05 on 06/05.
	store.put(dayKey("2026/06/05", "000500", 1), gzLines(
		`{"ts":"2026-06-04T23:50:00Z","event":"late","tenant":"acme","repo":"app"}`,
	))
	r := fixedNow(auditlog.NewReader(store, ""))
	f := auditlog.Filter{Until: time.Date(2026, 6, 4, 23, 59, 59, 0, time.UTC)}
	evs, _, err := r.Page(context.Background(), f, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := eventNames(evs); len(got) != 1 || got[0] != "late" {
		t.Fatalf("got %v want [late] (until+1 partition pad)", got)
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

	r := fixedNow(auditlog.NewReader(store, ""))
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

// TestReaderPage_SyntheticCursorIncludesItsDay: when the day budget fires, the
// synthetic cursor must resume INCLUDING the not-yet-listed day it points at —
// an object on that exact day must appear on the next page.
func TestReaderPage_SyntheticCursorIncludesItsDay(t *testing.T) {
	store := newFakeStore()
	// Newest object pins startDay; the second object sits exactly 100 days
	// (the per-page day-list budget) below it, i.e. on the first day the
	// budget prevents page 1 from listing.
	store.put(dayKey("2026/06/05", "120000", 2), gzLines(
		`{"ts":"2026-06-05T12:00:00Z","event":"new","tenant":"acme","repo":"app"}`,
	))
	store.put(dayKey("2026/02/25", "120000", 1), gzLines(
		`{"ts":"2026-02-25T12:00:00Z","event":"boundary","tenant":"acme","repo":"app"}`,
	))
	r := fixedNow(auditlog.NewReader(store, ""))

	// fixedNow pins today=2026/06/11; page 1 walks 06/11 down 100 day-lists
	// (through 2026/03/04) and stops with a synthetic cursor at 2026/03/03.
	evs, next, err := r.Page(context.Background(), auditlog.Filter{}, "")
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if got := eventNames(evs); len(got) != 1 || got[0] != "new" {
		t.Fatalf("page1: got %v want [new]", got)
	}
	if next == "" {
		t.Fatal("page1: want synthetic cursor")
	}

	// Page 2 walks 03/03 down; 2026/02/25 is within its 100-day budget, so
	// the boundary object must appear (and the floor is reached -> "").
	evs2, next2, err := r.Page(context.Background(), auditlog.Filter{}, next)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if got := eventNames(evs2); len(got) != 1 || got[0] != "boundary" {
		t.Fatalf("page2: got %v want [boundary] (synthetic cursor must include its day)", got)
	}
	if next2 != "" {
		t.Fatalf("page2: want empty cursor, got %q", next2)
	}
}

func TestReaderPage_UnparseableKeyFailsLoudly(t *testing.T) {
	inner := newFakeStore()
	inner.put("sys/logs/activity/garbage.txt", []byte("junk"))
	r := fixedNow(auditlog.NewReader(inner, ""))
	_, _, err := r.Page(context.Background(), auditlog.Filter{}, "")
	if err == nil {
		t.Fatal("want error for non-date-sharded key under the activity prefix")
	}

	// A key whose first 10 chars parse as a day but lack the '/' separator
	// (2026/06/05x.gz) would never be listed by listDay; dayOf must reject it
	// so the oldest-day probe fails loudly instead of going silently invisible.
	inner2 := newFakeStore()
	inner2.put("sys/logs/activity/2026/06/05x.gz", []byte("junk"))
	r2 := fixedNow(auditlog.NewReader(inner2, ""))
	_, _, err = r2.Page(context.Background(), auditlog.Filter{}, "")
	if err == nil {
		t.Fatal("want error for day-like key missing the '/' separator")
	}
}

// TestReaderPage_DayListPaginates: a day partition larger than one List page
// is fully consumed (listDay follows ContinuationToken).
func TestReaderPage_DayListPaginates(t *testing.T) {
	store := newFakeStore()
	for i := 1; i <= 5; i++ {
		store.put(dayKey("2026/06/05", fmt.Sprintf("1200%02d", i), i), gzLines(
			fmt.Sprintf(`{"ts":"2026-06-05T12:00:%02dZ","event":"e%d","tenant":"acme","repo":"app"}`, i, i),
		))
	}
	pagingStore := &maxKeysCappedStore{fakeStore: store, cap: 2}
	r := fixedNow(auditlog.NewReader(pagingStore, ""))
	evs, next, err := r.Page(context.Background(), auditlog.Filter{}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(evs) != 5 {
		t.Fatalf("got %d events, want 5 (listDay must follow ContinuationToken)", len(evs))
	}
	if next != "" {
		t.Fatalf("want empty cursor, got %q", next)
	}
}

// maxKeysCappedStore forces small List pages regardless of the caller's
// MaxKeys, exercising listDay's ContinuationToken loop.
type maxKeysCappedStore struct {
	*fakeStore
	cap int
}

func (s *maxKeysCappedStore) List(ctx context.Context, prefix string, opts *storage.ListOptions) (*storage.ListPage, error) {
	o := storage.ListOptions{}
	if opts != nil {
		o = *opts
	}
	if o.MaxKeys <= 0 || o.MaxKeys > s.cap {
		o.MaxKeys = s.cap
	}
	return s.fakeStore.List(ctx, prefix, &o)
}

// TestReaderPage_BadCursorSentinel: a malformed cursor must surface as
// ErrBadCursor through the real Page (the web handlers map it to 400; a
// regression to a plain error would silently turn those back into 500s).
func TestReaderPage_BadCursorSentinel(t *testing.T) {
	store := newFakeStore()
	store.put(dayKey("2026/06/05", "120000", 1), gzLines(
		`{"ts":"2026-06-05T12:00:00Z","event":"a","tenant":"acme","repo":"app"}`,
	))
	r := fixedNow(auditlog.NewReader(store, ""))
	_, _, err := r.Page(context.Background(), auditlog.Filter{}, "garbage")
	if !errors.Is(err, auditlog.ErrBadCursor) {
		t.Fatalf("err = %v, want ErrBadCursor", err)
	}
}
