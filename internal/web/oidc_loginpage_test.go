package web

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLoginPage_ShowsSSOWhenEnabled(t *testing.T) {
	h := NewHandler(oidcTestDeps(newFakeStore())) // OIDC enabled, Label "Single sign-on"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/login?next=/repos", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "/login/oidc") {
		t.Fatalf("login page missing SSO link:\n%s", body)
	}
	if !strings.Contains(body, "Single sign-on") {
		t.Fatalf("login page missing SSO label:\n%s", body)
	}
	if !strings.Contains(body, "/login/oidc?next=") {
		t.Fatalf("SSO link missing next:\n%s", body)
	}
}

func TestLoginPage_NoSSOWhenDisabled(t *testing.T) {
	h := NewHandler(Deps{Store: newFakeStore()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/login", nil))
	if strings.Contains(rec.Body.String(), "/login/oidc") {
		t.Fatal("SSO link shown when OIDC disabled")
	}
}
