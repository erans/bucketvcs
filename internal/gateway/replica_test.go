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
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/proxiedurl"
	"github.com/bucketvcs/bucketvcs/internal/replica"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// gateFunc adapts a plain func to the replica.Gate interface so tests can
// inject per-case advertise behavior (nil = healthy, non-nil = refused).
type gateFunc func(ctx context.Context, tenant, repo string) error

func (f gateFunc) CheckAdvertise(ctx context.Context, tenant, repo string) error {
	return f(ctx, tenant, repo)
}

// newReplicaServer stands up a Server over a seeded localfs repo with the
// given replica config. Uses the permissive auth store so receive-pack
// requests reach the handler (write refusal must fire regardless of auth).
func newReplicaServer(t *testing.T, cfg *replica.GatewayConfig) *httptest.Server {
	t.Helper()
	storeDir := t.TempDir()
	makeRepoInStore(t, storeDir, "acme", "demo")
	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	srv, err := NewServer(store, Options{
		MirrorDir: t.TempDir(),
		Version:   "test",
		AuthStore: newPermissiveAuthStore(t, "acme", "demo"),
		Replica:   cfg,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

func TestReplicaRefusesReceivePack(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	ts := newReplicaServer(t, &replica.GatewayConfig{WriteRegionURL: "https://gw-us.example"})

	// info/refs?service=git-receive-pack
	req, _ := http.NewRequest("GET", ts.URL+"/acme/demo.git/info/refs?service=git-receive-pack", nil)
	req.SetBasicAuth("perm", "perm")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET info/refs: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("info/refs receive-pack: status=%d want 403; body=%q", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "read-only replica") || !strings.Contains(string(body), "https://gw-us.example") {
		t.Fatalf("info/refs receive-pack body missing refusal markers: %q", body)
	}

	// POST git-receive-pack
	preq, _ := http.NewRequest("POST", ts.URL+"/acme/demo.git/git-receive-pack", strings.NewReader(""))
	preq.SetBasicAuth("perm", "perm")
	preq.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	presp, err := http.DefaultClient.Do(preq)
	if err != nil {
		t.Fatalf("POST receive-pack: %v", err)
	}
	pbody, _ := io.ReadAll(presp.Body)
	presp.Body.Close()
	if presp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST receive-pack: status=%d want 403; body=%q", presp.StatusCode, pbody)
	}
	if !strings.Contains(string(pbody), "read-only replica") || !strings.Contains(string(pbody), "https://gw-us.example") {
		t.Fatalf("POST receive-pack body missing refusal markers: %q", pbody)
	}
}

func TestReplicaGateBlocksAdvertise(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	gate := gateFunc(func(_ context.Context, tenant, repo string) error {
		return &replica.UnhealthyError{Tenant: tenant, Repo: repo, Reason: "lag budget exceeded"}
	})
	ts := newReplicaServer(t, &replica.GatewayConfig{WriteRegionURL: "https://gw-us.example", Gate: gate})

	// GET info/refs?service=git-upload-pack → 503
	req, _ := http.NewRequest("GET", ts.URL+"/acme/demo.git/info/refs?service=git-upload-pack", nil)
	req.SetBasicAuth("perm", "perm")
	req.Header.Set("Git-Protocol", "version=2")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET info/refs: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("info/refs upload-pack: status=%d want 503; body=%q", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "replica unhealthy") {
		t.Fatalf("info/refs upload-pack body missing 'replica unhealthy': %q", body)
	}

	// POST git-upload-pack → 503
	preq, _ := http.NewRequest("POST", ts.URL+"/acme/demo.git/git-upload-pack", strings.NewReader(""))
	preq.SetBasicAuth("perm", "perm")
	preq.Header.Set("Git-Protocol", "version=2")
	preq.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	presp, err := http.DefaultClient.Do(preq)
	if err != nil {
		t.Fatalf("POST upload-pack: %v", err)
	}
	pbody, _ := io.ReadAll(presp.Body)
	presp.Body.Close()
	if presp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("POST upload-pack: status=%d want 503; body=%q", presp.StatusCode, pbody)
	}
	if !strings.Contains(string(pbody), "replica unhealthy") {
		t.Fatalf("POST upload-pack body missing 'replica unhealthy': %q", pbody)
	}

	// Plain GET /healthz → still 200 "ok"
	hresp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	hbody, _ := io.ReadAll(hresp.Body)
	hresp.Body.Close()
	if hresp.StatusCode != http.StatusOK || !strings.Contains(string(hbody), "ok") {
		t.Fatalf("/healthz: status=%d body=%q want 200 ok", hresp.StatusCode, hbody)
	}
}

func TestReplicaGateAllowsWhenHealthy(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	gate := gateFunc(func(_ context.Context, _, _ string) error { return nil })
	ts := newReplicaServer(t, &replica.GatewayConfig{WriteRegionURL: "https://gw-us.example", Gate: gate})

	req, _ := http.NewRequest("GET", ts.URL+"/acme/demo.git/info/refs?service=git-upload-pack", nil)
	req.SetBasicAuth("perm", "perm")
	req.Header.Set("Git-Protocol", "version=2")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET info/refs: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthy gate: status=%d want 200; body=%q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/x-git-upload-pack-advertisement" {
		t.Fatalf("Content-Type: %q", got)
	}
	if !strings.Contains(string(body), "version 2") {
		t.Fatalf("body missing 'version 2': %q", body)
	}
}

func TestHealthzReplica(t *testing.T) {
	want := replica.HealthSnapshot{
		Role:               "replica",
		Mode:               "bounded-stale",
		ReposTracked:       7,
		ReposLagging:       2,
		MaxLagSeconds:      12.5,
		CanonicalReachable: true,
	}
	ts := newReplicaServer(t, &replica.GatewayConfig{
		WriteRegionURL: "https://gw-us.example",
		Health:         func() replica.HealthSnapshot { return want },
	})

	resp, err := http.Get(ts.URL + "/healthz/replica")
	if err != nil {
		t.Fatalf("GET /healthz/replica: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type=%q want application/json", ct)
	}
	var got replica.HealthSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != want {
		t.Fatalf("snapshot round-trip mismatch: got=%+v want=%+v", got, want)
	}
}

// TestReplicaRefusesProxiedLFSUpload proves the wiring through NewServer:
// a replica server with proxied LFS enabled mounts /_lfs/ and refuses an
// upload PUT (with a valid HMAC token) with a clean 403, not a 500 from
// the read-only storage backend.
func TestReplicaRefusesProxiedLFSUpload(t *testing.T) {
	key := bytes.Repeat([]byte{0xcd}, 32)
	storeDir := t.TempDir()
	makeRepoInStore(t, storeDir, "acme", "demo")
	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	srv, err := NewServer(store, Options{
		MirrorDir:               t.TempDir(),
		Version:                 "test",
		AuthStore:               newPermissiveAuthStore(t, "acme", "demo"),
		Replica:                 &replica.GatewayConfig{WriteRegionURL: "https://gw-us.example"},
		LFSEnabled:              true,
		LFSProxiedURLSigningKey: key,
		LFSProxiedBaseURL:       "https://replica.example",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	payload := []byte("replica lfs upload payload")
	sum := sha256.Sum256(payload)
	oid := hex.EncodeToString(sum[:])
	tok, err := proxiedurl.Mint(key, "lfs-put", "acme/demo/"+oid, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/_lfs/acme/demo/"+oid+"?token="+tok, bytes.NewReader(payload))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT /_lfs/: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("PUT /_lfs/: status=%d want 403; body=%q", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "read-only replica") || !strings.Contains(string(body), "https://gw-us.example") {
		t.Fatalf("PUT /_lfs/ body missing refusal markers: %q", body)
	}
}

func TestHealthzReplica_NotMountedWithoutReplica(t *testing.T) {
	storeDir := t.TempDir()
	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	srv, err := NewServer(store, Options{
		MirrorDir: t.TempDir(),
		Version:   "test",
		AuthStore: newAnonymousTestAuthStore(t, "acme", "demo", true),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/healthz/replica")
	if err != nil {
		t.Fatalf("GET /healthz/replica: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("non-replica /healthz/replica: status=%d want 404", resp.StatusCode)
	}
}
