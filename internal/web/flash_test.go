package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFlashRoundTrip(t *testing.T) {
	rec := httptest.NewRecorder()
	setFlash(rec, "grant added", false)
	res := rec.Result()
	cookies := res.Cookies()
	if len(cookies) != 1 || cookies[0].Name != flashCookieName {
		t.Fatalf("want one %s cookie, got %v", flashCookieName, cookies)
	}
	if !cookies[0].HttpOnly {
		t.Fatal("flash cookie must be HttpOnly")
	}

	r := httptest.NewRequest(http.MethodGet, "/settings", nil)
	r.AddCookie(cookies[0])
	rec2 := httptest.NewRecorder()
	got := takeFlash(rec2, r)
	if got != "grant added" {
		t.Fatalf("takeFlash = %q, want %q", got, "grant added")
	}
	// takeFlash must clear the cookie (MaxAge<0).
	cleared := rec2.Result().Cookies()
	if len(cleared) != 1 || cleared[0].MaxAge >= 0 {
		t.Fatalf("expected clearing Set-Cookie, got %v", cleared)
	}
}

func TestFlashAbsent(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := takeFlash(httptest.NewRecorder(), r); got != "" {
		t.Fatalf("takeFlash on no cookie = %q, want empty", got)
	}
}
