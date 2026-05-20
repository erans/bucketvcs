package lfs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/lfs"
	"github.com/bucketvcs/bucketvcs/internal/lfs/locks"
)

// locksTestHarness wires a fresh in-memory authdb + LFS handler with
// the actor returned by actorFn (which the tests can swap mid-flight
// to simulate "alice creates, bob unlocks"). The harness exposes the
// underlying locks.Store so tests can seed directly without going
// through the HTTP surface.
type locksTestHarness struct {
	handler http.Handler
	authdb  *sqlitestore.Store
	store   *locks.Store
	actor   *auth.Actor // mutable; closure below reads this
}

func newLocksHarness(t *testing.T) *locksTestHarness {
	t.Helper()
	authdb, err := sqlitestore.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlitestore.Open: %v", err)
	}
	t.Cleanup(func() { _ = authdb.Close() })

	h := &locksTestHarness{
		authdb: authdb,
		store:  locks.New(authdb),
	}
	h.handler = lfs.NewHTTPHandler(lfs.Deps{
		AuthStore:        authdb,
		ActorFromContext: func(context.Context) *auth.Actor { return h.actor },
		NewStore:         func(tenant, repo string) *lfs.Store { return nil }, // unused on locks routes
		LocksStore:       h.store,
	})
	return h
}

func (h *locksTestHarness) createUser(t *testing.T, name string) *auth.Actor {
	t.Helper()
	id, err := h.authdb.CreateUser(context.Background(), name, false)
	if err != nil {
		t.Fatalf("CreateUser %q: %v", name, err)
	}
	return &auth.Actor{UserID: id, Name: name}
}

func (h *locksTestHarness) do(method, urlStr string, body any) *httptest.ResponseRecorder {
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, urlStr, rdr)
	req.Header.Set("Content-Type", lfs.ContentType)
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, req)
	return w
}

func TestLocksCreate_CreatesNewLock(t *testing.T) {
	h := newLocksHarness(t)
	h.actor = h.createUser(t, "alice")

	w := h.do(http.MethodPost,
		"http://example.com/acme/demo.git/info/lfs/locks",
		lfs.LockRequest{Path: "art/hero.psd"})

	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s want 201", w.Code, w.Body.String())
	}
	var resp lfs.LockResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	if resp.Lock.Path != "art/hero.psd" {
		t.Errorf("Path=%q", resp.Lock.Path)
	}
	if resp.Lock.Owner.Name != "alice" {
		t.Errorf("Owner.Name=%q want alice", resp.Lock.Owner.Name)
	}
	if !strings.HasPrefix(resp.Lock.ID, "lock_") {
		t.Errorf("ID=%q missing lock_ prefix", resp.Lock.ID)
	}
	// LockedAt should be UTC.
	if loc := resp.Lock.LockedAt.Location(); loc != time.UTC {
		t.Errorf("LockedAt.Location=%v want UTC", loc)
	}
}

func TestLocksCreate_DuplicateReturns409(t *testing.T) {
	h := newLocksHarness(t)
	h.actor = h.createUser(t, "alice")

	body := lfs.LockRequest{Path: "a.psd"}
	if w := h.do(http.MethodPost,
		"http://example.com/acme/demo.git/info/lfs/locks", body); w.Code != http.StatusCreated {
		t.Fatalf("first: status=%d", w.Code)
	}
	w := h.do(http.MethodPost,
		"http://example.com/acme/demo.git/info/lfs/locks", body)
	if w.Code != http.StatusConflict {
		t.Fatalf("second: status=%d body=%s want 409", w.Code, w.Body.String())
	}
	var conflict lfs.LockConflictResponse
	if err := json.Unmarshal(w.Body.Bytes(), &conflict); err != nil {
		t.Fatalf("decode conflict: %v", err)
	}
	if conflict.Lock.Path != "a.psd" {
		t.Errorf("conflict.Lock.Path=%q want a.psd", conflict.Lock.Path)
	}
	if conflict.Message == "" {
		t.Errorf("conflict.Message empty")
	}
}

func TestLocksCreate_AnonymousReturns401(t *testing.T) {
	h := newLocksHarness(t)
	// h.actor stays nil
	w := h.do(http.MethodPost,
		"http://example.com/acme/demo.git/info/lfs/locks",
		lfs.LockRequest{Path: "a.psd"})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status=%d want 401", w.Code)
	}
	if got := w.Header().Get("WWW-Authenticate"); got == "" {
		t.Errorf("WWW-Authenticate header missing")
	}
}

func TestLocksCreate_MissingPathReturns400(t *testing.T) {
	h := newLocksHarness(t)
	h.actor = h.createUser(t, "alice")
	w := h.do(http.MethodPost,
		"http://example.com/acme/demo.git/info/lfs/locks",
		lfs.LockRequest{})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", w.Code)
	}
}

func TestLocksCreate_LocksDisabledReturns503(t *testing.T) {
	authdb, err := sqlitestore.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = authdb.Close() })
	actor := &auth.Actor{UserID: "u_alice", Name: "alice"}
	handler := lfs.NewHTTPHandler(lfs.Deps{
		AuthStore:        authdb,
		ActorFromContext: func(context.Context) *auth.Actor { return actor },
		NewStore:         func(tenant, repo string) *lfs.Store { return nil },
		// LocksStore intentionally nil
	})
	body, _ := json.Marshal(lfs.LockRequest{Path: "a.psd"})
	req := httptest.NewRequest(http.MethodPost,
		"http://example.com/acme/demo.git/info/lfs/locks", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d want 503", w.Code)
	}
}

func TestLocksList_ReturnsSeededLocks(t *testing.T) {
	h := newLocksHarness(t)
	h.actor = h.createUser(t, "alice")
	// Seed two locks directly via the store so we don't go through the
	// HTTP surface (this isolates List from Create).
	for _, p := range []string{"a.psd", "b.psd"} {
		if _, err := h.store.Create(context.Background(), locks.CreateInput{
			Tenant: "acme", Repo: "demo", Path: p,
			OwnerUserID: h.actor.UserID, Now: time.Unix(1700000000, 0),
		}); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}
	w := h.do(http.MethodGet,
		"http://example.com/acme/demo.git/info/lfs/locks", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp lfs.ListLocksResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Locks) != 2 {
		t.Errorf("len(Locks)=%d want 2", len(resp.Locks))
	}
}

func TestLocksList_AnonymousReturns401(t *testing.T) {
	h := newLocksHarness(t)
	w := h.do(http.MethodGet,
		"http://example.com/acme/demo.git/info/lfs/locks", nil)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status=%d want 401", w.Code)
	}
}

func TestLocksList_BadCursorReturns400(t *testing.T) {
	h := newLocksHarness(t)
	h.actor = h.createUser(t, "alice")
	w := h.do(http.MethodGet,
		"http://example.com/acme/demo.git/info/lfs/locks?cursor=not-a-number", nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", w.Code)
	}
}

func TestLocksUnlock_OwnerSucceeds(t *testing.T) {
	h := newLocksHarness(t)
	h.actor = h.createUser(t, "alice")
	id, err := h.store.Create(context.Background(), locks.CreateInput{
		Tenant: "acme", Repo: "demo", Path: "a.psd",
		OwnerUserID: h.actor.UserID, Now: time.Unix(1700000000, 0),
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	w := h.do(http.MethodPost,
		"http://example.com/acme/demo.git/info/lfs/locks/"+id+"/unlock",
		lfs.UnlockRequest{Force: false})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	// Confirm it's gone.
	if _, err := h.store.Get(context.Background(), "acme", "demo", id); !errors.Is(err, locks.ErrNotFound) {
		t.Errorf("post-unlock Get=%v want ErrNotFound", err)
	}
}

func TestLocksUnlock_NotFoundReturns404(t *testing.T) {
	h := newLocksHarness(t)
	h.actor = h.createUser(t, "alice")
	w := h.do(http.MethodPost,
		"http://example.com/acme/demo.git/info/lfs/locks/lock_nonexistent/unlock",
		lfs.UnlockRequest{})
	if w.Code != http.StatusNotFound {
		t.Errorf("status=%d want 404", w.Code)
	}
}

func TestLocksUnlock_NonOwnerWithoutForceReturns403(t *testing.T) {
	h := newLocksHarness(t)
	alice := h.createUser(t, "alice")
	bob := h.createUser(t, "bob")
	id, err := h.store.Create(context.Background(), locks.CreateInput{
		Tenant: "acme", Repo: "demo", Path: "a.psd",
		OwnerUserID: alice.UserID, Now: time.Unix(1700000000, 0),
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	h.actor = bob

	w := h.do(http.MethodPost,
		"http://example.com/acme/demo.git/info/lfs/locks/"+id+"/unlock",
		lfs.UnlockRequest{Force: false})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s want 403", w.Code, w.Body.String())
	}
	// Confirm still present.
	if _, err := h.store.Get(context.Background(), "acme", "demo", id); err != nil {
		t.Errorf("post-denied Get: %v (lock should still exist)", err)
	}
}

func TestLocksUnlock_NonOwnerWithForceSucceeds(t *testing.T) {
	h := newLocksHarness(t)
	alice := h.createUser(t, "alice")
	bob := h.createUser(t, "bob")
	id, err := h.store.Create(context.Background(), locks.CreateInput{
		Tenant: "acme", Repo: "demo", Path: "a.psd",
		OwnerUserID: alice.UserID, Now: time.Unix(1700000000, 0),
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	h.actor = bob

	w := h.do(http.MethodPost,
		"http://example.com/acme/demo.git/info/lfs/locks/"+id+"/unlock",
		lfs.UnlockRequest{Force: true})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s want 200", w.Code, w.Body.String())
	}
	if _, err := h.store.Get(context.Background(), "acme", "demo", id); !errors.Is(err, locks.ErrNotFound) {
		t.Errorf("post-force-unlock Get=%v want ErrNotFound", err)
	}
}

func TestLocksVerify_PartitionsOwnership(t *testing.T) {
	h := newLocksHarness(t)
	alice := h.createUser(t, "alice")
	bob := h.createUser(t, "bob")
	if _, err := h.store.Create(context.Background(), locks.CreateInput{
		Tenant: "acme", Repo: "demo", Path: "a.psd",
		OwnerUserID: alice.UserID, Now: time.Unix(1700000000, 0),
	}); err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	if _, err := h.store.Create(context.Background(), locks.CreateInput{
		Tenant: "acme", Repo: "demo", Path: "b.psd",
		OwnerUserID: bob.UserID, Now: time.Unix(1700000000, 0),
	}); err != nil {
		t.Fatalf("seed bob: %v", err)
	}

	h.actor = alice
	w := h.do(http.MethodPost,
		"http://example.com/acme/demo.git/info/lfs/locks/verify",
		lfs.LocksVerifyRequest{})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp lfs.LocksVerifyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Ours) != 1 || resp.Ours[0].Path != "a.psd" {
		t.Errorf("Ours=%v want one a.psd", resp.Ours)
	}
	if len(resp.Theirs) != 1 || resp.Theirs[0].Path != "b.psd" {
		t.Errorf("Theirs=%v want one b.psd", resp.Theirs)
	}
}

func TestLocksVerify_EmptyBodyTolerated(t *testing.T) {
	// The LFS spec allows /verify with no body; we must tolerate it
	// rather than erroring on JSON-decode of an empty stream.
	h := newLocksHarness(t)
	h.actor = h.createUser(t, "alice")
	w := h.do(http.MethodPost,
		"http://example.com/acme/demo.git/info/lfs/locks/verify", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s want 200", w.Code, w.Body.String())
	}
}

func TestLocksVerify_EmptyChunkedBodyTolerated(t *testing.T) {
	h := newLocksHarness(t)
	h.actor = h.createUser(t, "alice")
	// Build a request with ContentLength=-1 and an empty body to simulate chunked encoding.
	req := httptest.NewRequest(http.MethodPost,
		"http://example.com/acme/demo.git/info/lfs/locks/verify", nil)
	req.ContentLength = -1
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s want 200", w.Code, w.Body.String())
	}
}

func TestLocksList_BadLimitReturns400(t *testing.T) {
	h := newLocksHarness(t)
	h.actor = h.createUser(t, "alice")
	w := h.do(http.MethodGet,
		"http://example.com/acme/demo.git/info/lfs/locks?limit=abc", nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", w.Code)
	}
}

func TestLocksList_NegativeLimitReturns400(t *testing.T) {
	h := newLocksHarness(t)
	h.actor = h.createUser(t, "alice")
	w := h.do(http.MethodGet,
		"http://example.com/acme/demo.git/info/lfs/locks?limit=-1", nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", w.Code)
	}
}

func TestLocksUnlock_LocksDisabledReturns503(t *testing.T) {
	authdb, err := sqlitestore.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = authdb.Close() })
	actor := &auth.Actor{UserID: "u_alice", Name: "alice"}
	handler := lfs.NewHTTPHandler(lfs.Deps{
		AuthStore:        authdb,
		ActorFromContext: func(context.Context) *auth.Actor { return actor },
		NewStore:         func(tenant, repo string) *lfs.Store { return nil },
		// LocksStore nil
	})
	req := httptest.NewRequest(http.MethodPost,
		"http://example.com/acme/demo.git/info/lfs/locks/lock_x/unlock", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d want 503", w.Code)
	}
}

// --- observability tests ---

// newLocksHarnessWithLogger creates a harness whose handler emits slog
// output to buf, allowing tests to assert on metric + audit lines.
func newLocksHarnessWithLogger(t *testing.T, buf *bytes.Buffer) *locksTestHarness {
	t.Helper()
	authdb, err := sqlitestore.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlitestore.Open: %v", err)
	}
	t.Cleanup(func() { _ = authdb.Close() })

	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := &locksTestHarness{
		authdb: authdb,
		store:  locks.New(authdb),
	}
	h.handler = lfs.NewHTTPHandler(lfs.Deps{
		AuthStore:        authdb,
		ActorFromContext: func(context.Context) *auth.Actor { return h.actor },
		NewStore:         func(tenant, repo string) *lfs.Store { return nil },
		LocksStore:       h.store,
		Logger:           logger,
	})
	return h
}

func TestLocksCreate_EmitsMetricAndAudit_OnSuccess(t *testing.T) {
	var buf bytes.Buffer
	h := newLocksHarnessWithLogger(t, &buf)
	h.actor = h.createUser(t, "alice")

	w := h.do(http.MethodPost,
		"http://example.com/acme/demo.git/info/lfs/locks",
		lfs.LockRequest{Path: "art/hero.psd"})
	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s want 201", w.Code, w.Body.String())
	}
	out := buf.String()
	for _, want := range []string{
		`"metric_name":"lfs_locks_created_total"`,
		`"outcome":"created"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metric: missing %q in log output:\n%s", want, out)
		}
	}
	for _, want := range []string{
		`"event":"lfs.lock.create"`,
		`"repo":"acme/demo"`,
		`"user":"alice"`,
		`"owner_user_id":"` + h.actor.UserID + `"`,
		`"path":"art/hero.psd"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("audit: missing %q in log output:\n%s", want, out)
		}
	}
}

func TestLocksCreate_EmitsConflictMetric_On409(t *testing.T) {
	var buf bytes.Buffer
	h := newLocksHarnessWithLogger(t, &buf)
	h.actor = h.createUser(t, "alice")

	body := lfs.LockRequest{Path: "conflict.psd"}
	// First create succeeds.
	if w := h.do(http.MethodPost,
		"http://example.com/acme/demo.git/info/lfs/locks", body); w.Code != http.StatusCreated {
		t.Fatalf("first: status=%d", w.Code)
	}
	buf.Reset() // clear first-create output
	// Second create should 409.
	if w := h.do(http.MethodPost,
		"http://example.com/acme/demo.git/info/lfs/locks", body); w.Code != http.StatusConflict {
		t.Fatalf("second: status=%d want 409", w.Code)
	}
	out := buf.String()
	for _, want := range []string{
		`"metric_name":"lfs_locks_created_total"`,
		`"outcome":"conflict"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in log output:\n%s", want, out)
		}
	}
	// No audit event on conflict.
	if strings.Contains(out, `"event":"lfs.lock.create"`) {
		t.Errorf("unexpected audit event on conflict path; log:\n%s", out)
	}
}

func TestLocksList_EmitsSuccessMetric(t *testing.T) {
	var buf bytes.Buffer
	h := newLocksHarnessWithLogger(t, &buf)
	h.actor = h.createUser(t, "alice")

	w := h.do(http.MethodGet,
		"http://example.com/acme/demo.git/info/lfs/locks", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	out := buf.String()
	for _, want := range []string{
		`"metric_name":"lfs_locks_listed_total"`,
		`"outcome":"success"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in log output:\n%s", want, out)
		}
	}
}

func TestLocksVerify_EmitsAuditAndMetric(t *testing.T) {
	var buf bytes.Buffer
	h := newLocksHarnessWithLogger(t, &buf)
	alice := h.createUser(t, "alice")
	bob := h.createUser(t, "bob")
	// Seed: alice owns 2, bob owns 1.
	for _, p := range []string{"a.psd", "b.psd"} {
		if _, err := h.store.Create(context.Background(), locks.CreateInput{
			Tenant: "acme", Repo: "demo", Path: p,
			OwnerUserID: alice.UserID, Now: time.Unix(1700000000, 0),
		}); err != nil {
			t.Fatalf("seed alice %s: %v", p, err)
		}
	}
	if _, err := h.store.Create(context.Background(), locks.CreateInput{
		Tenant: "acme", Repo: "demo", Path: "c.psd",
		OwnerUserID: bob.UserID, Now: time.Unix(1700000000, 0),
	}); err != nil {
		t.Fatalf("seed bob: %v", err)
	}

	h.actor = alice
	w := h.do(http.MethodPost,
		"http://example.com/acme/demo.git/info/lfs/locks/verify",
		lfs.LocksVerifyRequest{})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	out := buf.String()
	for _, want := range []string{
		`"metric_name":"lfs_locks_verified_total"`,
		`"outcome":"success"`,
		`"event":"lfs.lock.verify"`,
		`"repo":"acme/demo"`,
		`"user":"alice"`,
		`"ours_count":2`,
		`"theirs_count":1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in log output:\n%s", want, out)
		}
	}
}

func TestLocksUnlock_OwnerEmitsOwnerMetricAndAudit(t *testing.T) {
	var buf bytes.Buffer
	h := newLocksHarnessWithLogger(t, &buf)
	h.actor = h.createUser(t, "alice")
	id, err := h.store.Create(context.Background(), locks.CreateInput{
		Tenant: "acme", Repo: "demo", Path: "a.psd",
		OwnerUserID: h.actor.UserID, Now: time.Unix(1700000000, 0),
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	w := h.do(http.MethodPost,
		"http://example.com/acme/demo.git/info/lfs/locks/"+id+"/unlock",
		lfs.UnlockRequest{Force: false})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	out := buf.String()
	for _, want := range []string{
		`"metric_name":"lfs_locks_deleted_total"`,
		`"outcome":"owner"`,
		`"force":"false"`,
		`"event":"lfs.lock.delete"`,
		`"user":"alice"`,
		`"force_target_user_id":""`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in log output:\n%s", want, out)
		}
	}
}

func TestLocksUnlock_ForcedEmitsForcedMetricAndAudit(t *testing.T) {
	var buf bytes.Buffer
	h := newLocksHarnessWithLogger(t, &buf)
	alice := h.createUser(t, "alice")
	bob := h.createUser(t, "bob")
	id, err := h.store.Create(context.Background(), locks.CreateInput{
		Tenant: "acme", Repo: "demo", Path: "a.psd",
		OwnerUserID: alice.UserID, Now: time.Unix(1700000000, 0),
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	h.actor = bob

	w := h.do(http.MethodPost,
		"http://example.com/acme/demo.git/info/lfs/locks/"+id+"/unlock",
		lfs.UnlockRequest{Force: true})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	out := buf.String()
	for _, want := range []string{
		`"metric_name":"lfs_locks_deleted_total"`,
		`"outcome":"forced"`,
		`"force":"true"`,
		`"event":"lfs.lock.delete"`,
		`"user":"bob"`,
		`"force_target_user_id":"` + alice.UserID + `"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in log output:\n%s", want, out)
		}
	}
}

func TestLocksUnlock_DeniedEmitsDeniedMetric(t *testing.T) {
	var buf bytes.Buffer
	h := newLocksHarnessWithLogger(t, &buf)
	alice := h.createUser(t, "alice")
	bob := h.createUser(t, "bob")
	id, err := h.store.Create(context.Background(), locks.CreateInput{
		Tenant: "acme", Repo: "demo", Path: "a.psd",
		OwnerUserID: alice.UserID, Now: time.Unix(1700000000, 0),
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	h.actor = bob

	w := h.do(http.MethodPost,
		"http://example.com/acme/demo.git/info/lfs/locks/"+id+"/unlock",
		lfs.UnlockRequest{Force: false})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403", w.Code)
	}
	out := buf.String()
	for _, want := range []string{
		`"metric_name":"lfs_locks_deleted_total"`,
		`"outcome":"denied"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in log output:\n%s", want, out)
		}
	}
	// No audit event on denied path.
	if strings.Contains(out, `"event":"lfs.lock.delete"`) {
		t.Errorf("unexpected audit event on denied path; log:\n%s", out)
	}
}

func TestLocksList_LimitExceedsMaxReturns400(t *testing.T) {
	h := newLocksHarness(t)
	h.actor = h.createUser(t, "alice")
	w := h.do(http.MethodGet,
		"http://example.com/acme/demo.git/info/lfs/locks?limit=10000", nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", w.Code)
	}
}

func TestLocksCreate_WrongContentTypeReturns415(t *testing.T) {
	h := newLocksHarness(t)
	h.actor = h.createUser(t, "alice")
	body, _ := json.Marshal(lfs.LockRequest{Path: "a.psd"})
	req := httptest.NewRequest(http.MethodPost,
		"http://example.com/acme/demo.git/info/lfs/locks", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status=%d want 415", w.Code)
	}
}

func TestLocksCreate_RefScopePropagatesToStore(t *testing.T) {
	h := newLocksHarness(t)
	h.actor = h.createUser(t, "alice")
	w := h.do(http.MethodPost,
		"http://example.com/acme/demo.git/info/lfs/locks",
		lfs.LockRequest{
			Path: "art/hero.psd",
			Ref:  &lfs.LockRefSpec{Name: "refs/heads/release"},
		})
	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp lfs.LockResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Verify via the store directly that ref_name was persisted.
	stored, err := h.store.Get(context.Background(), "acme", "demo", resp.Lock.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.RefName != "refs/heads/release" {
		t.Errorf("stored.RefName=%q want refs/heads/release (handler dropped Ref on the floor)", stored.RefName)
	}
}
