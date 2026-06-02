package web

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestRenderLanding_DiskOverride(t *testing.T) {
	dir := t.TempDir()
	// Recreate the templates/ tree on disk from the embedded assets so the
	// disk-override path (--ui-dir) is exercised exactly as in production.
	if err := os.MkdirAll(filepath.Join(dir, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"base.html", "landing.html", "login.html", "error.html"} {
		b, err := assetsFS.ReadFile("templates/" + name)
		if err != nil {
			t.Fatalf("read embedded %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "templates", name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	r, err := newRenderer(dir)
	if err != nil {
		t.Fatalf("newRenderer(%q): %v", dir, err)
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
	for _, want := range []string{"<!doctype html>", "acme/", "demo", "alice"} {
		if !strings.Contains(out, want) {
			t.Fatalf("disk render missing %q:\n%s", want, out)
		}
	}

	// Hot-reload: editing a template on disk is reflected without rebuilding the renderer.
	custom := "{{define \"title\"}}x{{end}}{{define \"content\"}}HOTRELOAD-MARKER{{end}}{{template \"base\" .}}"
	if err := os.WriteFile(filepath.Join(dir, "templates", "landing.html"), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if err := r.render(&buf, "landing.html", data); err != nil {
		t.Fatalf("render after edit: %v", err)
	}
	if !strings.Contains(buf.String(), "HOTRELOAD-MARKER") {
		t.Fatalf("disk override did not hot-reload:\n%s", buf.String())
	}
}
