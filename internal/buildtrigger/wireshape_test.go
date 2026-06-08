package buildtrigger

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
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

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/codebuild"
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

type sbWire struct {
	ProjectName                  string `json:"projectName"`
	SourceVersion                string `json:"sourceVersion"`
	EnvironmentVariablesOverride []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
		Type  string `json:"type"`
	} `json:"environmentVariablesOverride"`
}

type sbCapture struct {
	req    sbWire
	target string
	auth   string
}

func TestWireShape_CodeBuild(t *testing.T) {
	recv := make(chan sbCapture, 1)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req sbWire
		_ = json.Unmarshal(b, &req)
		recv <- sbCapture{req: req, target: r.Header.Get("X-Amz-Target"), auth: r.Header.Get("Authorization")}
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		_, _ = w.Write([]byte(`{"build":{"id":"b-1"}}`))
	}))
	defer fake.Close()

	clientFor := func(Trigger) (startBuildAPI, error) {
		cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
			awsconfig.WithRegion("us-east-1"),
			awsconfig.WithCredentialsProvider(
				credentials.NewStaticCredentialsProvider("AKIATEST", "secret", "")))
		if err != nil {
			return nil, err
		}
		return codebuild.NewFromConfig(cfg, func(o *codebuild.Options) {
			o.BaseEndpoint = aws.String(fake.URL)
		}), nil
	}

	svc, _ := newTestSvc(t)
	if _, err := svc.Create(context.Background(), TriggerInput{
		Tenant: "acme", Repo: "app", Name: "cbld", Kind: KindCodeBuild,
		Config:     Config{AWSRegion: "us-east-1", AWSProject: "app-release"},
		RefInclude: []string{"refs/heads/main"}, TokenMode: TokenInject,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	d := &codeBuildDeliverer{clientFor: clientFor, mintFn: fixedMint}
	runWorkerOnce(t, svc, map[Kind]Deliverer{KindCodeBuild: d})

	var got sbCapture
	select {
	case got = <-recv:
	case <-time.After(3 * time.Second):
		t.Fatal("no StartBuild received within 3s — worker did not deliver")
	}

	if got.auth == "" {
		t.Error("missing Authorization header — request was not SigV4-signed")
	}
	if !strings.HasSuffix(got.target, ".StartBuild") {
		t.Errorf("X-Amz-Target=%q, want a *.StartBuild target", got.target)
	}
	if got.req.ProjectName != "app-release" {
		t.Errorf("projectName=%q, want app-release", got.req.ProjectName)
	}
	if got.req.SourceVersion != wsHeadOID {
		t.Errorf("sourceVersion=%q, want %s", got.req.SourceVersion, wsHeadOID)
	}
	wantEnv := map[string]string{
		"BV_REF":     "refs/heads/main",
		"BV_REPO":    "acme/app",
		"BV_COMMIT":  wsHeadOID,
		"BVTS_TOKEN": "bvts_TESTTOKEN",
	}
	gotEnv := map[string]string{}
	for _, ev := range got.req.EnvironmentVariablesOverride {
		gotEnv[ev.Name] = ev.Value
		if ev.Type != "PLAINTEXT" {
			t.Errorf("env %s type=%q, want PLAINTEXT", ev.Name, ev.Type)
		}
	}
	for k, v := range wantEnv {
		if gotEnv[k] != v {
			t.Errorf("env %s=%q, want %q", k, gotEnv[k], v)
		}
	}
}

func TestWireShape_AzureWebhook(t *testing.T) {
	recv := make(chan capturedHTTP, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		recv <- capturedHTTP{method: r.Method, path: r.URL.Path, headers: r.Header.Clone(), body: b}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	const secret = "azure-shared-secret"
	svc, _ := newTestSvc(t)
	tr, err := svc.Create(context.Background(), TriggerInput{
		Tenant: "acme", Repo: "app", Name: "aw", Kind: KindAzureWebhook,
		Config:     Config{AzureWebhookURL: srv.URL, Secret: secret},
		RefInclude: []string{"refs/heads/main"}, TokenMode: TokenInject,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	d := &httpDeliverer{client: srv.Client(), mintFn: fixedMint}
	runWorkerOnce(t, svc, map[Kind]Deliverer{KindAzureWebhook: d})

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
	// Azure default signature header, HMAC-SHA1 over the raw body (no timestamp).
	wantSig := signAzureSHA1(tr.Secret, got.body, 0)
	if sig := got.headers.Get("X-Hub-Signature"); sig != wantSig {
		t.Errorf("X-Hub-Signature=%q, want %q", sig, wantSig)
	}
	assertGolden(t, "azurewebhook_body.golden.json", got.body)
}

func TestWireShape_AzureWebhook_CustomHeaderAndUnsigned(t *testing.T) {
	// Custom header when a secret is set.
	recv := make(chan capturedHTTP, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		recv <- capturedHTTP{headers: r.Header.Clone(), body: b}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	svc, _ := newTestSvc(t)
	tr, err := svc.Create(context.Background(), TriggerInput{
		Tenant: "acme", Repo: "app", Name: "awc", Kind: KindAzureWebhook,
		Config:     Config{AzureWebhookURL: srv.URL, Secret: "s", AzureSigHeader: "X-Custom-Sig"},
		RefInclude: []string{"refs/heads/main"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	d := &httpDeliverer{client: srv.Client(), mintFn: fixedMint}
	runWorkerOnce(t, svc, map[Kind]Deliverer{KindAzureWebhook: d})
	got := <-recv
	if got.headers.Get("X-Custom-Sig") == "" {
		t.Error("custom header X-Custom-Sig not set")
	}
	if got.headers.Get("X-Hub-Signature") != "" {
		t.Error("default X-Hub-Signature should not be set when custom header configured")
	}
	_ = tr

	// Unsigned: no secret → no signature header at all.
	recv2 := make(chan capturedHTTP, 1)
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		recv2 <- capturedHTTP{headers: r.Header.Clone(), body: b}
		w.WriteHeader(200)
	}))
	defer srv2.Close()
	svc2, _ := newTestSvc(t)
	if _, err := svc2.Create(context.Background(), TriggerInput{
		Tenant: "acme", Repo: "app", Name: "awu", Kind: KindAzureWebhook,
		Config:     Config{AzureWebhookURL: srv2.URL}, // no secret
		RefInclude: []string{"refs/heads/main"},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	d2 := &httpDeliverer{client: srv2.Client(), mintFn: fixedMint}
	runWorkerOnce(t, svc2, map[Kind]Deliverer{KindAzureWebhook: d2})
	got2 := <-recv2
	if got2.headers.Get("X-Hub-Signature") != "" || got2.headers.Get("X-Custom-Sig") != "" {
		t.Error("unsigned azurewebhook must send no signature header")
	}
}

func TestWireShape_AzurePipelines(t *testing.T) {
	recv := make(chan capturedHTTP, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		recv <- capturedHTTP{method: r.Method, path: r.URL.Path + "?" + r.URL.RawQuery, headers: r.Header.Clone(), body: b}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":123,"state":"inProgress"}`))
	}))
	defer srv.Close()

	clientFor := func(Trigger) (azureConn, error) {
		return azureConn{orgURL: srv.URL, pat: "testpat", client: srv.Client()}, nil
	}

	svc, _ := newTestSvc(t)
	if _, err := svc.Create(context.Background(), TriggerInput{
		Tenant: "acme", Repo: "app", Name: "ap", Kind: KindAzurePipelines,
		Config:     Config{AzureConnector: "prod", AzureProject: "MyProject", AzurePipelineID: 42},
		RefInclude: []string{"refs/heads/main"}, TokenMode: TokenInject,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	d := &azurePipelinesDeliverer{clientFor: clientFor, mintFn: fixedMint}
	runWorkerOnce(t, svc, map[Kind]Deliverer{KindAzurePipelines: d})

	var got capturedHTTP
	select {
	case got = <-recv:
	case <-time.After(3 * time.Second):
		t.Fatal("no request received within 3s — worker did not deliver")
	}
	if got.method != http.MethodPost {
		t.Errorf("method=%s, want POST", got.method)
	}
	if got.path != "/MyProject/_apis/pipelines/42/runs?api-version=7.1" {
		t.Errorf("path=%q, want /MyProject/_apis/pipelines/42/runs?api-version=7.1", got.path)
	}
	// Basic auth: empty username + PAT as password.
	user, pass, ok := parseBasicAuth(got.headers.Get("Authorization"))
	if !ok || user != "" || pass != "testpat" {
		t.Errorf("Authorization basic user=%q pass=%q ok=%v, want user=\"\" pass=\"testpat\"", user, pass, ok)
	}
	assertGolden(t, "azurepipelines_body.golden.json", got.body)
}

// parseBasicAuth decodes a "Basic base64(user:pass)" header for assertions.
func parseBasicAuth(h string) (user, pass string, ok bool) {
	const prefix = "Basic "
	if !strings.HasPrefix(h, prefix) {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(h[len(prefix):])
	if err != nil {
		return "", "", false
	}
	u, p, found := strings.Cut(string(raw), ":")
	return u, p, found
}
