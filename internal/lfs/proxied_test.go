package lfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/proxiedurl"
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

func TestProxiedObjectHandler_RejectsPostAndDelete(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	srv, _ := newProxiedHandlerForTest(t, key)
	defer srv.Close()

	for _, m := range []string{http.MethodPost, http.MethodDelete, http.MethodPatch} {
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
