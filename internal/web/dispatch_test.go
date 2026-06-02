package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDispatcher_Routing(t *testing.T) {
	gitHit, webHit := false, false
	git := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gitHit = true })
	ui := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { webHit = true })
	d := Dispatcher(git, ui)

	cases := []struct {
		path string
		git  bool
	}{
		{"/acme/demo.git/info/refs", true},
		{"/acme/demo.git/git-upload-pack", true},
		{"/acme/demo.git/info/lfs/objects/batch", true},
		{"/_lfs/abc", true},
		{"/_bundle/x", true},
		{"/healthz", true},
		{"/", false},
		{"/login", false},
		{"/acme", false},
		{"/acme/demo", false},
		{"/_ui/static/style.css", false}, // UI owns /_ui despite the /_ prefix
	}
	for _, c := range cases {
		gitHit, webHit = false, false
		d.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", c.path, nil))
		if c.git && !gitHit {
			t.Errorf("%s: expected git, got web", c.path)
		}
		if !c.git && !webHit {
			t.Errorf("%s: expected web, got git", c.path)
		}
	}
}
