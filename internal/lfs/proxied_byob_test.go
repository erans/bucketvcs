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

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// Wave-2 U-7 regression coverage: proxied /_lfs/ transfers must route to the
// per-tenant BYOB store when a resolver is wired, not the operator store,
// and must fail closed on resolver error.

// tenantMapResolver routes mapped tenants to their per-tenant store and
// reports auth.ErrNoSuchBinding for the rest (the Resolver contract's
// genuinely-absent shape). An err field forces every call to fail.
type tenantMapResolver struct {
	stores map[string]storage.ObjectStore
	err    error
}

func (r *tenantMapResolver) Resolve(_ context.Context, tenant string) (storage.ObjectStore, error) {
	if r.err != nil {
		return nil, r.err
	}
	if s, ok := r.stores[tenant]; ok {
		return s, nil
	}
	return nil, auth.ErrNoSuchBinding
}

func TestProxiedLFS_BYOB_PutVerifyLandInTenantStore(t *testing.T) {
	opDir, tenantDir := t.TempDir(), t.TempDir()
	opStore, err := localfs.Open(opDir)
	if err != nil {
		t.Fatalf("localfs.Open operator: %v", err)
	}
	t.Cleanup(func() { _ = opStore.Close() })
	tenantStore, err := localfs.Open(tenantDir)
	if err != nil {
		t.Fatalf("localfs.Open tenant: %v", err)
	}
	t.Cleanup(func() { _ = tenantStore.Close() })

	key := bytes.Repeat([]byte{0xcd}, 32)
	srv := httptest.NewServer(NewProxiedObjectHandler(ProxiedDeps{
		Store:    opStore,
		Resolver: &tenantMapResolver{stores: map[string]storage.ObjectStore{"acme": tenantStore}},
		Key:      key,
	}))
	defer srv.Close()

	putBody := []byte("BYOB tenant LFS bytes must live in the tenant store")
	sum := sha256.Sum256(putBody)
	oid := hex.EncodeToString(sum[:])

	// PUT via proxied handler with a valid lfs-put token.
	putTok := mintLFSToken(t, key, "lfs-put", "acme", "foo", oid)
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/_lfs/acme/foo/"+oid+"?token="+putTok, bytes.NewReader(putBody))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status=%d, want 200", resp.StatusCode)
	}

	expectedKey := "tenants/acme/repos/foo/lfs/objects/" + oid
	if _, err := tenantStore.Head(context.Background(), expectedKey); err != nil {
		t.Fatalf("tenant store Head: %v — bytes must land in the BYOB tenant store (U-7: pre-fix they silently land in the operator bucket)", err)
	}
	if _, err := opStore.Head(context.Background(), expectedKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("operator store Head err=%v, want ErrNotFound — proxied BYOB bytes must not enter the operator bucket", err)
	}

	// GET must read back from the tenant store.
	getTok := mintLFSToken(t, key, "lfs-get", "acme", "foo", oid)
	getResp, err := http.Get(srv.URL + "/_lfs/acme/foo/" + oid + "?token=" + getTok)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET status=%d, want 200", getResp.StatusCode)
	}
	got, _ := io.ReadAll(getResp.Body)
	if !bytes.Equal(got, putBody) {
		t.Errorf("GET body = %q, want %q", got, putBody)
	}

	// Verify must look in the tenant store too (cloud BYOB verify routes
	// here per adjudication).
	verifyTok := mintLFSToken(t, key, "lfs-verify", "acme", "foo", oid)
	vbody, _ := json.Marshal(VerifyRequest{OID: oid, Size: int64(len(putBody))})
	vreq, _ := http.NewRequest(http.MethodPost, srv.URL+"/_lfs/acme/foo/"+oid+"?token="+verifyTok, bytes.NewReader(vbody))
	vreq.Header.Set("Content-Type", ContentType)
	vresp, err := http.DefaultClient.Do(vreq)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	defer vresp.Body.Close()
	if vresp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(vresp.Body)
		t.Fatalf("verify status=%d, want 200 (object is durably in the tenant store); body=%s", vresp.StatusCode, b)
	}
}

func TestProxiedLFS_ResolverError_FailsClosed_500(t *testing.T) {
	opDir := t.TempDir()
	opStore, err := localfs.Open(opDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = opStore.Close() })

	key := bytes.Repeat([]byte{0xcd}, 32)
	resErr := errors.New("byob: binding for acme: authdb connection refused")
	srv := httptest.NewServer(NewProxiedObjectHandler(ProxiedDeps{
		Store:    opStore,
		Resolver: &tenantMapResolver{err: resErr},
		Key:      key,
	}))
	defer srv.Close()

	putBody := []byte("bytes")
	sum := sha256.Sum256(putBody)
	oid := hex.EncodeToString(sum[:])

	// PUT path.
	putTok := mintLFSToken(t, key, "lfs-put", "acme", "foo", oid)
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/_lfs/acme/foo/"+oid+"?token="+putTok, bytes.NewReader(putBody))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("PUT on resolver error must fail closed: status=%d, want 500", resp.StatusCode)
	}

	// GET path.
	getTok := mintLFSToken(t, key, "lfs-get", "acme", "foo", oid)
	getResp, err := http.Get(srv.URL + "/_lfs/acme/foo/" + oid + "?token=" + getTok)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("GET on resolver error must fail closed: status=%d, want 500", getResp.StatusCode)
	}

	// Verify path.
	verifyTok := mintLFSToken(t, key, "lfs-verify", "acme", "foo", oid)
	vbody, _ := json.Marshal(VerifyRequest{OID: oid, Size: int64(len(putBody))})
	vreq, _ := http.NewRequest(http.MethodPost, srv.URL+"/_lfs/acme/foo/"+oid+"?token="+verifyTok, bytes.NewReader(vbody))
	vreq.Header.Set("Content-Type", ContentType)
	vresp, err := http.DefaultClient.Do(vreq)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	defer vresp.Body.Close()
	if vresp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("verify on resolver error must fail closed: status=%d, want 500", vresp.StatusCode)
	}

	// Nothing may have entered the operator store.
	if _, err := opStore.Head(context.Background(), "tenants/acme/repos/foo/lfs/objects/"+oid); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("operator store Head err=%v, want ErrNotFound after failed PUT", err)
	}
}

func TestProxiedLFS_NoBinding_OperatorFallbackStillAllowed(t *testing.T) {
	// Canary for the one sanctioned fallback on proxied LFS: a resolver
	// reporting genuinely-absent binding must keep serving from the operator
	// store.
	opDir := t.TempDir()
	opStore, err := localfs.Open(opDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = opStore.Close() })

	key := bytes.Repeat([]byte{0xcd}, 32)
	srv := httptest.NewServer(NewProxiedObjectHandler(ProxiedDeps{
		Store:    opStore,
		Resolver: &tenantMapResolver{}, // no mapping → ErrNoSuchBinding shape
		Key:      key,
	}))
	defer srv.Close()

	putBody := []byte("operator lfs bytes")
	sum := sha256.Sum256(putBody)
	oid := hex.EncodeToString(sum[:])

	// Seed the object directly in the operator store; GET via proxied handler.
	expectedKey := "tenants/acme/repos/foo/lfs/objects/" + oid
	if _, err := opStore.PutIfAbsent(context.Background(), expectedKey, strings.NewReader(string(putBody)), nil); err != nil {
		t.Fatalf("seed operator store: %v", err)
	}
	getTok := mintLFSToken(t, key, "lfs-get", "acme", "foo", oid)
	getResp, err := http.Get(srv.URL + "/_lfs/acme/foo/" + oid + "?token=" + getTok)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("absent binding must keep operator fallback: GET status=%d, want 200", getResp.StatusCode)
	}
	got, _ := io.ReadAll(getResp.Body)
	if !bytes.Equal(got, putBody) {
		t.Errorf("GET body = %q, want %q", got, putBody)
	}
}
