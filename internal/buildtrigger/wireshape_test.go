package buildtrigger

import (
	"bytes"
	"context"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

// -update rewrites the *.golden.json testdata from captured bodies:
//
//	go test ./internal/buildtrigger/ -run WireShape -update
var updateGolden = flag.Bool("update", false, "rewrite *.golden.json testdata")

const wsHeadOID = "1111111111111111111111111111111111111111"
const wsZeroOID = "0000000000000000000000000000000000000000"

func fixedPush() PushInfo {
	return PushInfo{
		Tenant: "acme", Repo: "app", Actor: "tester", TxID: "tx-test", HeadOID: wsHeadOID,
		RefUpdates: []RefUpdate{{Refname: "refs/heads/main", OldOID: wsZeroOID, NewOID: wsHeadOID}},
	}
}

func fixedMint(context.Context, Trigger, BuildPayload) (string, error) { return "bvts_TESTTOKEN", nil }

type capturedHTTP struct {
	method, path string
	headers      http.Header
	body         []byte
}

func runWorkerOnce(t *testing.T, svc *Service, deliverers map[Kind]Deliverer) {
	t.Helper()
	ctx := context.Background()
	if err := svc.Enqueue(ctx, fixedPush()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	cfg := WorkerConfig{
		TickInterval:    5 * time.Millisecond,
		ClaimBatchSize:  8,
		BackoffSchedule: []time.Duration{5 * time.Millisecond},
		Deliverers:      deliverers,
	}
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	go StartWorker(wctx, svc, cfg)
	waitUntil(t, 3*time.Second, func() bool { return svc.countByStatus(ctx, "delivered") == 1 })
}

func assertSig(t *testing.T, secret, header string, body []byte) {
	t.Helper()
	parts := strings.SplitN(header, ",", 2)
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "t=") {
		t.Fatalf("malformed signature header %q", header)
	}
	tsec, err := strconv.ParseInt(strings.TrimPrefix(parts[0], "t="), 10, 64)
	if err != nil {
		t.Fatalf("bad t in signature %q: %v", header, err)
	}
	if want := webhooks.Sign(secret, tsec, body); want != header {
		t.Fatalf("signature mismatch:\n got: %s\nwant: %s", header, want)
	}
}

func assertGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", name, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run -update to create)", name, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("body mismatch for %s:\n got: %s\nwant: %s", name, got, want)
	}
}

func TestWireShape_Generic(t *testing.T) {
	recv := make(chan capturedHTTP, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		recv <- capturedHTTP{method: r.Method, path: r.URL.Path, headers: r.Header.Clone(), body: b}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	svc, _ := newTestSvc(t)
	tr, err := svc.Create(context.Background(), TriggerInput{
		Tenant: "acme", Repo: "app", Name: "g", Kind: KindGeneric,
		Config: Config{URL: srv.URL}, RefInclude: []string{"refs/heads/main"},
		TokenMode: TokenInject,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	d := &httpDeliverer{client: srv.Client(), mintFn: fixedMint}
	runWorkerOnce(t, svc, map[Kind]Deliverer{KindGeneric: d})

	var got capturedHTTP
	select {
	case got = <-recv:
	case <-time.After(3 * time.Second):
		t.Fatal("no request received within 3s — worker did not deliver")
	}
	if got.method != http.MethodPost || got.path != "/" {
		t.Fatalf("got %s %s, want POST /", got.method, got.path)
	}
	if ct := got.headers.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type=%q, want application/json", ct)
	}
	if ua := got.headers.Get("User-Agent"); ua != "bucketvcs-buildtrigger/1" {
		t.Errorf("User-Agent=%q, want bucketvcs-buildtrigger/1", ua)
	}
	assertSig(t, tr.Secret, got.headers.Get("BucketVCS-Signature"), got.body)
	assertGolden(t, "generic_body.golden.json", got.body)
}

func TestWireShape_CloudBuild(t *testing.T) {
	recv := make(chan capturedHTTP, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		recv <- capturedHTTP{method: r.Method, path: r.URL.Path, headers: r.Header.Clone(), body: b}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	svc, _ := newTestSvc(t)
	tr, err := svc.Create(context.Background(), TriggerInput{
		Tenant: "acme", Repo: "app", Name: "cb", Kind: KindCloudBuild,
		Config: Config{URL: srv.URL}, RefInclude: []string{"refs/heads/main"},
		TokenMode: TokenInject,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	d := &httpDeliverer{client: srv.Client(), mintFn: fixedMint}
	runWorkerOnce(t, svc, map[Kind]Deliverer{KindCloudBuild: d})

	var got capturedHTTP
	select {
	case got = <-recv:
	case <-time.After(3 * time.Second):
		t.Fatal("no request received within 3s — worker did not deliver")
	}
	assertSig(t, tr.Secret, got.headers.Get("BucketVCS-Signature"), got.body)
	assertGolden(t, "cloudbuild_body.golden.json", got.body)

	if !bytes.Contains(got.body, []byte(`"ref":"refs/heads/main"`)) {
		t.Errorf("cloudbuild body missing flattened ref: %s", got.body)
	}
	if !bytes.Contains(got.body, []byte(`"commit":"`+wsHeadOID+`"`)) {
		t.Errorf("cloudbuild body missing flattened commit: %s", got.body)
	}
}
