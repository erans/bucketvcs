// Round-2 M17 roborev fix M1: dedicated unit tests covering the LFS
// Batch handler's M17 token-scope enforcement (download requires
// lfs:read, upload requires lfs:write, legacy tokens bypass). The
// existing LFS handler tests use legacy actors (Scopes==0) and so
// bypass the scope check; this file fills that coverage gap.
package lfs

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// TestHandler_Batch_Download_ForbiddenWithoutLFSRead exercises the
// download branch of handleBatch with a non-legacy actor whose scopes
// do not include lfs:read. CheckScope must reject before any object
// resolution, and the response must mention lfs:read so the client
// understands which scope to request.
func TestHandler_Batch_Download_ForbiddenWithoutLFSRead(t *testing.T) {
	store := newBatchStore(nil, signedFn())
	authStore := &fakeAuth{
		repoPerm: map[string]auth.Perm{"acme/foo": auth.PermRead},
		actors:   map[string]*auth.Actor{"pw": {Name: "alice"}},
	}
	// Actor has repo:read but NOT lfs:read. EffectiveScopes does not
	// promote repo:read into lfs:read, so the LFS download must 403.
	actor := &auth.Actor{Name: "alice", UserID: "u-alice", Scopes: auth.ScopeRepoRead}
	srv := newHandlerForTest(t, store, authStore, actor)
	defer srv.Close()

	body, _ := json.Marshal(BatchRequest{
		Operation: "download",
		Transfers: []string{"basic"},
		Objects:   []ObjectRef{{OID: oidNew, Size: 1}},
	})
	req, _ := http.NewRequest(http.MethodPost,
		srv.URL+"/acme/foo.git/info/lfs/objects/batch",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", ContentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", resp.StatusCode)
	}
	b, _ := readAllBody(resp)
	if !strings.Contains(b, "lfs:read") {
		t.Errorf("body should mention lfs:read scope: %s", b)
	}
	if !strings.Contains(b, "insufficient scope") {
		t.Errorf("body should mention 'insufficient scope': %s", b)
	}
}

// TestHandler_Batch_Upload_ForbiddenWithoutLFSWrite covers the upload
// branch. lfs:read alone (no lfs:write) is the relevant boundary: the
// caller can pull LFS blobs but must NOT be able to push.
func TestHandler_Batch_Upload_ForbiddenWithoutLFSWrite(t *testing.T) {
	store := newBatchStore(nil, signedFn())
	authStore := &fakeAuth{
		repoPerm: map[string]auth.Perm{"acme/foo": auth.PermWrite},
		actors:   map[string]*auth.Actor{"pw": {Name: "alice"}},
	}
	actor := &auth.Actor{Name: "alice", UserID: "u-alice", Scopes: auth.ScopeLFSRead}
	srv := newHandlerForTest(t, store, authStore, actor)
	defer srv.Close()

	body, _ := json.Marshal(BatchRequest{
		Operation: "upload",
		Transfers: []string{"basic"},
		Objects:   []ObjectRef{{OID: oidNew, Size: 1}},
	})
	req, _ := http.NewRequest(http.MethodPost,
		srv.URL+"/acme/foo.git/info/lfs/objects/batch",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", ContentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", resp.StatusCode)
	}
	b, _ := readAllBody(resp)
	if !strings.Contains(b, "lfs:write") {
		t.Errorf("body should mention lfs:write scope: %s", b)
	}
}

// TestHandler_Batch_Download_AllowedWithLFSRead is the happy-path
// counterpart for download: lfs:read on the actor must pass the scope
// gate. The store is empty so each object surfaces a not-found per-
// object error in the response — but the outer Batch must return 200,
// not 403, proving the scope check let the request through.
func TestHandler_Batch_Download_AllowedWithLFSRead(t *testing.T) {
	store := newBatchStore(nil, signedFn())
	authStore := &fakeAuth{
		repoPerm: map[string]auth.Perm{"acme/foo": auth.PermRead},
		actors:   map[string]*auth.Actor{"pw": {Name: "alice"}},
	}
	actor := &auth.Actor{Name: "alice", UserID: "u-alice", Scopes: auth.ScopeLFSRead}
	srv := newHandlerForTest(t, store, authStore, actor)
	defer srv.Close()

	body, _ := json.Marshal(BatchRequest{
		Operation: "download",
		Transfers: []string{"basic"},
		Objects:   []ObjectRef{{OID: oidNew, Size: 1}},
	})
	req, _ := http.NewRequest(http.MethodPost,
		srv.URL+"/acme/foo.git/info/lfs/objects/batch",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", ContentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := readAllBody(resp)
		t.Fatalf("status=%d, want 200; body=%s", resp.StatusCode, b)
	}
}

// TestHandler_Batch_Upload_AllowedWithLFSWrite is the happy-path
// counterpart for upload: lfs:write covers the scope check; the
// secondary perm check (PermWrite) lets the request through to a 200.
func TestHandler_Batch_Upload_AllowedWithLFSWrite(t *testing.T) {
	store := newProxiedBatchStore(nil, signedFn())
	authStore := &fakeAuth{
		repoPerm: map[string]auth.Perm{"acme/foo": auth.PermWrite},
		actors:   map[string]*auth.Actor{"pw": {Name: "alice"}},
	}
	actor := &auth.Actor{Name: "alice", UserID: "u-alice", Scopes: auth.ScopeLFSWrite}
	srv := newHandlerForTest(t, store, authStore, actor)
	defer srv.Close()

	body, _ := json.Marshal(BatchRequest{
		Operation: "upload",
		Transfers: []string{"basic"},
		Objects:   []ObjectRef{{OID: oidNew, Size: 1}},
	})
	req, _ := http.NewRequest(http.MethodPost,
		srv.URL+"/acme/foo.git/info/lfs/objects/batch",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", ContentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := readAllBody(resp)
		t.Fatalf("status=%d, want 200; body=%s", resp.StatusCode, b)
	}
}

// TestHandler_Batch_LegacyTokenBypassesScopeCheck guards against a
// regression where the M17 scope wiring would inadvertently break
// pre-M17 tokens. An actor with ScopeLegacy (== 0) must pass through
// CheckScope unchanged — only the secondary perm check applies.
func TestHandler_Batch_LegacyTokenBypassesScopeCheck(t *testing.T) {
	store := newBatchStore(nil, signedFn())
	authStore := &fakeAuth{
		repoPerm: map[string]auth.Perm{"acme/foo": auth.PermRead},
		actors:   map[string]*auth.Actor{"pw": {Name: "alice"}},
	}
	actor := &auth.Actor{Name: "alice", UserID: "u-alice", Scopes: auth.ScopeLegacy}
	srv := newHandlerForTest(t, store, authStore, actor)
	defer srv.Close()

	body, _ := json.Marshal(BatchRequest{
		Operation: "download",
		Transfers: []string{"basic"},
		Objects:   []ObjectRef{{OID: oidNew, Size: 1}},
	})
	req, _ := http.NewRequest(http.MethodPost,
		srv.URL+"/acme/foo.git/info/lfs/objects/batch",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", ContentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	b, _ := readAllBody(resp)
	// Legacy bypass must NOT produce a 403-with-insufficient-scope. We
	// permit other statuses (the store is empty so per-object errors
	// are fine), but the scope check itself must not fire.
	if resp.StatusCode == http.StatusForbidden && strings.Contains(b, "insufficient scope") {
		t.Errorf("legacy actor unexpectedly rejected on scope: status=%d body=%s",
			resp.StatusCode, b)
	}
}

// readAllBody returns the response body as a string for substring asserts.
func readAllBody(resp *http.Response) (string, error) {
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return "", err
	}
	return buf.String(), nil
}
