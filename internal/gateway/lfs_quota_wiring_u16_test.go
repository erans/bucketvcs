package gateway

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

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/lfs"
	"github.com/bucketvcs/bucketvcs/internal/lfs/quota"
	"github.com/bucketvcs/bucketvcs/internal/proxiedurl"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// Wave-3 U-16 regression coverage: gateway.Options had NO quota field, so
// NewServer never threaded the quota service the serve layer already
// constructed into the LFS deps — production enforcement (Batch CheckBatch)
// and charging (proxied verify Add) were dead. These tests drive both seam
// points through the full NewServer path and pin both behaviors:
//
//   * nil Quota → batch always negotiates, verify never charges (exactly
//     today's behavior; replica/minimal deployments must keep working),
//   * wired Quota → over-quota upload batches are rejected with per-object
//     507s and successful proxied verifies charge used_bytes exactly once.

// newU16TestGateway builds a gateway with LFS (+ proxied transfer) enabled.
// wired controls whether Options.Quota carries the given quota service; the
// quota service itself is always returned so the test can Set/Get limits
// regardless of wiring.
func newU16TestGateway(t *testing.T, wired bool) (ts *httptest.Server, qsvc *quota.Service) {
	t.Helper()
	storeDir := t.TempDir()
	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	authdb, err := sqlitestore.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlitestore.Open: %v", err)
	}
	t.Cleanup(func() { _ = authdb.Close() })
	qsvc = quota.New(authdb.DB(), nil)

	key := bytes.Repeat([]byte{0xab}, 32)
	opts := Options{
		MirrorDir:               t.TempDir(),
		Version:                 "test",
		AuthStore:               newPermissiveAuthStore(t, "acme", "demo"),
		LFSEnabled:              true,
		LFSPresignTTL:           time.Minute,
		LFSProxiedURLSigningKey: key,
		LFSProxiedBaseURL:       "http://lfs.example",
	}
	if wired {
		opts.Quota = qsvc
	}
	srv, err := NewServer(store, opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ts = httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, qsvc
}

// postBatch POSTs an upload batch for one object and returns the decoded
// response (the LFS batch endpoint reports quota rejection as per-object
// errors inside a 200 envelope).
func postBatch(t *testing.T, ts *httptest.Server, oid string, size int64) (int, lfs.BatchResponse) {
	t.Helper()
	body, _ := json.Marshal(lfs.BatchRequest{
		Operation: "upload",
		Transfers: []string{"basic"},
		Objects:   []lfs.ObjectRef{{OID: oid, Size: size}},
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
	var br lfs.BatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&br); err != nil {
		t.Fatalf("decode batch response: %v", err)
	}
	return resp.StatusCode, br
}

func TestLFSQuota_BatchEnforcedOnlyWhenWired_U16(t *testing.T) {
	// --- nil Quota: behavior exactly as today, even over the limit ---
	tsNil, qsvcNil := newU16TestGateway(t, false)
	if err := qsvcNil.Set(context.Background(), "acme", 10); err != nil {
		t.Fatalf("Set: %v", err)
	}

	fat := []byte("way over the 10-byte quota limit")
	sum := sha256.Sum256(fat)
	oid := hex.EncodeToString(sum[:])
	code, br := postBatch(t, tsNil, oid, int64(len(fat)))
	if code != http.StatusOK {
		t.Fatalf("nil Quota: batch status=%d, want 200", code)
	}
	if len(br.Objects) != 1 || br.Objects[0].Error != nil {
		t.Fatalf("nil Quota: objects[0].Error=%+v, want nil — nil Quota must mean no enforcement", br.Objects[0].Error)
	}
	if up, ok := br.Objects[0].Actions["upload"]; !ok || up.Href == "" {
		t.Fatalf("nil Quota: expected an upload action to be negotiated; actions=%v", br.Objects[0].Actions)
	}

	// --- wired Quota: enforcement fires ---
	tsWired, qsvcWired := newU16TestGateway(t, true)
	if err := qsvcWired.Set(context.Background(), "acme", 10); err != nil {
		t.Fatalf("Set: %v", err)
	}
	code, br = postBatch(t, tsWired, oid, int64(len(fat)))
	if code != http.StatusOK {
		t.Fatalf("wired Quota: batch status=%d, want 200 (per-object 507s live inside the envelope)", code)
	}
	if len(br.Objects) != 1 {
		t.Fatalf("wired Quota: len(Objects)=%d, want 1", len(br.Objects))
	}
	obj := br.Objects[0]
	if obj.Error == nil || obj.Error.Code != 507 {
		t.Fatalf("wired Quota: objects[0].Error=%+v, want code 507 (U-16: enforcement unwired if nil)", obj.Error)
	}
	if len(obj.Actions) != 0 {
		t.Fatalf("wired Quota: rejected object must carry no actions; got %v", obj.Actions)
	}
	st, err := qsvcWired.Get(context.Background(), "acme")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if st.UsedBytes != 0 {
		t.Fatalf("batch must check, not charge: UsedBytes=%d, want 0", st.UsedBytes)
	}

	// A subsequent batch that fits the limit still negotiates when wired.
	small := []byte("fit")
	sum2 := sha256.Sum256(small)
	oid2 := hex.EncodeToString(sum2[:])
	_, br2 := postBatch(t, tsWired, oid2, int64(len(small)))
	if len(br2.Objects) != 1 || br2.Objects[0].Error != nil {
		t.Fatalf("wired Quota within limit: objects[0].Error=%+v, want nil", br2.Objects[0].Error)
	}
}

func TestLFSQuota_ProxiedVerifyChargesOnceOnlyWhenWired_U16(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32) // matches newU16TestGateway's signing key

	// --- wired Quota: verify charges used_bytes exactly once ---
	tsWired, qsvcWired := newU16TestGateway(t, true)
	if err := qsvcWired.Set(context.Background(), "acme", 1<<30); err != nil {
		t.Fatalf("Set: %v", err)
	}

	payload := []byte("proxied lfs object bytes")
	sum := sha256.Sum256(payload)
	oid := hex.EncodeToString(sum[:])

	mustProxiedPut(t, tsWired.URL, key, oid, payload) // PUT completes; not yet charged
	st, err := qsvcWired.Get(context.Background(), "acme")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if st.UsedBytes != 0 {
		t.Fatalf("charge belongs to verify, not PUT: UsedBytes=%d, want 0", st.UsedBytes)
	}

	mustProxiedVerify(t, tsWired.URL, key, oid, payload)
	st, _ = qsvcWired.Get(context.Background(), "acme")
	if st.UsedBytes != int64(len(payload)) {
		t.Fatalf("after first verify: UsedBytes=%d, want %d (U-16: verify charging unwired if 0)", st.UsedBytes, len(payload))
	}

	mustProxiedVerify(t, tsWired.URL, key, oid, payload) // double-verify replay
	st, _ = qsvcWired.Get(context.Background(), "acme")
	if st.UsedBytes != int64(len(payload)) {
		t.Fatalf("after second verify: UsedBytes=%d, want %d (double-count)", st.UsedBytes, len(payload))
	}

	// --- nil Quota: verify succeeds but never charges ---
	tsNil, qsvcNil := newU16TestGateway(t, false)
	if err := qsvcNil.Set(context.Background(), "acme", 1<<30); err != nil {
		t.Fatalf("Set: %v", err)
	}
	mustProxiedPut(t, tsNil.URL, key, oid, payload)
	mustProxiedVerify(t, tsNil.URL, key, oid, payload)
	st2, err := qsvcNil.Get(context.Background(), "acme")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if st2.UsedBytes != 0 {
		t.Fatalf("nil Quota must not charge: UsedBytes=%d, want 0", st2.UsedBytes)
	}
}

// mustProxiedPut drives proxied object PUT and requires a 200 (object stored).
func mustProxiedPut(t *testing.T, base string, key []byte, oid string, payload []byte) {
	t.Helper()
	tok, err := proxiedurl.Mint(key, "lfs-put", "acme/demo/"+oid, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Mint put: %v", err)
	}
	req, _ := http.NewRequest(http.MethodPut, base+"/_lfs/acme/demo/"+oid+"?token="+tok, bytes.NewReader(payload))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT status=%d want 200; body=%q", resp.StatusCode, b)
	}
}

// mustProxiedVerify drives proxied verify and requires a 200.
func mustProxiedVerify(t *testing.T, base string, key []byte, oid string, payload []byte) {
	t.Helper()
	tok, err := proxiedurl.Mint(key, "lfs-verify", "acme/demo/"+oid, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Mint verify: %v", err)
	}
	body, _ := json.Marshal(lfs.VerifyRequest{OID: oid, Size: int64(len(payload))})
	req, _ := http.NewRequest(http.MethodPost, base+"/_lfs/acme/demo/"+oid+"?token="+tok, bytes.NewReader(body))
	req.Header.Set("Content-Type", lfs.ContentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("verify POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("verify status=%d want 200; body=%q", resp.StatusCode, b)
	}
}
