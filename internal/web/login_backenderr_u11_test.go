package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/ratelimit"
)

// Wave-4 U-11 regression coverage: before the fix, handleLogin rendered the
// 401 "invalid username or password" page for ANY VerifyPassword error. A
// backend authdb failure (wrapped lookup error, NOT an auth.IsCredentialError
// sentinel) was indistinguishable from bad credentials and incremented the
// rate-limit failure bucket. The adjudicated contract mirrors gateway.RunAuth:
// credential errors → 401 page + MarkFailure; any other error → renderError
// 500, no MarkFailure.
//
// MarkFailure gating is asserted behaviorally with a real Burst=1, RefillPerMinute=0
// limiter: a marked failure pins the bucket so the NEXT request 429s; an
// unmarked error leaves the following request unthrottled.

func u11Handler(store *fakeStore) http.Handler {
	limiter := ratelimit.NewLimiter(ratelimit.Config{
		Burst:           1,
		RefillPerMinute: 0, // no decay: any MarkFailure pins the bucket
		SweepInterval:   24 * time.Hour,
	})
	return NewHandler(Deps{Store: store, Logger: slog.Default(), Limiter: limiter})
}

func u11LoginPOST(t *testing.T, h http.Handler, csrfCookie *http.Cookie, tok string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"username": {"alice"}, "password": {"pw"}, csrfFormField: {tok}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(csrfCookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestU11_LoginBackendError_Renders500_NoFailureMark(t *testing.T) {
	store := newFakeStore()
	store.verify = func(ctx context.Context, u, p string) (*auth.Actor, error) {
		// The adjudicated failure shape: VerifyPassword wraps lookup errors as
		// non-credential errors (internal/auth/errors.go classification).
		return nil, fmt.Errorf("lookup user: %w", errors.New("database is closed"))
	}
	h := u11Handler(store)

	// GET /login → CSRF cookie + hidden token.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	csrfCookie := findCookie(rec.Result().Cookies(), csrfCookieName)
	tok := extractHidden(rec.Body.String(), csrfFormField)
	if csrfCookie == nil || tok == "" {
		t.Fatal("GET /login did not issue CSRF cookie+token")
	}

	// Backend failure → 500 error page, NOT the 401 credential page.
	rec = u11LoginPOST(t, h, csrfCookie, tok)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("backend error status %d, want 500 (must not mask as 401)", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "invalid username or password") {
		t.Fatalf("backend error rendered the credential-failure page:\n%s", rec.Body.String())
	}

	// No MarkFailure → the limiter bucket is untouched → the next POST is NOT
	// 429 (Burst=1, no decay).
	rec = u11LoginPOST(t, h, csrfCookie, tok)
	if rec.Code == http.StatusTooManyRequests {
		t.Fatalf("backend error counted as a credential failure (MarkFailure fired); status 429")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("second backend error status %d, want 500", rec.Code)
	}
}

func TestU11_LoginCredentialError_Renders401_MarksFailure(t *testing.T) {
	store := newFakeStore()
	store.verify = func(ctx context.Context, u, p string) (*auth.Actor, error) {
		return nil, auth.ErrInvalidCredential
	}
	h := u11Handler(store)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	csrfCookie := findCookie(rec.Result().Cookies(), csrfCookieName)
	tok := extractHidden(rec.Body.String(), csrfFormField)

	// Credential failure → 401 login page with the invalid-credentials copy
	// (unchanged behavior) ...
	rec = u11LoginPOST(t, h, csrfCookie, tok)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("credential error status %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid username or password") {
		t.Fatalf("credential error page missing invalid-credentials copy:\n%s", rec.Body.String())
	}
	if findCookie(rec.Result().Cookies(), sessionCookieName) != nil {
		t.Fatal("session cookie issued on credential failure")
	}

	// ... and MarkFailure pins a Burst=1 bucket, so the retry 429s.
	rec = u11LoginPOST(t, h, csrfCookie, tok)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("credential failure did not MarkFailure; status %d, want 429 (Retry-After throttled)", rec.Code)
	}
}
