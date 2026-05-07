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
	t       *testing.T
	objects map[string]mockObject // key (incl. bucket prefix) -> obj
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
	case http.MethodPut:
		m.servePut(w, r)
	case http.MethodGet, http.MethodHead:
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
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
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

	etag := `"v` + strconv.Itoa(len(m.objects)+1) + `"`
	m.objects[key] = mockObject{body: body, etag: etag}
	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
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
