package lfs

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/lfs/locks"
)

// newReplicaHandlerForTest builds an LFS handler with ReadOnlyReplica set,
// wiring a real in-memory authdb-backed locks store so the locks-create
// route reaches the replica refusal (rather than the locks-disabled 503).
func newReplicaHandlerForTest(t *testing.T, store *Store, authStore *fakeAuth, actor *auth.Actor) *httptest.Server {
	t.Helper()
	authdb, err := sqlitestore.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlitestore.Open: %v", err)
	}
	t.Cleanup(func() { _ = authdb.Close() })
	lfsH := NewHTTPHandler(Deps{
		AuthStore:        authStore,
		ActorFromContext: func(context.Context) *auth.Actor { return actor },
		NewStore:         func(tenant, repo string) *Store { return store },
		PresignTTL:       5 * time.Minute,
		Logger:           captureLogger(&bytes.Buffer{}),
		LocksStore:       locks.New(authdb),
		ReadOnlyReplica:  true,
		WriteRegionURL:   "https://gw-us.example",
	})
	return httptest.NewServer(lfsH)
}

func TestHandlerReadOnlyReplica(t *testing.T) {
	authStore := &fakeAuth{
		repoPerm: map[string]auth.Perm{"acme/foo": auth.PermWrite},
		actors:   map[string]*auth.Actor{"pw": {Name: "alice"}},
	}
	actor := &auth.Actor{Name: "alice", UserID: "u-alice"}

	t.Run("upload_batch_refused", func(t *testing.T) {
		store := newProxiedBatchStore(nil, signedFn())
		srv := newReplicaHandlerForTest(t, store, authStore, actor)
		defer srv.Close()

		body, _ := json.Marshal(BatchRequest{
			Operation: "upload",
			Transfers: []string{"basic"},
			Objects:   []ObjectRef{{OID: oidNew, Size: 1}},
		})
		req, _ := http.NewRequest(http.MethodPost,
			srv.URL+"/acme/foo.git/info/lfs/objects/batch", bytes.NewReader(body))
		req.Header.Set("Content-Type", ContentType)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			b, _ := readAllBody(resp)
			t.Fatalf("upload batch: status=%d want 403; body=%s", resp.StatusCode, b)
		}
		if ct := resp.Header.Get("Content-Type"); ct != ContentType {
			t.Errorf("Content-Type=%q want %q", ct, ContentType)
		}
		b, _ := readAllBody(resp)
		if !strings.Contains(b, "read-only replica") || !strings.Contains(b, "https://gw-us.example") {
			t.Errorf("upload batch body missing refusal markers: %s", b)
		}
	})

	t.Run("download_batch_unaffected", func(t *testing.T) {
		store := newBatchStore(nil, signedFn())
		srv := newReplicaHandlerForTest(t, store, authStore, actor)
		defer srv.Close()

		body, _ := json.Marshal(BatchRequest{
			Operation: "download",
			Transfers: []string{"basic"},
			Objects:   []ObjectRef{{OID: oidNew, Size: 1}},
		})
		req, _ := http.NewRequest(http.MethodPost,
			srv.URL+"/acme/foo.git/info/lfs/objects/batch", bytes.NewReader(body))
		req.Header.Set("Content-Type", ContentType)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := readAllBody(resp)
			t.Fatalf("download batch: status=%d want 200; body=%s", resp.StatusCode, b)
		}
	})

	t.Run("locks_create_refused", func(t *testing.T) {
		store := newBatchStore(nil, signedFn())
		srv := newReplicaHandlerForTest(t, store, authStore, actor)
		defer srv.Close()

		body, _ := json.Marshal(LockRequest{Path: "art/hero.psd"})
		req, _ := http.NewRequest(http.MethodPost,
			srv.URL+"/acme/foo.git/info/lfs/locks", bytes.NewReader(body))
		req.Header.Set("Content-Type", ContentType)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			b, _ := readAllBody(resp)
			t.Fatalf("locks create: status=%d want 403; body=%s", resp.StatusCode, b)
		}
		if ct := resp.Header.Get("Content-Type"); ct != ContentType {
			t.Errorf("Content-Type=%q want %q", ct, ContentType)
		}
		b, _ := readAllBody(resp)
		if !strings.Contains(b, "read-only replica") || !strings.Contains(b, "https://gw-us.example") {
			t.Errorf("locks create body missing refusal markers: %s", b)
		}
	})
}
