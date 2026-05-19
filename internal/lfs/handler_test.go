package lfs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// testActorKey is the context key used by the handler tests to inject
// an *auth.Actor before invoking the LFS handler — mirroring how the
// gateway path attaches the actor after RunAuth via
// gateway.ActorFromContext. T1.6 removed in-handler Basic-auth
// parsing; the handler now reads actor exclusively from context.
type testActorKey struct{}

func contextWithActor(ctx context.Context, a *auth.Actor) context.Context {
	return context.WithValue(ctx, testActorKey{}, a)
}

func actorFromTestContext(ctx context.Context) *auth.Actor {
	v, _ := ctx.Value(testActorKey{}).(*auth.Actor)
	return v
}

// fakeAuth implements auth.Store. The LFS handler only consults
// GetRepoFlags and LookupRepoPerm (for the secondary write check on
// upload); the rest are zero-value stubs so the type satisfies the
// interface. VerifyCredential is retained as a stub for compliance
// with the interface but is no longer exercised by the handler — the
// gateway's RunAuth handles credential verification.
type fakeAuth struct {
	repoPerm map[string]auth.Perm   // "tenant/repo" -> perm granted to known actor
	actors   map[string]*auth.Actor // password -> actor
}

func (f *fakeAuth) GetRepoFlags(_ context.Context, tenant, repo string) (auth.RepoFlags, error) {
	if _, ok := f.repoPerm[tenant+"/"+repo]; !ok {
		return auth.RepoFlags{}, auth.ErrNoSuchRepo
	}
	return auth.RepoFlags{}, nil
}

func (f *fakeAuth) LookupRepoPerm(_ context.Context, actor *auth.Actor, tenant, repo string) (auth.Perm, error) {
	if actor == nil {
		return auth.PermNone, nil
	}
	p := f.repoPerm[tenant+"/"+repo]
	return p, nil
}

func (f *fakeAuth) VerifyCredential(_ context.Context, c auth.Credential) (*auth.Actor, string, *auth.Scope, error) {
	bp, ok := c.(auth.BasicPassword)
	if !ok {
		return nil, "", nil, auth.ErrInvalidCredential
	}
	a, ok := f.actors[bp.Password]
	if !ok {
		return nil, "", nil, auth.ErrInvalidCredential
	}
	return a, "tok-id", nil, nil
}

// The remaining auth.Store methods are stubs — the LFS handler never
// calls them.
func (f *fakeAuth) TouchTokenUsage(context.Context, string) error                            { return nil }
func (f *fakeAuth) AddSSHKey(context.Context, auth.SSHKey) error                             { return nil }
func (f *fakeAuth) ListSSHKeysForUser(context.Context, string) ([]auth.SSHKey, error)        { return nil, nil }
func (f *fakeAuth) ListSSHKeysForRepo(_ context.Context, _, _ string) ([]auth.SSHKey, error) { return nil, nil }
func (f *fakeAuth) RevokeSSHKey(context.Context, string) error                               { return nil }
func (f *fakeAuth) TouchSSHKeyUsage(context.Context, string) error                           { return nil }
func (f *fakeAuth) GetUserByName(context.Context, string) (*auth.User, error)                { return nil, nil }
func (f *fakeAuth) Close() error                                                             { return nil }

// newHandlerForTest stands up an httptest server with a handler-only mux
// mounting LFS at the same path the gateway will use. The given actor
// (nil for anonymous) is injected into the request context by the
// wrapping middleware — emulating the gateway path where RunAuth
// attaches the actor before routeRepo dispatches to the LFS handler.
func newHandlerForTest(t *testing.T, store *Store, authStore *fakeAuth, actor *auth.Actor) *httptest.Server {
	t.Helper()
	lfsH := NewHTTPHandler(Deps{
		AuthStore:        authStore,
		ActorFromContext: actorFromTestContext,
		NewStore:         func(tenant, repo string) *Store { return store },
		PresignTTL:       5 * time.Minute,
		Logger:           captureLogger(&bytes.Buffer{}),
	})
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(contextWithActor(r.Context(), actor))
		lfsH.ServeHTTP(w, r)
	})
	return httptest.NewServer(wrapped)
}

func TestHandler_Batch_UploadHappyPath(t *testing.T) {
	store := newBatchStore(nil, signedFn())
	authStore := &fakeAuth{
		repoPerm: map[string]auth.Perm{"acme/foo": auth.PermWrite},
		actors:   map[string]*auth.Actor{"pw": {Name: "alice"}},
	}
	actor := &auth.Actor{Name: "alice"}
	srv := newHandlerForTest(t, store, authStore, actor)
	defer srv.Close()

	body, _ := json.Marshal(BatchRequest{
		Operation: "upload",
		Transfers: []string{"basic"},
		Objects:   []ObjectRef{{OID: oidNew, Size: 1}},
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/acme/foo.git/info/lfs/objects/batch", bytes.NewReader(body))
	req.Header.Set("Content-Type", ContentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != ContentType {
		t.Errorf("Content-Type=%q want %q", ct, ContentType)
	}
	var got BatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Transfer != "basic" || len(got.Objects) != 1 {
		t.Fatalf("got=%+v", got)
	}
	if _, ok := got.Objects[0].Actions["upload"]; !ok {
		t.Error("upload action missing")
	}
}

// TestHandler_Batch_Unauthorized_AnonymousUpload covers the only 401
// path the LFS handler is still responsible for after T1.6: an
// anonymous actor (nil from ActorFromContext) attempting an upload
// against a non-public-write repo. The "no credentials" 401 from
// missing Basic is now RunAuth's responsibility, not the LFS
// handler's.
func TestHandler_Batch_Unauthorized_AnonymousUpload(t *testing.T) {
	store := newBatchStore(nil, signedFn())
	authStore := &fakeAuth{
		repoPerm: map[string]auth.Perm{"acme/foo": auth.PermWrite},
		actors:   map[string]*auth.Actor{"pw": {Name: "alice"}},
	}
	srv := newHandlerForTest(t, store, authStore, nil)
	defer srv.Close()

	body, _ := json.Marshal(BatchRequest{
		Operation: "upload",
		Transfers: []string{"basic"},
		Objects:   []ObjectRef{{OID: oidNew, Size: 1}},
	})
	resp, err := http.Post(srv.URL+"/acme/foo.git/info/lfs/objects/batch", ContentType, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestHandler_Batch_Forbidden_ReadOnlyUserUploads(t *testing.T) {
	// Actor has PermRead. Upload must be 403.
	store := newBatchStore(nil, signedFn())
	authStore := &fakeAuth{
		repoPerm: map[string]auth.Perm{"acme/foo": auth.PermRead},
		actors:   map[string]*auth.Actor{"pw": {Name: "alice"}},
	}
	actor := &auth.Actor{Name: "alice"}
	srv := newHandlerForTest(t, store, authStore, actor)
	defer srv.Close()

	body, _ := json.Marshal(BatchRequest{
		Operation: "upload",
		Transfers: []string{"basic"},
		Objects:   []ObjectRef{{OID: oidNew, Size: 1}},
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/acme/foo.git/info/lfs/objects/batch", bytes.NewReader(body))
	req.Header.Set("Content-Type", ContentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

// 404 for missing repo is now RunAuth's responsibility (see
// gateway.RunAuth). The LFS handler never sees requests for missing
// repos in the gateway path; no in-handler test for it.

func TestHandler_Batch_Unprocessable_MalformedBody(t *testing.T) {
	store := newBatchStore(nil, signedFn())
	authStore := &fakeAuth{
		repoPerm: map[string]auth.Perm{"acme/foo": auth.PermWrite},
		actors:   map[string]*auth.Actor{"pw": {Name: "alice"}},
	}
	actor := &auth.Actor{Name: "alice"}
	srv := newHandlerForTest(t, store, authStore, actor)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/acme/foo.git/info/lfs/objects/batch", strings.NewReader("{not-json"))
	req.Header.Set("Content-Type", ContentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 422 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestHandler_Batch_VerifyActionUsesInboundAuthHeader(t *testing.T) {
	// The verify action's Authorization header should mirror whatever
	// the inbound request carried. Since the handler no longer
	// Basic-auths internally, we set a synthetic Authorization header
	// directly and assert verify echoes that exact string.
	store := newBatchStore(nil, signedFn())
	authStore := &fakeAuth{
		repoPerm: map[string]auth.Perm{"acme/foo": auth.PermWrite},
		actors:   map[string]*auth.Actor{"pw": {Name: "alice"}},
	}
	actor := &auth.Actor{Name: "alice"}
	srv := newHandlerForTest(t, store, authStore, actor)
	defer srv.Close()

	body, _ := json.Marshal(BatchRequest{
		Operation: "upload",
		Transfers: []string{"basic"},
		Objects:   []ObjectRef{{OID: oidNew, Size: 1}},
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/acme/foo.git/info/lfs/objects/batch", bytes.NewReader(body))
	req.Header.Set("Content-Type", ContentType)
	req.Header.Set("Authorization", "Bearer test-bearer")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var got BatchResponse
	_ = json.NewDecoder(resp.Body).Decode(&got)
	vf := got.Objects[0].Actions["verify"]
	if vf.Header["Authorization"] != "Bearer test-bearer" {
		t.Errorf("verify Authorization=%q want %q", vf.Header["Authorization"], "Bearer test-bearer")
	}
}

// TestHandler_Batch_UnsupportedMediaType covers the LFS-spec
// Content-Type requirement: a non-vendor Content-Type should fail
// fast with 415 before the body is parsed.
func TestHandler_Batch_UnsupportedMediaType(t *testing.T) {
	store := newBatchStore(nil, signedFn())
	authStore := &fakeAuth{repoPerm: map[string]auth.Perm{"acme/foo": auth.PermWrite}}
	srv := newHandlerForTest(t, store, authStore, &auth.Actor{Name: "alice"})
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/acme/foo.git/info/lfs/objects/batch", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status=%d, want 415", resp.StatusCode)
	}
}

// TestNewHTTPHandler_PanicsOnNilAuthStore asserts construction-time
// validation of required deps — a missing AuthStore is a wire-up
// programmer error, not a request-time failure.
func TestNewHTTPHandler_PanicsOnNilAuthStore(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil AuthStore")
		}
	}()
	NewHTTPHandler(Deps{NewStore: func(string, string) *Store { return nil }})
}

// TestNewHTTPHandler_PanicsOnNilNewStore is the companion check for
// the second required dep.
func TestNewHTTPHandler_PanicsOnNilNewStore(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil NewStore")
		}
	}()
	NewHTTPHandler(Deps{AuthStore: &fakeAuth{}})
}

// TestHandler_Batch_RequestBodyTooLarge asserts the 1 MiB MaxBytesReader
// cap surfaces as 413 (RequestEntityTooLarge) rather than a generic
// 422 — clients hitting the cap need a distinct signal.
func TestHandler_Batch_RequestBodyTooLarge(t *testing.T) {
	store := newBatchStore(nil, signedFn())
	authStore := &fakeAuth{repoPerm: map[string]auth.Perm{"acme/foo": auth.PermWrite}}
	srv := newHandlerForTest(t, store, authStore, &auth.Actor{Name: "alice"})
	defer srv.Close()

	// Build a > 1 MiB syntactically-valid JSON body so the decoder
	// reads past the MaxBytesReader cap (a body of all 'a' bytes would
	// fail JSON syntax before the cap was hit).
	var sb strings.Builder
	sb.WriteString(`{"operation":"upload","transfers":["basic"],"objects":[`)
	// Each object record is ~80 bytes; 20000 of them is ~1.6 MiB.
	for i := 0; i < 20000; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"oid":"0000000000000000000000000000000000000000000000000000000000000000","size":1}`)
	}
	sb.WriteString(`]}`)
	body := strings.NewReader(sb.String())
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/acme/foo.git/info/lfs/objects/batch", body)
	req.Header.Set("Content-Type", ContentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d, want 413", resp.StatusCode)
	}
}

// TestParseLFSPath_RejectsAdversarialNames covers the validRouteName
// guard in parseLFSPath: tenant/repo segments must match the canonical
// routenames.ValidateName character set [A-Za-z0-9._-], which rejects
// path separators, control chars, and non-ASCII. Leading dots and
// dot-sequences like ".." are syntactically valid names and are not
// rejected by the validator — namespace escape would require a Path
// separator (/), which routenames.ValidateName rejects.
func TestParseLFSPath_RejectsAdversarialNames(t *testing.T) {
	cases := []struct {
		path     string
		wantRoute lfsRoute
		reason   string
	}{
		{"/acme/..git/info/lfs/objects/batch", lfsRouteBatch, "..git is syntactically valid per routenames.ValidateName"},
		{"/../acme.git/info/lfs/objects/batch", lfsRouteNone, "tenant is '..' but path is not clean (/../)"},
		{"/./acme.git/info/lfs/objects/batch", lfsRouteNone, "tenant is '.' but path is not clean (/./)"},
		{"/acme/.hidden.git/info/lfs/objects/batch", lfsRouteBatch, ".hidden.git is syntactically valid per routenames.ValidateName"},
		{"/acme/foo.bar.git/info/lfs/objects/batch", lfsRouteBatch, "valid sanity-pin"},
		{"/acme/foo/../bar.git/info/lfs/objects/batch", lfsRouteNone, "path not clean: foo/../bar is traversal"},
	}
	for _, c := range cases {
		_, _, _, got := parseLFSPath(c.path)
		if got != c.wantRoute {
			t.Errorf("%q (%s): route=%v want %v", c.path, c.reason, got, c.wantRoute)
		}
	}
}

func TestHandler_Batch_RejectsExcessiveObjectCount(t *testing.T) {
	store := newBatchStore(nil, signedFn())
	authStore := &fakeAuth{repoPerm: map[string]auth.Perm{"acme/foo": auth.PermWrite}}
	srv := newHandlerForTest(t, store, authStore, &auth.Actor{Name: "alice"})
	defer srv.Close()

	objs := make([]ObjectRef, 1001)
	for i := range objs {
		objs[i] = ObjectRef{OID: fmt.Sprintf("%064x", i), Size: 1}
	}
	body, _ := json.Marshal(BatchRequest{
		Operation: "upload",
		Transfers: []string{"basic"},
		Objects:   objs,
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/acme/foo.git/info/lfs/objects/batch", bytes.NewReader(body))
	req.Header.Set("Content-Type", ContentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d, want 422", resp.StatusCode)
	}
}

func TestHandler_Verify_OK(t *testing.T) {
	oid := strings.Repeat("a", 64)
	store := newBatchStore(map[string]int64{oid: 100}, signedFn())
	authStore := &fakeAuth{repoPerm: map[string]auth.Perm{"acme/foo": auth.PermWrite}}
	srv := newHandlerForTest(t, store, authStore, &auth.Actor{Name: "alice"})
	defer srv.Close()

	body, _ := json.Marshal(VerifyRequest{OID: oid, Size: 100})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/acme/foo.git/info/lfs/objects/"+oid+"/verify", bytes.NewReader(body))
	req.Header.Set("Content-Type", ContentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestHandler_Verify_SizeMismatch(t *testing.T) {
	oid := strings.Repeat("a", 64)
	store := newBatchStore(map[string]int64{oid: 100}, signedFn())
	authStore := &fakeAuth{repoPerm: map[string]auth.Perm{"acme/foo": auth.PermWrite}}
	srv := newHandlerForTest(t, store, authStore, &auth.Actor{Name: "alice"})
	defer srv.Close()

	body, _ := json.Marshal(VerifyRequest{OID: oid, Size: 999})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/acme/foo.git/info/lfs/objects/"+oid+"/verify", bytes.NewReader(body))
	req.Header.Set("Content-Type", ContentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 422 {
		t.Fatalf("status=%d, want 422", resp.StatusCode)
	}
}

func TestHandler_Verify_NotFound(t *testing.T) {
	oid := strings.Repeat("a", 64)
	store := newBatchStore(nil, signedFn())
	authStore := &fakeAuth{repoPerm: map[string]auth.Perm{"acme/foo": auth.PermWrite}}
	srv := newHandlerForTest(t, store, authStore, &auth.Actor{Name: "alice"})
	defer srv.Close()

	body, _ := json.Marshal(VerifyRequest{OID: oid, Size: 100})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/acme/foo.git/info/lfs/objects/"+oid+"/verify", bytes.NewReader(body))
	req.Header.Set("Content-Type", ContentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("status=%d, want 404", resp.StatusCode)
	}
}

func TestHandler_Verify_BodyOIDMismatch(t *testing.T) {
	urlOID := strings.Repeat("a", 64)
	bodyOID := strings.Repeat("b", 64)
	store := newBatchStore(map[string]int64{urlOID: 100}, signedFn())
	authStore := &fakeAuth{repoPerm: map[string]auth.Perm{"acme/foo": auth.PermWrite}}
	srv := newHandlerForTest(t, store, authStore, &auth.Actor{Name: "alice"})
	defer srv.Close()

	body, _ := json.Marshal(VerifyRequest{OID: bodyOID, Size: 100})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/acme/foo.git/info/lfs/objects/"+urlOID+"/verify", bytes.NewReader(body))
	req.Header.Set("Content-Type", ContentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 422 {
		t.Fatalf("status=%d, want 422", resp.StatusCode)
	}
}

func TestHandler_Verify_GET_Returns404(t *testing.T) {
	// GET on /verify is not a recognized route.
	oid := strings.Repeat("a", 64)
	store := newBatchStore(nil, signedFn())
	authStore := &fakeAuth{repoPerm: map[string]auth.Perm{"acme/foo": auth.PermWrite}}
	srv := newHandlerForTest(t, store, authStore, &auth.Actor{Name: "alice"})
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/acme/foo.git/info/lfs/objects/" + oid + "/verify")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("status=%d, want 404", resp.StatusCode)
	}
}

func TestHandler_Verify_RejectsMismatchedContentType(t *testing.T) {
	// LFS spec mandates application/vnd.git-lfs+json; reject text/plain.
	oid := strings.Repeat("a", 64)
	store := newBatchStore(map[string]int64{oid: 100}, signedFn())
	authStore := &fakeAuth{repoPerm: map[string]auth.Perm{"acme/foo": auth.PermWrite}}
	srv := newHandlerForTest(t, store, authStore, &auth.Actor{Name: "alice"})
	defer srv.Close()

	body, _ := json.Marshal(VerifyRequest{OID: oid, Size: 100})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/acme/foo.git/info/lfs/objects/"+oid+"/verify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status=%d, want 415", resp.StatusCode)
	}
}
