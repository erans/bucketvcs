package auditlog_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"

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

// List returns ALL keys under prefix, ascending. ContinuationToken/MaxKeys are
// ignored (single page) — sufficient for these tests.
func (s *fakeStore) List(ctx context.Context, prefix string, opts *storage.ListOptions) (*storage.ListPage, error) {
	var keys []string
	for k := range s.objs {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	page := &storage.ListPage{}
	for _, k := range keys {
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
	store.put("sys/logs/activity/120000", gzLines(
		`{"ts":"2026-05-22T12:00:00Z","event":"a","tenant":"acme","repo":"app"}`,
	))
	store.put("sys/logs/activity/130000", gzLines(
		`{"ts":"2026-05-22T13:00:00Z","event":"b","tenant":"acme","repo":"app"}`,
	))
	store.put("sys/logs/activity/140000", gzLines(
		`{"ts":"2026-05-22T14:00:00Z","event":"c","tenant":"acme","repo":"app"}`,
	))

	r := auditlog.NewReader(store, "")
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
	store.put("sys/logs/activity/120000", gzLines(
		`{"ts":"2026-05-22T12:00:00Z","event":"acme-evt","tenant":"acme","repo":"app"}`,
		`{"ts":"2026-05-22T12:00:01Z","event":"other-evt","tenant":"other","repo":"app"}`,
	))

	r := auditlog.NewReader(store, "")
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
	r := auditlog.NewReader(store, "")
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
	store.put("sys/logs/activity/120000", gzLines(
		`{"ts":"2026-05-22T12:00:00Z","event":"ev-a","tenant":"acme","repo":"app"}`,
	))
	store.put("sys/logs/activity/130000", gzLines(
		`{"ts":"2026-05-22T13:00:00Z","event":"ev-b","tenant":"acme","repo":"app"}`,
	))
	store.put("sys/logs/activity/140000", gzLines(
		`{"ts":"2026-05-22T14:00:00Z","event":"ev-c","tenant":"acme","repo":"app"}`,
	))

	r := auditlog.NewReader(store, "")
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
	store.put("sys/logs/activity/120000", gzLines(
		`{"ts":"2026-05-22T12:00:00Z","event":"a","tenant":"acme","repo":"app"}`,
	))
	store.put("sys/logs/activity/140000", gzLines(
		`{"ts":"2026-05-22T14:00:00Z","event":"c","tenant":"acme","repo":"app"}`,
	))

	// Cursor names an object that no longer exists (retention swept 130000
	// between page views). Pagination must resume with the keys strictly older
	// than the cursor, not dead-end.
	r := auditlog.NewReader(store, "")
	events, next, err := r.Page(context.Background(), auditlog.Filter{}, "sys/logs/activity/130000")
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
	inner.put("sys/logs/activity/120000", gzLines(
		`{"ts":"2026-05-22T12:00:00Z","event":"a","tenant":"acme","repo":"app"}`,
	))
	inner.put("sys/logs/activity/130000", gzLines(
		`{"ts":"2026-05-22T13:00:00Z","event":"b","tenant":"acme","repo":"app"}`,
	))
	store := &brokenGetStore{fakeStore: inner, failKey: "sys/logs/activity/130000"}

	var buf bytes.Buffer
	r := auditlog.NewReader(store, "")
	r.Logger = slog.New(slog.NewTextHandler(&buf, nil))

	events, _, err := r.Page(context.Background(), auditlog.Filter{}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := eventNames(events); len(got) != 1 || got[0] != "a" {
		t.Fatalf("events: got %v want [a] (broken object skipped)", got)
	}
	out := buf.String()
	if !strings.Contains(out, "sys/logs/activity/130000") || !strings.Contains(out, "simulated get failure") {
		t.Fatalf("skipped object not logged with key+error; log:\n%s", out)
	}
}
