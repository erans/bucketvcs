package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/lfs"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// Wave-2 U-6 regression coverage: the LFS batch NewStore closure must not
// substitute the operator store on resolver error; the failure must surface
// as a request-level error, and the request-scoped context must be threaded
// (not context.Background()).

func TestLFSBatch_ResolverError_FailsClosed(t *testing.T) {
	storeDir := t.TempDir()
	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	key := bytes.Repeat([]byte{0xab}, 32)
	srv, err := NewServer(store, Options{
		MirrorDir:               t.TempDir(),
		Version:                 "test",
		AuthStore:               newPermissiveAuthStore(t, "acme", "demo"),
		StoreResolver:           &errResolver{err: errors.New("byob: decrypt creds for acme: cipher: message authentication failed")},
		LFSEnabled:              true,
		LFSPresignTTL:           time.Minute,
		LFSProxiedURLSigningKey: key,
		LFSProxiedBaseURL:       "http://lfs.example",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	payload := []byte("contents never stored")
	sum := sha256.Sum256(payload)
	oid := hex.EncodeToString(sum[:])
	body, _ := json.Marshal(lfs.BatchRequest{
		Operation: "upload",
		Transfers: []string{"basic"},
		Objects:   []lfs.ObjectRef{{OID: oid, Size: int64(len(payload))}},
	})
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/acme/demo.git/info/lfs/objects/batch", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", lfs.ContentType)
	// Permissive auth store: any basic credentials pass, with write perm.
	req.SetBasicAuth("any", "any")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("batch POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		// U-6 fail-open shape: the batch negotiated upload actions against
		// the operator store as if the resolver had succeeded.
		var br lfs.BatchResponse
		if err := json.NewDecoder(resp.Body).Decode(&br); err == nil {
			t.Fatalf("resolver error must fail closed with a non-200 batch response; got 200 with objects=%+v", br.Objects)
		}
		t.Fatalf("resolver error must fail closed with a non-200 batch response; got 200")
	}
	if resp.StatusCode != http.StatusInternalServerError {
		body2, _ := io.ReadAll(resp.Body)
		t.Fatalf("resolver error should surface as 500, got %d; body=%q", resp.StatusCode, body2)
	}
}

func TestLFSBatch_NilResolver_StillOperatorStore(t *testing.T) {
	// Canary: with no resolver configured (pre-BYOB single-store mode),
	// the batch endpoint keeps working against the operator store.
	storeDir := t.TempDir()
	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	key := bytes.Repeat([]byte{0xab}, 32)
	srv, err := NewServer(store, Options{
		MirrorDir:               t.TempDir(),
		Version:                 "test",
		AuthStore:               newPermissiveAuthStore(t, "acme", "demo"),
		LFSEnabled:              true,
		LFSPresignTTL:           time.Minute,
		LFSProxiedURLSigningKey: key,
		LFSProxiedBaseURL:       "http://lfs.example",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	payload := []byte("operator-store lfs object")
	sum := sha256.Sum256(payload)
	oid := hex.EncodeToString(sum[:])
	body, _ := json.Marshal(lfs.BatchRequest{
		Operation: "upload",
		Transfers: []string{"basic"},
		Objects:   []lfs.ObjectRef{{OID: oid, Size: int64(len(payload))}},
	})
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/acme/demo.git/info/lfs/objects/batch", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", lfs.ContentType)
	req.SetBasicAuth("any", "any")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("batch POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("nil-resolver batch: status=%d, want 200; body=%q", resp.StatusCode, b)
	}
	var br lfs.BatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&br); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(br.Objects) != 1 || br.Objects[0].Error != nil {
		t.Fatalf("nil-resolver batch objects=%+v, want one error-free entry", br.Objects)
	}
}
