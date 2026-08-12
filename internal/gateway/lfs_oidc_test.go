package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/lfs"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// TestE2E_LFSBatchUpload_OIDCScopedToken_Succeeds is the U-1 regression:
// an OIDC-minted (repo-bound) write token must be able to drive an LFS
// upload batch. RunAuth honors the credential's scope.Perm over the
// per-repo DB lookup because grants to the synthetic _oidc owner are
// forbidden (so LookupRepoPerm always returns PermNone for these tokens).
// Before the fix, handleBatch re-derived permission purely from
// LookupRepoPerm, so every OIDC-token upload batch 403'd while git
// receive-pack worked. This test drives the full gateway stack — real
// sqlitestore (real MintOIDCToken → real VerifyCredential scope path),
// real RunAuth, real lfs handler wired by NewServer.
func TestE2E_LFSBatchUpload_OIDCScopedToken_Succeeds(t *testing.T) {
	objStore, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = objStore.Close() })

	authStore, err := sqlitestore.Open(t.TempDir() + "/auth.db")
	if err != nil {
		t.Fatalf("sqlitestore.Open: %v", err)
	}
	t.Cleanup(func() { _ = authStore.Close() })
	if err := authStore.RegisterRepo(context.Background(), "acme", "app"); err != nil {
		t.Fatalf("RegisterRepo: %v", err)
	}

	// Mint the token the same way the OIDC exchange does (write rule incl.
	// lfs:write so the M17 scope gate also passes).
	tok, err := authStore.MintOIDCToken(context.Background(), sqlitestore.MintOIDCParams{
		Tenant:     "acme",
		Repo:       "app",
		Perm:       auth.PermWrite,
		Scopes:     auth.ScopeRepoWrite | auth.ScopeLFSWrite,
		TTLSeconds: 900,
		Label:      "oidc:gh:repo:acme/app",
	})
	if err != nil {
		t.Fatalf("MintOIDCToken: %v", err)
	}

	// httptest first so the proxied base URL can point at the eventual
	// listener (same pattern as internal/lfs e2e).
	ts := httptest.NewServer(nil)
	t.Cleanup(ts.Close)
	baseURL := ts.URL

	key := bytes.Repeat([]byte{0xab}, 32)
	srv, err := NewServer(objStore, Options{
		MirrorDir:               t.TempDir(),
		Version:                 "test",
		AuthStore:               authStore,
		LFSEnabled:              true,
		LFSPresignTTL:           time.Minute,
		LFSProxiedURLSigningKey: key,
		LFSProxiedBaseURL:       baseURL,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ts.Config.Handler = srv

	payload := []byte("oidc upload regression")
	sum := sha256.Sum256(payload)
	oid := hex.EncodeToString(sum[:])
	body, _ := json.Marshal(lfs.BatchRequest{
		Operation: "upload",
		Transfers: []string{"basic"},
		Objects:   []lfs.ObjectRef{{OID: oid, Size: int64(len(payload))}},
	})
	req, err := http.NewRequest(http.MethodPost, baseURL+"/acme/app.git/info/lfs/objects/batch", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", lfs.ContentType)
	// OIDC-minted tokens ignore the URL username (verifyBasicPassword OIDC
	// branch); CI plumbs them into arbitrary user slots.
	req.SetBasicAuth("x-access-token", tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("batch POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("batch status = %d, want 200 (OIDC write token must upload)", resp.StatusCode)
	}
	var batchResp lfs.BatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&batchResp); err != nil {
		t.Fatalf("decode batch response: %v", err)
	}
	if len(batchResp.Objects) != 1 || batchResp.Objects[0].Error != nil {
		t.Fatalf("batch objects = %+v, want one error-free entry", batchResp.Objects)
	}
	uploadAction, ok := batchResp.Objects[0].Actions["upload"]
	if !ok || uploadAction.Href == "" {
		t.Fatalf("upload action missing/empty href: %+v", batchResp.Objects[0])
	}
}
