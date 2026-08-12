package lfs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// Wave-2 U-6 regression coverage (handler contract half): Deps.NewStore
// receives the request-scoped context and surfaces resolution failures as a
// request-level error instead of the gateway substituting the operator
// store.

// TestBatch_NewStoreReceivesRequestContext cancels the request context before
// dispatch: the NewStore closure (which the gateway wires to its resolver)
// must observe the cancellation and return the error; the batch must then
// fail at request level. Pre-fix the closure signature carried no context
// and the gateway resolved on a detached context.Background().
func TestBatch_NewStoreReceivesRequestContext(t *testing.T) {
	authStore := &fakeAuth{
		actors:   map[string]*auth.Actor{"pw": {Name: "alice"}},
		repoPerm: map[string]auth.Perm{"acme/foo": auth.PermWrite},
	}
	observed := make(chan error, 1)
	lfsH := NewHTTPHandler(Deps{
		AuthStore:        authStore,
		ActorFromContext: func(context.Context) *auth.Actor { return &auth.Actor{Name: "alice"} },
		NewStore: func(ctx context.Context, tenant, repo string) (*Store, error) {
			observed <- ctx.Err()
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return newProxiedBatchStore(nil, signedFn()), nil
		},
		PresignTTL: 5 * time.Minute,
		Logger:     captureLogger(&bytes.Buffer{}),
	})

	body, _ := json.Marshal(BatchRequest{
		Operation: "upload",
		Transfers: []string{"basic"},
		Objects:   []ObjectRef{{OID: "abc123def456" + strings.Repeat("0", 52), Size: 5}},
	})

	// Cancel BEFORE dispatch so any use of the request context downstream
	// observes it.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req := httptest.NewRequest(http.MethodPost,
		"/acme/foo.git/info/lfs/objects/batch", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", ContentType)
	rec := httptest.NewRecorder()
	lfsH.ServeHTTP(rec, req)

	select {
	case err := <-observed:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("NewStore observed ctx.Err() = %v, want context.Canceled (pre-fix the gateway passed a detached context.Background() — U-6)", err)
		}
	default:
		t.Fatal("NewStore closure was never invoked")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("NewStore error must fail the batch at request level: status=%d, want 500", rec.Code)
	}
}
