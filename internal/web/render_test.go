package web

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func TestRenderLanding_Embedded(t *testing.T) {
	r, err := newRenderer("") // "" => embedded
	if err != nil {
		t.Fatalf("newRenderer: %v", err)
	}
	var buf bytes.Buffer
	data := landingData{
		base:  base{Session: &auth.Session{Name: "alice"}, CSRF: "tok"},
		Repos: map[string][]Repo{"acme": {{Tenant: "acme", Name: "demo", PublicRead: true}}},
	}
	if err := r.render(&buf, "landing.html", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"acme/", "demo", "public", "alice", "bucketvcs"} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered output missing %q:\n%s", want, out)
		}
	}
}

func TestStaticHandlerServesCSS(t *testing.T) {
	h := staticHandler("")
	req := httptest.NewRequest(http.MethodGet, "/_ui/static/style.css", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct == "" {
		t.Fatal("missing content-type")
	}
}
