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

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type mockUpload struct {
	id    string
	key   string
	parts map[int][]byte
}

// mockBackend is a minimal in-memory S3 server. Each test uses one.
// The handler matches request method + URL path against a tiny dispatch
// table the test registers via Set/SetGet/etc. before exercising the
// adapter. It exists ONLY for unit tests; live conformance covers real
// S3/R2 behavior.
type mockBackend struct {
	t               *testing.T
	objects         map[string]mockObject  // key (incl. bucket prefix) -> obj
	uploads         map[string]*mockUpload // upload-id -> upload
	nextETag        uint64
	lastContentType string
}

type mockObject struct {
	body []byte
	etag string
}

func newMockBackend(t *testing.T) (*S3Compat, *mockBackend) {
	t.Helper()
	mb := &mockBackend{t: t, objects: map[string]mockObject{}, uploads: map[string]*mockUpload{}}
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

func newMockBackendWithPrefix(t *testing.T, prefix string) (*S3Compat, *mockBackend, *httptest.Server) {
	t.Helper()
	mb := &mockBackend{t: t, objects: map[string]mockObject{}, uploads: map[string]*mockUpload{}}
	srv := httptest.NewServer(http.HandlerFunc(mb.serve))
	t.Cleanup(srv.Close)
	cfg := Config{
		Bucket:          "test-bucket",
		Prefix:          prefix,
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
	return s, mb, srv
}

// newMockBackendStrictChecksum returns an S3Compat configured with
// ResponseChecksumValidation=WhenRequired so the AWS SDK does NOT
// inject x-amz-checksum-mode=ENABLED into every GetObject presign by
// default. With that off, signed-URL tests can prove that the adapter's
// own ExpectedHash branch (signed.go) is the sole source of the
// checksum-mode header. Bypasses Open so we can thread the load option;
// Open's public signature does not accept extra awsconfig.LoadOptions.
func newMockBackendStrictChecksum(t *testing.T) (*S3Compat, *mockBackend) {
	t.Helper()
	mb := &mockBackend{t: t, objects: map[string]mockObject{}, uploads: map[string]*mockUpload{}}
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
	cfg.applyDefaults()
	loadOpts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.AccessKeyID, cfg.SecretAccessKey, cfg.SessionToken,
		)),
		awsconfig.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired),
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), loadOpts...)
	if err != nil {
		t.Fatalf("LoadDefaultConfig: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	awsCfg.Retryer = func() aws.Retryer { return newRetryer(cfg.MaxRetries) }
	clientOpts := []func(*s3.Options){
		func(o *s3.Options) { o.UsePathStyle = cfg.ForcePathStyle },
		func(o *s3.Options) { o.BaseEndpoint = aws.String(cfg.Endpoint) },
	}
	client := s3.NewFromConfig(awsCfg, clientOpts...)
	return &S3Compat{
		cfg:     cfg,
		client:  client,
		presign: s3.NewPresignClient(client),
	}, mb
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
	if m.serveMultipart(w, r) {
		return
	}
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

// serveMultipart returns true if it handled the request, false if it
// did not match any multipart pattern (caller should fall through).
func (m *mockBackend) serveMultipart(w http.ResponseWriter, r *http.Request) bool {
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		// CreateMultipartUpload
		m.nextETag++
		id := fmt.Sprintf("upload-%d", m.nextETag)
		key := m.keyFromPath(r.URL.Path)
		m.uploads[id] = &mockUpload{id: id, key: key, parts: map[int][]byte{}}
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, `<?xml version="1.0"?><InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, id)
		return true

	case r.Method == http.MethodPut && q.Has("uploadId") && q.Has("partNumber"):
		// UploadPart
		id := q.Get("uploadId")
		up, ok := m.uploads[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		pnStr := q.Get("partNumber")
		pn, _ := strconv.Atoi(pnStr)
		body, _ := io.ReadAll(r.Body)
		up.parts[pn] = body
		m.nextETag++
		etag := fmt.Sprintf(`"p%s-%d"`, pnStr, m.nextETag)
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusOK)
		return true

	case r.Method == http.MethodPost && q.Has("uploadId"):
		// CompleteMultipartUpload
		id := q.Get("uploadId")
		up, ok := m.uploads[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		// Honor If-None-Match: *
		if r.Header.Get("If-None-Match") == "*" {
			if _, exists := m.objects[up.key]; exists {
				w.WriteHeader(http.StatusPreconditionFailed)
				return true
			}
		}
		// NOTE: This mock ignores the CompleteMultipartUpload XML body and
		// assembles all uploaded parts in part-number order. That hides the
		// case where production code submits the wrong CompletedPart list to
		// S3. We accept this divergence because (a) validateCompletePartsLocked
		// already enforces the part contract locally before any SDK call, and
		// (b) live conformance at T13 against real S3/R2 exercises the full
		// XML round-trip.
		// Reassemble parts in part-number order.
		var pns []int
		for pn := range up.parts {
			pns = append(pns, pn)
		}
		sort.Ints(pns)
		var buf []byte
		for _, pn := range pns {
			buf = append(buf, up.parts[pn]...)
		}
		m.nextETag++
		etag := fmt.Sprintf(`"complete-%d"`, m.nextETag)
		m.objects[up.key] = mockObject{body: buf, etag: etag}
		delete(m.uploads, id)
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, `<?xml version="1.0"?><CompleteMultipartUploadResult><ETag>%s</ETag></CompleteMultipartUploadResult>`, etag)
		return true

	case r.Method == http.MethodDelete && q.Has("uploadId"):
		// AbortMultipartUpload
		id := q.Get("uploadId")
		if _, ok := m.uploads[id]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		delete(m.uploads, id)
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
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
	// continuation-token is the token returned by the previous page's
	// NextContinuationToken. The mock uses it as a "start-after" value:
	// keys whose sort order is <= the token are skipped, which matches
	// how real S3 implements opaque continuation tokens for this mock.
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

	// Honor max-keys: truncate contents and emit NextContinuationToken.
	maxKeys := 0
	if mk := q.Get("max-keys"); mk != "" {
		if n, err := strconv.Atoi(mk); err == nil && n > 0 {
			maxKeys = n
		}
	}
	isTruncated := false
	nextToken := ""
	if maxKeys > 0 && len(contents) > maxKeys {
		nextToken = contents[maxKeys-1].Key
		contents = contents[:maxKeys]
		isTruncated = true
	}

	w.Header().Set("Content-Type", "application/xml")
	w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>`))
	w.Write([]byte(`<ListBucketResult>`))
	fmt.Fprintf(w, `<IsTruncated>%v</IsTruncated>`, isTruncated)
	if nextToken != "" {
		fmt.Fprintf(w, `<NextContinuationToken>%s</NextContinuationToken>`, nextToken)
	}
	for _, c := range contents {
		fmt.Fprintf(w, `<Contents><Key>%s</Key><ETag>%s</ETag><Size>%d</Size><LastModified>2026-05-07T00:00:00.000Z</LastModified></Contents>`,
			c.Key, c.ETag, c.Size)
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
