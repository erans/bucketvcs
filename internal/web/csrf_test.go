package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestCSRFIssueAndCheck(t *testing.T) {
	// Issue sets a cookie and returns the token.
	rec := httptest.NewRecorder()
	tok := issueCSRF(rec, false)
	if tok == "" {
		t.Fatal("empty csrf token")
	}
	cookie := rec.Result().Cookies()[0]
	if cookie.Name != csrfCookieName {
		t.Fatalf("cookie name = %q", cookie.Name)
	}

	// Matching form value + cookie => valid.
	form := url.Values{csrfFormField: {tok}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	if err := req.ParseForm(); err != nil {
		t.Fatalf("ParseForm: %v", err)
	}
	if !checkCSRF(req) {
		t.Fatal("valid token rejected")
	}

	// Mismatch => invalid.
	bad := httptest.NewRequest("POST", "/login", strings.NewReader(url.Values{csrfFormField: {"nope"}}.Encode()))
	bad.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	bad.AddCookie(cookie)
	_ = bad.ParseForm()
	if checkCSRF(bad) {
		t.Fatal("mismatched token accepted")
	}

	// Missing cookie => invalid.
	noCookie := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	noCookie.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_ = noCookie.ParseForm()
	if checkCSRF(noCookie) {
		t.Fatal("no-cookie request accepted")
	}
}

var _ = http.MethodPost
