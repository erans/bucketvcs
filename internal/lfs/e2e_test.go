package lfs_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/gateway"
	"github.com/bucketvcs/bucketvcs/internal/lfs"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// fakeAuthE2E used only for the integration test — minimal: one user,
// PermWrite on one repo.
type fakeAuthE2E struct{}

func (f *fakeAuthE2E) GetRepoFlags(context.Context, string, string) (auth.RepoFlags, error) {
	return auth.RepoFlags{}, nil
}
func (f *fakeAuthE2E) LookupRepoPerm(context.Context, *auth.Actor, string, string) (auth.Perm, error) {
	return auth.PermWrite, nil
}
func (f *fakeAuthE2E) VerifyCredential(_ context.Context, c auth.Credential) (*auth.Actor, string, *auth.Scope, error) {
	return &auth.Actor{Name: "alice"}, "tok", nil, nil
}
func (f *fakeAuthE2E) TouchTokenUsage(context.Context, string) error { return nil }
func (f *fakeAuthE2E) AddSSHKey(context.Context, auth.SSHKey) error  { return nil }
func (f *fakeAuthE2E) ListSSHKeysForUser(context.Context, string) ([]auth.SSHKey, error) {
	return nil, nil
}
func (f *fakeAuthE2E) ListSSHKeysForRepo(_ context.Context, _, _ string) ([]auth.SSHKey, error) {
	return nil, nil
}
func (f *fakeAuthE2E) RevokeSSHKey(context.Context, string) error                 { return nil }
func (f *fakeAuthE2E) TouchSSHKeyUsage(context.Context, string) error             { return nil }
func (f *fakeAuthE2E) GetUserByName(context.Context, string) (*auth.User, error)  { return nil, nil }
func (f *fakeAuthE2E) Close() error                                               { return nil }

// nilProxiedKeyResolver satisfies gateway.ProxiedKeyResolver for the
// LFS-only e2e test. The proxied bundle/pack routes are not exercised
// here; only LFS uses /_lfs/, which is gated by its own HMAC token.
// gateway.NewServer requires a non-nil resolver whenever a signing key
// is configured, so this stub exists purely to satisfy that contract.
type nilProxiedKeyResolver struct{}

func (nilProxiedKeyResolver) BundleKey(string) (string, bool) { return "", false }
func (nilProxiedKeyResolver) PackKey(string) (string, bool)   { return "", false }

func TestE2E_LFS_LocalfsProxiedTransfer(t *testing.T) {
	dir := t.TempDir()
	l, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	defer l.Close()

	// 32-byte HMAC key (>= 16 required by gateway.NewServer).
	key := bytes.Repeat([]byte{0xab}, 32)

	srv := httptest.NewServer(nil) // placeholder so we know the URL
	defer srv.Close()
	baseURL := srv.URL

	gw, err := gateway.NewServer(l, gateway.Options{
		MirrorDir:            t.TempDir(),
		AuthStore:            &fakeAuthE2E{},
		LFSEnabled:           true,
		LFSPresignTTL:        time.Minute,
		ProxiedURLSigningKey: key,
		ProxiedBaseURL:       baseURL,
		ProxiedKeyResolver:   nilProxiedKeyResolver{},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer gw.Close()
	srv.Config.Handler = gw

	// Compute a real LFS OID for the payload.
	payload := []byte("the bunny hops at dawn")
	sum := sha256.Sum256(payload)
	oid := hex.EncodeToString(sum[:])

	// 1. Batch upload — expect upload action with a /_lfs/ URL.
	batchReq, _ := json.Marshal(lfs.BatchRequest{
		Operation: "upload",
		Transfers: []string{"basic"},
		Objects:   []lfs.ObjectRef{{OID: oid, Size: int64(len(payload))}},
	})
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/acme/foo.git/info/lfs/objects/batch", bytes.NewReader(batchReq))
	req.Header.Set("Content-Type", lfs.ContentType)
	req.SetBasicAuth("alice", "pw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("batch upload: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("batch status=%d", resp.StatusCode)
	}
	var batchResp lfs.BatchResponse
	_ = json.NewDecoder(resp.Body).Decode(&batchResp)
	resp.Body.Close()
	if len(batchResp.Objects) != 1 || batchResp.Objects[0].Error != nil {
		t.Fatalf("batchResp=%+v", batchResp)
	}
	uploadAction, ok := batchResp.Objects[0].Actions["upload"]
	if !ok || uploadAction.Href == "" {
		t.Fatalf("upload action missing or empty href: %+v", batchResp.Objects[0])
	}

	// 2. PUT the payload to the upload URL.
	putReq, _ := http.NewRequest(http.MethodPut, uploadAction.Href, bytes.NewReader(payload))
	if ct := uploadAction.Header["Content-Type"]; ct != "" {
		putReq.Header.Set("Content-Type", ct)
	}
	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	putResp.Body.Close()
	if putResp.StatusCode != 200 {
		t.Fatalf("PUT status=%d", putResp.StatusCode)
	}

	// 3. Batch download — expect a download action.
	batchReq2, _ := json.Marshal(lfs.BatchRequest{
		Operation: "download",
		Transfers: []string{"basic"},
		Objects:   []lfs.ObjectRef{{OID: oid, Size: int64(len(payload))}},
	})
	req2, _ := http.NewRequest(http.MethodPost, baseURL+"/acme/foo.git/info/lfs/objects/batch", bytes.NewReader(batchReq2))
	req2.Header.Set("Content-Type", lfs.ContentType)
	req2.SetBasicAuth("alice", "pw")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("batch download: %v", err)
	}
	var batchResp2 lfs.BatchResponse
	_ = json.NewDecoder(resp2.Body).Decode(&batchResp2)
	resp2.Body.Close()
	downloadAction := batchResp2.Objects[0].Actions["download"]
	if downloadAction.Href == "" {
		t.Fatalf("download action missing href: %+v", batchResp2.Objects[0])
	}

	// 4. GET via download URL — body must equal payload.
	getResp, err := http.Get(downloadAction.Href)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	got, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	if !bytes.Equal(got, payload) {
		t.Fatalf("downloaded bytes differ: got %q want %q", got, payload)
	}
}
