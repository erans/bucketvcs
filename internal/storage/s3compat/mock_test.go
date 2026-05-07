// mock_test.go contains the in-memory S3 substrate shared by all
// per-method test files in this package (get_test.go, put_test.go,
// delete_test.go, list_test.go, multipart_test.go). T8+ extend
// mockBackend.serve by inspecting r.Method, r.URL.Path, and
// r.URL.Query() in that order — the GET branch routes to object
// lookup today, but T10 (List) will refactor it to dispatch on
// query parameters (?list-type=2) before falling through. Keep
// dispatch logic linear and case-additive; do not interleave
// task-specific state.

package s3compat

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// mockBackend is a minimal in-memory S3 server. Each test uses one.
// The handler matches request method + URL path against a tiny dispatch
// table the test registers via Set/SetGet/etc. before exercising the
// adapter. It exists ONLY for unit tests; live conformance covers real
// S3/R2 behavior.
type mockBackend struct {
	t               *testing.T
	objects         map[string]mockObject // key (incl. bucket prefix) -> obj
	nextETag        uint64
	lastContentType string
}

type mockObject struct {
	body []byte
	etag string
}

func newMockBackend(t *testing.T) (*S3Compat, *mockBackend) {
	t.Helper()
	mb := &mockBackend{t: t, objects: map[string]mockObject{}}
	srv := httptest.NewServer(http.HandlerFunc(mb.serve))
	t.Cleanup(srv.Close)
	cfg := Config{
		Bucket:          "test-bucket",
		Region:          "us-east-1",
		Endpoint:        srv.URL,
		ForcePathStyle:  true,
		AccessKeyID:     "AKID",
		SecretAccessKey: "SECRET",
	}
	s, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s, mb
}

func (m *mockBackend) put(key string, body []byte, etag string) {
	m.objects[key] = mockObject{body: body, etag: etag}
}

func (m *mockBackend) keyFromPath(p string) string {
	// Path is "/<bucket>/<key>"; strip the leading "/<bucket>/".
	p = strings.TrimPrefix(p, "/")
	_, key, _ := strings.Cut(p, "/")
	return key
}

func (m *mockBackend) serve(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if r.URL.Query().Get("list-type") == "2" {
			m.serveList(w, r)
			return
		}
		m.serveGetOrHead(w, r)
	case http.MethodHead:
		m.serveGetOrHead(w, r)
	case http.MethodPut:
		m.servePut(w, r)
	case http.MethodDelete:
		m.serveDelete(w, r)
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func (m *mockBackend) serveGetOrHead(w http.ResponseWriter, r *http.Request) {
	key := m.keyFromPath(r.URL.Path)
	obj, ok := m.objects[key]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	// If-Match: reject if ETag doesn't match.
	if im := r.Header.Get("If-Match"); im != "" && obj.etag != im {
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}
	w.Header().Set("ETag", obj.etag)
	// Honor Range if present.
	if rng := r.Header.Get("Range"); rng != "" {
		start, end, ok := parseSimpleRange(rng, len(obj.body))
		if !ok {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		chunk := obj.body[start : end+1]
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(chunk)))
		w.WriteHeader(http.StatusPartialContent)
		if r.Method == http.MethodGet {
			_, _ = w.Write(chunk)
		}
		return
	}
	// Always set Content-Length so HEAD callers see the object size.
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(obj.body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(obj.body)
	}
}

func (m *mockBackend) serveList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prefix := q.Get("prefix")
	delimiter := q.Get("delimiter")
	startAfter := q.Get("continuation-token")

	var keys []string
	for k := range m.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	type entry struct {
		Key  string
		ETag string
		Size int
	}
	var contents []entry
	prefixes := map[string]struct{}{}

	for _, k := range keys {
		if startAfter != "" && k <= startAfter {
			continue
		}
		rem := strings.TrimPrefix(k, prefix)
		if delimiter != "" {
			if i := strings.Index(rem, delimiter); i >= 0 {
				prefixes[prefix+rem[:i+len(delimiter)]] = struct{}{}
				continue
			}
		}
		contents = append(contents, entry{Key: k, ETag: m.objects[k].etag, Size: len(m.objects[k].body)})
	}

	w.Header().Set("Content-Type", "application/xml")
	w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>`))
	w.Write([]byte(`<ListBucketResult><IsTruncated>false</IsTruncated>`))
	for _, c := range contents {
		w.Write([]byte(`<Contents><Key>` + c.Key + `</Key><ETag>` + c.ETag + `</ETag><Size>` + strconv.Itoa(c.Size) + `</Size></Contents>`))
	}
	for p := range prefixes {
		w.Write([]byte(`<CommonPrefixes><Prefix>` + p + `</Prefix></CommonPrefixes>`))
	}
	w.Write([]byte(`</ListBucketResult>`))
}

// servePut handles PUT with optional If-None-Match: * and If-Match: <etag>
// semantics that mirror the production adapter's CAS expectations.
func (m *mockBackend) servePut(w http.ResponseWriter, r *http.Request) {
	key := m.keyFromPath(r.URL.Path)
	body, _ := io.ReadAll(r.Body)
	existing, exists := m.objects[key]

	if r.Header.Get("If-None-Match") == "*" && exists {
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}
	if im := r.Header.Get("If-Match"); im != "" {
		if !exists || existing.etag != im {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
	}

	m.nextETag++
	etag := fmt.Sprintf(`"v%d"`, m.nextETag)
	m.lastContentType = r.Header.Get("Content-Type")
	m.objects[key] = mockObject{body: body, etag: etag}
	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
}

// serveDelete handles DELETE with optional If-Match ETag check.
// It mirrors real S3 idempotent-delete behavior: absent keys return 204
// even when If-Match is set, because real S3 doesn't honor If-Match on
// absent keys for DELETE. The adapter must Head-verify before deleting.
func (m *mockBackend) serveDelete(w http.ResponseWriter, r *http.Request) {
	key := m.keyFromPath(r.URL.Path)
	existing, exists := m.objects[key]

	// Real S3 quirk: DELETE is idempotent. Absent keys return 204
	// even when If-Match is set. The adapter must Head-verify first.
	if !exists {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Race: object exists but was rewritten with different ETag.
	if im := r.Header.Get("If-Match"); im != "" && existing.etag != im {
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}

	delete(m.objects, key)
	w.WriteHeader(http.StatusNoContent)
}

// parseSimpleRange handles "bytes=start-end" only.
func parseSimpleRange(h string, size int) (start, end int, ok bool) {
	const prefix = "bytes="
	if !strings.HasPrefix(h, prefix) {
		return 0, 0, false
	}
	parts := strings.SplitN(h[len(prefix):], "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	s, err := strconv.Atoi(parts[0])
	if err != nil || s < 0 {
		return 0, 0, false
	}
	e, err := strconv.Atoi(parts[1])
	if err != nil || e < 0 {
		return 0, 0, false
	}
	if e >= size {
		e = size - 1
	}
	if s < 0 || s > e {
		return 0, 0, false
	}
	return s, e, true
}
