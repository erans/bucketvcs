package auditlog_test

import (
	"bytes"
	"context"
	"io"
	"sort"
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

func sortStrings(ss []string) []string {
	out := append([]string(nil), ss...)
	sort.Strings(out)
	return out
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
	_ = sortStrings // keep helper referenced
}
