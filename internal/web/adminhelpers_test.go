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
	"github.com/bucketvcs/bucketvcs/internal/policy"
)

// newTestServerWithRender builds a *server with a real embedded renderer for
// tests that exercise paths calling renderError (which writes a template).
func newTestServerWithRender(t *testing.T) *server {
	t.Helper()
	r, err := newRenderer("")
	if err != nil {
		t.Fatalf("newRenderer: %v", err)
	}
	return &server{
		store:  newFakeStore(),
		logger: slog.Default(),
		ttl:    DefaultSessionTTL,
		render: r,
	}
}

func TestPostGuard(t *testing.T) {
	s := newTestServerWithRender(t)
	rec := httptest.NewRecorder()
	if s.postGuard(rec, httptest.NewRequest(http.MethodGet, "/x", nil)) {
		t.Fatal("GET must fail postGuard")
	}
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code = %d, want 405", rec.Code)
	}
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("a=b"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	if s.postGuard(rec, r) {
		t.Fatal("no-CSRF POST must fail postGuard")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", rec.Code)
	}
	form := url.Values{"a": {"b"}}
	r = csrfPost(t, "/x", form)
	rec = httptest.NewRecorder()
	if !s.postGuard(rec, r) {
		t.Fatal("valid POST must pass postGuard")
	}
}

func TestRedirectFlash(t *testing.T) {
	s := newTestServerStruct(newFakeStore())
	r := httptest.NewRequest(http.MethodPost, "/x", nil)
	rec := httptest.NewRecorder()
	s.redirectFlash(rec, r, "/settings", "saved!")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("redirectFlash: code = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/settings" {
		t.Fatalf("redirectFlash: Location = %q, want /settings", got)
	}
	// Flash cookie must be set.
	var flashCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == flashCookieName {
			flashCookie = c
			break
		}
	}
	if flashCookie == nil {
		t.Fatal("redirectFlash: flash cookie not set")
	}
	if flashCookie.Value == "" {
		t.Fatal("redirectFlash: flash cookie value is empty")
	}
}

func TestEmitAdmin(t *testing.T) {
	logger, sink := newTestLogger()
	s := &server{logger: logger}

	// With a session — actor should be the session name.
	sess := &auth.Session{
		UserID:    "u1",
		Name:      "alice",
		Provider:  "password",
		ExpiresAt: time.Now().Add(time.Hour),
	}
	ctx := withSession(context.Background(), sess)
	s.emitAdmin(ctx, "admin.test.event",
		slog.String("domain", "webhook"),
		slog.String("action", "create"),
	)
	if !sink.Has("admin.test.event", map[string]string{
		"actor":  "alice",
		"source": "web",
		"domain": "webhook",
		"action": "create",
	}) {
		t.Fatal("emitAdmin: expected log record not found for session actor")
	}

	// Nil session — actor should be empty string (anonymous).
	ctx2 := withSession(context.Background(), nil)
	s.emitAdmin(ctx2, "admin.anon.event")
	if !sink.Has("admin.anon.event", map[string]string{
		"actor":  "",
		"source": "web",
	}) {
		t.Fatal("emitAdmin: expected log record not found for anonymous actor")
	}

	// Nil logger — should not panic (falls back to slog.Default()).
	s2 := &server{logger: nil}
	s2.emitAdmin(context.Background(), "admin.nillogger.event")
	// Just confirm it did not panic.
}

func TestRenderBufferedNilLogger(t *testing.T) {
	r, err := newRenderer("")
	if err != nil {
		t.Fatalf("newRenderer: %v", err)
	}
	// Hand-constructed server with nil logger — mirrors the code-review scenario.
	s := &server{render: r, logger: nil}
	rec := httptest.NewRecorder()
	// "nonexistent.html" is not in the embedded cache, so render() returns an
	// error and renderBuffered must log it without panicking.
	gotErr := s.renderBuffered(rec, "nonexistent.html", nil)
	if gotErr == nil {
		t.Fatal("renderBuffered: expected error for unknown page, got nil")
	}
}

// TestFlashableErr pins the conservative validation-vs-internal predicates so
// wrapped DB errors never leak into a flash.
func TestFlashableErr(t *testing.T) {
	dbWrap := errors.New("disk I/O error")
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		// policy.Add validation messages.
		{"policy empty pattern", fmt.Errorf("policy: refname_pattern must not be empty"), true},
		{"policy invalid refname", fmt.Errorf("policy: invalid refname_pattern %q: %w", "[", errors.New("syntax error in pattern")), true},
		// policy.Add DB wrap ("policy add ...", no colon) → masked.
		{"policy.Add db wrap", fmt.Errorf("policy add %q/%q %q: %w", "acme", "demo", "refs/heads/main", dbWrap), false},
		// policy.AddPathRule validation wraps ErrInvalidInput.
		{"path ErrInvalidInput", fmt.Errorf("%w: invalid path_pattern: bad", policy.ErrInvalidInput), true},
		// policy.AddPathRule DB wrap ("policy: add path rule: ...") → masked
		// despite the "policy: " prefix.
		{"path db wrap", fmt.Errorf("policy: add path rule: %w", dbWrap), false},
		// hooks validateRow messages.
		{"hooks validate", fmt.Errorf("hooks: invalid script_name %q (must be ...)", "bad/name"), true},
		// hooks DB wrap ("hooks.Add: ...") → masked.
		{"hooks.Add db wrap", fmt.Errorf("hooks.Add: %w", dbWrap), false},
		// ErrConflict reserved-sentinel conformance.
		{"policy ErrConflict", fmt.Errorf("wrapped: %w", policy.ErrConflict), true},
		// Arbitrary unknown error → masked.
		{"unknown", dbWrap, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := flashableErr(c.err); got != c.want {
				t.Fatalf("flashableErr(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
