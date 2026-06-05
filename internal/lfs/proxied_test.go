package lfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/proxiedurl"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// goodOID is a syntactically-valid OID used by tests that hit reject
// paths BEFORE the SHA-256 content check (token/method/format gates).
// Tests that exercise the success or hash-mismatch paths derive their
// OID from sha256(body) directly.
const goodOID = "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"

func newProxiedHandlerForTest(t *testing.T, key []byte) (*httptest.Server, *localfs.Localfs) {
	t.Helper()
	dir := t.TempDir()
	l, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	h := NewProxiedObjectHandler(ProxiedDeps{
		Store:  l,
		Key:    key,
		Logger: nil,
	})
	return httptest.NewServer(h), l
}

func mintLFSToken(t *testing.T, key []byte, kind, tenant, repo, oid string) string {
	t.Helper()
	tok, err := proxiedurl.Mint(key, kind, tenant+"/"+repo+"/"+oid, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	return tok
}

func TestProxiedObjectHandler_PutThenGet(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	srv, l := newProxiedHandlerForTest(t, key)
	defer srv.Close()

	putBody := []byte("hello LFS world")
	sum := sha256.Sum256(putBody)
	oid := hex.EncodeToString(sum[:])

	// PUT.
	putTok := mintLFSToken(t, key, "lfs-put", "acme", "foo", oid)
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/_lfs/acme/foo/"+oid+"?token="+putTok, bytes.NewReader(putBody))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status=%d", resp.StatusCode)
	}

	// HEAD via the localfs backend at the canonical LFS key.
	expectedKey := "tenants/acme/repos/foo/lfs/objects/" + oid
	meta, err := l.Head(context.Background(), expectedKey)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if meta.Size != int64(len(putBody)) {
		t.Fatalf("Head size=%d, want %d", meta.Size, len(putBody))
	}

	// GET via proxied handler.
	getTok := mintLFSToken(t, key, "lfs-get", "acme", "foo", oid)
	getResp, err := http.Get(srv.URL + "/_lfs/acme/foo/" + oid + "?token=" + getTok)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET status=%d", getResp.StatusCode)
	}
	got, _ := io.ReadAll(getResp.Body)
	if !bytes.Equal(got, putBody) {
		t.Errorf("body bytes differ: got %q want %q", got, putBody)
	}
}

func TestProxiedObjectHandler_RejectsMissingToken(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	srv, _ := newProxiedHandlerForTest(t, key)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/_lfs/acme/foo/" + goodOID)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", resp.StatusCode)
	}
}

func TestProxiedObjectHandler_RejectsKindMismatch(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	srv, _ := newProxiedHandlerForTest(t, key)
	defer srv.Close()

	// GET with a PUT-kind token.
	putTok := mintLFSToken(t, key, "lfs-put", "acme", "foo", goodOID)
	resp, err := http.Get(srv.URL + "/_lfs/acme/foo/" + goodOID + "?token=" + putTok)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", resp.StatusCode)
	}
}

func TestProxiedObjectHandler_RejectsBadOID(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	srv, _ := newProxiedHandlerForTest(t, key)
	defer srv.Close()

	badPaths := []string{
		"/_lfs/acme/foo/short",
		"/_lfs/acme/foo/" + strings.Repeat("Z", 64),
		"/_lfs/acme/foo/../etc/passwd",
		"/_lfs/acme/foo/" + strings.Repeat("a", 64) + "x",
	}
	for _, p := range badPaths {
		// Mint a token with the path's "oid" component anyway — the
		// OID-format reject should fire before token verification.
		resp, err := http.Get(srv.URL + p + "?token=fake")
		if err != nil {
			t.Fatalf("GET %q: %v", p, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%q: status=200 want 4xx", p)
		}
	}
}

func TestProxiedObjectHandler_RejectsBadTenantRepo(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	srv, _ := newProxiedHandlerForTest(t, key)
	defer srv.Close()

	badPaths := []string{
		"/_lfs//foo/" + goodOID,         // empty tenant
		"/_lfs/acme//" + goodOID,        // empty repo
		"/_lfs/../foo/" + goodOID,       // traversal in tenant
		"/_lfs/acme/.hidden/" + goodOID, // leading dot in repo
	}
	for _, p := range badPaths {
		resp, err := http.Get(srv.URL + p + "?token=fake")
		if err != nil {
			t.Fatalf("GET %q: %v", p, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%q: status=200 want 4xx", p)
		}
	}
}

func TestProxiedObjectHandler_RejectsDeleteAndPatch(t *testing.T) {
	// POST is the verify method (added in M13.1 T4); it is no longer 405.
	// DELETE and PATCH remain unsupported.
	key := bytes.Repeat([]byte{0xab}, 32)
	srv, _ := newProxiedHandlerForTest(t, key)
	defer srv.Close()

	for _, m := range []string{http.MethodDelete, http.MethodPatch} {
		req, _ := http.NewRequest(m, srv.URL+"/_lfs/acme/foo/"+goodOID+"?token=fake", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s: status=%d want 405", m, resp.StatusCode)
		}
	}
}

func TestProxiedObjectHandler_PutAlreadyExistsIsIdempotent(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	srv, _ := newProxiedHandlerForTest(t, key)
	defer srv.Close()

	body := []byte("dup-test")
	sum := sha256.Sum256(body)
	oid := hex.EncodeToString(sum[:])

	putTok := mintLFSToken(t, key, "lfs-put", "acme", "foo", oid)
	// First PUT.
	req1, _ := http.NewRequest(http.MethodPut, srv.URL+"/_lfs/acme/foo/"+oid+"?token="+putTok, bytes.NewReader(body))
	resp1, _ := http.DefaultClient.Do(req1)
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first PUT status=%d", resp1.StatusCode)
	}
	// Second PUT — same body, same key. Must be 200 (idempotent).
	req2, _ := http.NewRequest(http.MethodPut, srv.URL+"/_lfs/acme/foo/"+oid+"?token="+putTok, bytes.NewReader(body))
	resp2, _ := http.DefaultClient.Do(req2)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second PUT status=%d, want 200 (idempotent)", resp2.StatusCode)
	}
}

func newTestLocalfs(t *testing.T) *localfs.Localfs {
	t.Helper()
	dir := t.TempDir()
	l, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// TestProxiedObjectHandler_TokenInvalidMetricReasons asserts that
// lfs_object_token_invalid_total is emitted with the correct reason
// label for each of the four failure modes: missing, expired,
// kind_mismatch, and invalid (signature). captureLogger writes the
// metric record to a buffer we can grep.
func TestProxiedObjectHandler_TokenInvalidMetricReasons(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	cases := []struct {
		name        string
		kind        string
		hash        string
		exp         time.Duration
		signWithKey []byte
		token       string // "MISSING" marker = send no token
		wantReason  string
	}{
		{
			name:       "missing",
			token:      "MISSING",
			wantReason: "missing",
		},
		{
			name:        "expired",
			kind:        "lfs-get",
			hash:        "acme/foo/" + goodOID,
			exp:         -time.Hour,
			signWithKey: key,
			wantReason:  "expired",
		},
		{
			name:        "kind_mismatch",
			kind:        "lfs-put",
			hash:        "acme/foo/" + goodOID,
			exp:         time.Hour,
			signWithKey: key,
			wantReason:  "kind_mismatch",
		},
		{
			name:        "invalid_signature",
			kind:        "lfs-get",
			hash:        "acme/foo/" + goodOID,
			exp:         time.Hour,
			signWithKey: bytes.Repeat([]byte{0x99}, 32), // wrong key
			wantReason:  "invalid",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			h := NewProxiedObjectHandler(ProxiedDeps{
				Store:  newTestLocalfs(t),
				Key:    key,
				Logger: captureLogger(&buf),
			})
			srv := httptest.NewServer(h)
			defer srv.Close()

			url := srv.URL + "/_lfs/acme/foo/" + goodOID
			if tc.token != "MISSING" {
				tok, err := proxiedurl.Mint(tc.signWithKey, tc.kind, tc.hash, time.Now().Add(tc.exp))
				if err != nil {
					t.Fatalf("Mint: %v", err)
				}
				url += "?token=" + tok
			}
			resp, err := http.Get(url)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status=%d want 403", resp.StatusCode)
			}
			if !strings.Contains(buf.String(), `"reason":"`+tc.wantReason+`"`) {
				t.Errorf("expected reason=%q in metric log; got %s", tc.wantReason, buf.String())
			}
			if !strings.Contains(buf.String(), `"metric_name":"lfs_object_token_invalid_total"`) {
				t.Errorf("expected lfs_object_token_invalid_total metric; got %s", buf.String())
			}
		})
	}
}

// TestProxiedObjectHandler_RejectsMismatchedContent asserts that a PUT
// whose body hashes to something other than the URL's OID is rejected
// with 422. This is the LFS content-addressing invariant: a tenant
// with write perms must NOT be able to plant arbitrary bytes at an
// OID slot.
func TestProxiedObjectHandler_RejectsMismatchedContent(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	srv, _ := newProxiedHandlerForTest(t, key)
	defer srv.Close()

	// OID asserts content is "X-X-X" but we PUT "wrong-bytes".
	payload := []byte("X-X-X")
	sum := sha256.Sum256(payload)
	oid := hex.EncodeToString(sum[:])

	putTok := mintLFSToken(t, key, "lfs-put", "acme", "foo", oid)
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/_lfs/acme/foo/"+oid+"?token="+putTok, bytes.NewReader([]byte("wrong-bytes")))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d, want 422", resp.StatusCode)
	}
}

// TestProxiedObjectHandler_AcceptsMatchedContent is the positive
// counterpart: a PUT whose body hashes to the URL's OID is 200 and
// the bytes land at the canonical storage key.
func TestProxiedObjectHandler_AcceptsMatchedContent(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	srv, l := newProxiedHandlerForTest(t, key)
	defer srv.Close()

	payload := []byte("real content")
	sum := sha256.Sum256(payload)
	oid := hex.EncodeToString(sum[:])

	putTok := mintLFSToken(t, key, "lfs-put", "acme", "foo", oid)
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/_lfs/acme/foo/"+oid+"?token="+putTok, bytes.NewReader(payload))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	// Confirm the bytes were stored.
	expectedKey := "tenants/acme/repos/foo/lfs/objects/" + oid
	meta, err := l.Head(context.Background(), expectedKey)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if meta.Size != int64(len(payload)) {
		t.Errorf("size=%d want %d", meta.Size, len(payload))
	}
}

// TestProxiedObjectHandler_DuplicateMismatchIsRejected exercises the
// ErrAlreadyExists drain branch: even when the storage layer would
// have short-circuited as idempotent, a re-upload of mismatched bytes
// at the same OID is still rejected with 422 (not 200) because the
// gateway hashes the bytes the client sent, not the bytes on disk.
func TestProxiedObjectHandler_DuplicateMismatchIsRejected(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	srv, _ := newProxiedHandlerForTest(t, key)
	defer srv.Close()

	payload := []byte("first-content")
	sum := sha256.Sum256(payload)
	oid := hex.EncodeToString(sum[:])

	// First PUT — correct content, succeeds.
	putTok := mintLFSToken(t, key, "lfs-put", "acme", "foo", oid)
	req1, _ := http.NewRequest(http.MethodPut, srv.URL+"/_lfs/acme/foo/"+oid+"?token="+putTok, bytes.NewReader(payload))
	resp1, _ := http.DefaultClient.Do(req1)
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first PUT status=%d", resp1.StatusCode)
	}

	// Second PUT — same OID but wrong content. Even though
	// ErrAlreadyExists would normally return 200, the hash mismatch
	// must override and 422.
	req2, _ := http.NewRequest(http.MethodPut, srv.URL+"/_lfs/acme/foo/"+oid+"?token="+putTok, bytes.NewReader([]byte("different bytes")))
	resp2, _ := http.DefaultClient.Do(req2)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("second PUT (wrong bytes, same OID) status=%d, want 422", resp2.StatusCode)
	}
}

// --- M13.1 T4: POST /_lfs/<tenant>/<repo>/<oid> = verify ---

// postVerifyJSON is a small helper that POSTs a VerifyRequest body and
// returns the response + buffered body for inspection.
func postVerifyJSON(t *testing.T, url string, vreq VerifyRequest) *http.Response {
	t.Helper()
	body, err := json.Marshal(vreq)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", ContentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	return resp
}

// putForVerifyTest seeds an object via the proxied PUT path so a
// subsequent verify can succeed. Returns (oid, size).
func putForVerifyTest(t *testing.T, srvURL string, key []byte, tenant, repo string, payload []byte) (string, int64) {
	t.Helper()
	sum := sha256.Sum256(payload)
	oid := hex.EncodeToString(sum[:])
	putTok := mintLFSToken(t, key, "lfs-put", tenant, repo, oid)
	req, _ := http.NewRequest(http.MethodPut, srvURL+"/_lfs/"+tenant+"/"+repo+"/"+oid+"?token="+putTok, bytes.NewReader(payload))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("seed PUT: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seed PUT status=%d", resp.StatusCode)
	}
	return oid, int64(len(payload))
}

func TestProxied_Verify_OK(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	var buf bytes.Buffer
	h := NewProxiedObjectHandler(ProxiedDeps{
		Store:  newTestLocalfs(t),
		Key:    key,
		Logger: captureLogger(&buf),
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	oid, size := putForVerifyTest(t, srv.URL, key, "acme", "foo", []byte("verify-ok"))
	verTok := mintLFSToken(t, key, "lfs-verify", "acme", "foo", oid)
	resp := postVerifyJSON(t, srv.URL+"/_lfs/acme/foo/"+oid+"?token="+verTok, VerifyRequest{OID: oid, Size: size})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	if !strings.Contains(buf.String(), `"metric_name":"lfs_verify_requests_total"`) ||
		!strings.Contains(buf.String(), `"result":"ok"`) {
		t.Errorf("expected lfs_verify_requests_total{result=ok}; got %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"event":"lfs.verify"`) {
		t.Errorf("expected lfs.verify audit event; got %s", buf.String())
	}
}

func TestProxied_Verify_TokenMissing(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	var buf bytes.Buffer
	h := NewProxiedObjectHandler(ProxiedDeps{
		Store:  newTestLocalfs(t),
		Key:    key,
		Logger: captureLogger(&buf),
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp := postVerifyJSON(t, srv.URL+"/_lfs/acme/foo/"+goodOID, VerifyRequest{OID: goodOID, Size: 1})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d want 403", resp.StatusCode)
	}
	if !strings.Contains(buf.String(), `"metric_name":"lfs_object_token_invalid_total"`) ||
		!strings.Contains(buf.String(), `"reason":"missing"`) {
		t.Errorf("expected lfs_object_token_invalid_total{reason=missing}; got %s", buf.String())
	}
}

func TestProxied_Verify_TokenWrongKind(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	var buf bytes.Buffer
	h := NewProxiedObjectHandler(ProxiedDeps{
		Store:  newTestLocalfs(t),
		Key:    key,
		Logger: captureLogger(&buf),
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	// POST with a kind=3 (lfs-put) token — verify endpoint expects kind=5.
	putTok := mintLFSToken(t, key, "lfs-put", "acme", "foo", goodOID)
	resp := postVerifyJSON(t, srv.URL+"/_lfs/acme/foo/"+goodOID+"?token="+putTok, VerifyRequest{OID: goodOID, Size: 1})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d want 403", resp.StatusCode)
	}
	if !strings.Contains(buf.String(), `"reason":"kind_mismatch"`) {
		t.Errorf("expected reason=kind_mismatch; got %s", buf.String())
	}
}

func TestProxied_Verify_TokenExpired(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	var buf bytes.Buffer
	h := NewProxiedObjectHandler(ProxiedDeps{
		Store:  newTestLocalfs(t),
		Key:    key,
		Logger: captureLogger(&buf),
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	tok, err := proxiedurl.Mint(key, "lfs-verify", "acme/foo/"+goodOID, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	resp := postVerifyJSON(t, srv.URL+"/_lfs/acme/foo/"+goodOID+"?token="+tok, VerifyRequest{OID: goodOID, Size: 1})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d want 403", resp.StatusCode)
	}
	if !strings.Contains(buf.String(), `"reason":"expired"`) {
		t.Errorf("expected reason=expired; got %s", buf.String())
	}
}

func TestProxied_Verify_TokenHashMismatch(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	var buf bytes.Buffer
	h := NewProxiedObjectHandler(ProxiedDeps{
		Store:  newTestLocalfs(t),
		Key:    key,
		Logger: captureLogger(&buf),
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Token is minted for a different OID — hash mismatch surfaces as
	// reason=invalid (proxiedurl.Verify returns a non-typed mismatch err).
	otherOID := strings.Repeat("b", 64)
	tok := mintLFSToken(t, key, "lfs-verify", "acme", "foo", otherOID)
	resp := postVerifyJSON(t, srv.URL+"/_lfs/acme/foo/"+goodOID+"?token="+tok, VerifyRequest{OID: goodOID, Size: 1})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d want 403", resp.StatusCode)
	}
	if !strings.Contains(buf.String(), `"reason":"invalid"`) {
		t.Errorf("expected reason=invalid; got %s", buf.String())
	}
}

func TestProxied_Verify_BodyOIDMismatch(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	var buf bytes.Buffer
	h := NewProxiedObjectHandler(ProxiedDeps{
		Store:  newTestLocalfs(t),
		Key:    key,
		Logger: captureLogger(&buf),
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	urlOID, _ := putForVerifyTest(t, srv.URL, key, "acme", "foo", []byte("body-oid-mismatch"))
	bodyOID := strings.Repeat("d", 64)
	verTok := mintLFSToken(t, key, "lfs-verify", "acme", "foo", urlOID)
	resp := postVerifyJSON(t, srv.URL+"/_lfs/acme/foo/"+urlOID+"?token="+verTok, VerifyRequest{OID: bodyOID, Size: 1})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d want 422", resp.StatusCode)
	}
	if !strings.Contains(buf.String(), `"metric_name":"lfs_verify_requests_total"`) ||
		!strings.Contains(buf.String(), `"result":"error"`) {
		t.Errorf("expected lfs_verify_requests_total{result=error}; got %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"event":"lfs.verify"`) {
		t.Errorf("expected lfs.verify audit event; got %s", buf.String())
	}
}

func TestProxied_Verify_SizeMismatch(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	var buf bytes.Buffer
	h := NewProxiedObjectHandler(ProxiedDeps{
		Store:  newTestLocalfs(t),
		Key:    key,
		Logger: captureLogger(&buf),
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	oid, size := putForVerifyTest(t, srv.URL, key, "acme", "foo", []byte("size-check"))
	verTok := mintLFSToken(t, key, "lfs-verify", "acme", "foo", oid)
	resp := postVerifyJSON(t, srv.URL+"/_lfs/acme/foo/"+oid+"?token="+verTok, VerifyRequest{OID: oid, Size: size + 1})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d want 422", resp.StatusCode)
	}
	if !strings.Contains(buf.String(), `"result":"size_mismatch"`) {
		t.Errorf("expected result=size_mismatch; got %s", buf.String())
	}
}

func TestProxied_Verify_Missing(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	var buf bytes.Buffer
	h := NewProxiedObjectHandler(ProxiedDeps{
		Store:  newTestLocalfs(t),
		Key:    key,
		Logger: captureLogger(&buf),
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	// No PUT first — object is missing.
	verTok := mintLFSToken(t, key, "lfs-verify", "acme", "foo", goodOID)
	resp := postVerifyJSON(t, srv.URL+"/_lfs/acme/foo/"+goodOID+"?token="+verTok, VerifyRequest{OID: goodOID, Size: 100})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d want 404", resp.StatusCode)
	}
	if !strings.Contains(buf.String(), `"result":"missing"`) {
		t.Errorf("expected result=missing; got %s", buf.String())
	}
}

// errHeadStore wraps a real ObjectStore for everything except Head,
// which always returns a synthetic backend error. Used to exercise the
// 500 result=error branch of serveVerify.
type errHeadStore struct {
	storage.ObjectStore
	err error
}

func (e *errHeadStore) Head(context.Context, string) (*storage.ObjectMetadata, error) {
	return nil, e.err
}

func TestProxied_Verify_BackendError(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	var buf bytes.Buffer
	backend := &errHeadStore{ObjectStore: newTestLocalfs(t), err: errors.New("boom")}
	h := NewProxiedObjectHandler(ProxiedDeps{
		Store:  backend,
		Key:    key,
		Logger: captureLogger(&buf),
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	verTok := mintLFSToken(t, key, "lfs-verify", "acme", "foo", goodOID)
	resp := postVerifyJSON(t, srv.URL+"/_lfs/acme/foo/"+goodOID+"?token="+verTok, VerifyRequest{OID: goodOID, Size: 1})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500", resp.StatusCode)
	}
	if !strings.Contains(buf.String(), `"result":"error"`) {
		t.Errorf("expected result=error; got %s", buf.String())
	}
}

// TestProxied_Verify_RejectsMismatchedContentType covers the 415 branch
// in serveVerify: a verify POST with a non-LFS Content-Type is rejected
// before the body is parsed. Empty Content-Type is tolerated (mirrors
// the handler.go policy) and is covered by the other Verify_OK tests.
func TestProxied_Verify_RejectsMismatchedContentType(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	var buf bytes.Buffer
	h := NewProxiedObjectHandler(ProxiedDeps{
		Store:  newTestLocalfs(t),
		Key:    key,
		Logger: captureLogger(&buf),
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	verTok := mintLFSToken(t, key, "lfs-verify", "acme", "foo", goodOID)
	body, _ := json.Marshal(VerifyRequest{OID: goodOID, Size: 1})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/_lfs/acme/foo/"+goodOID+"?token="+verTok, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status=%d want 415", resp.StatusCode)
	}
	if !strings.Contains(buf.String(), `"result":"error"`) {
		t.Errorf("expected verify metric/audit result=error; got %s", buf.String())
	}
}

// TestProxiedReadOnlyReplica asserts the proxied /_lfs/ handler refuses
// upload (PUT) and verify (POST) with a clean 403 on a read-only replica
// — even with a fully valid HMAC token replayed from the write region —
// while download (GET) is unaffected.
func TestProxiedReadOnlyReplica(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	dir := t.TempDir()
	l, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	// Seed an object directly so the GET happy path has something to read.
	payload := []byte("replica download payload")
	sum := sha256.Sum256(payload)
	oid := hex.EncodeToString(sum[:])
	storageKey := RepoLFSPrefix("acme", "foo") + oid
	if _, perr := l.PutIfAbsent(context.Background(), storageKey, bytes.NewReader(payload), nil); perr != nil {
		t.Fatalf("seed PutIfAbsent: %v", perr)
	}

	h := NewProxiedObjectHandler(ProxiedDeps{
		Store:           l,
		Key:             key,
		Logger:          nil,
		ReadOnlyReplica: true,
		WriteRegionURL:  "https://write.example.com",
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	// (a) PUT with a valid lfs-put token → 403 (not 500), body mentions replica.
	putTok := mintLFSToken(t, key, "lfs-put", "acme", "foo", oid)
	putReq, _ := http.NewRequest(http.MethodPut, srv.URL+"/_lfs/acme/foo/"+oid+"?token="+putTok, bytes.NewReader(payload))
	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	putBody, _ := io.ReadAll(putResp.Body)
	putResp.Body.Close()
	if putResp.StatusCode != http.StatusForbidden {
		t.Fatalf("PUT status=%d want 403; body=%s", putResp.StatusCode, putBody)
	}
	if !strings.Contains(string(putBody), "read-only replica") {
		t.Errorf("PUT body=%q, want 'read-only replica'", putBody)
	}

	// (b) verify (POST) with a valid lfs-verify token → 403.
	verTok := mintLFSToken(t, key, "lfs-verify", "acme", "foo", oid)
	verResp := postVerifyJSON(t, srv.URL+"/_lfs/acme/foo/"+oid+"?token="+verTok, VerifyRequest{OID: oid, Size: int64(len(payload))})
	verBody, _ := io.ReadAll(verResp.Body)
	verResp.Body.Close()
	if verResp.StatusCode != http.StatusForbidden {
		t.Fatalf("verify status=%d want 403; body=%s", verResp.StatusCode, verBody)
	}
	if !strings.Contains(string(verBody), "read-only replica") {
		t.Errorf("verify body=%q, want 'read-only replica'", verBody)
	}

	// (c) GET (download) with a valid lfs-get token → unaffected (200).
	getTok := mintLFSToken(t, key, "lfs-get", "acme", "foo", oid)
	getResp, err := http.Get(srv.URL + "/_lfs/acme/foo/" + oid + "?token=" + getTok)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	got, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET status=%d want 200", getResp.StatusCode)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("GET body=%q want %q", got, payload)
	}
}
