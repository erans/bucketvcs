package gateway

import (
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func TestParseRoute_Table(t *testing.T) {
	type tc struct {
		name     string
		method   string
		path     string
		query    string
		wantOp   Op
		wantAct  auth.Action
		wantTen  string
		wantRepo string
		wantErr  bool
	}
	cases := []tc{
		{"info-refs upload", "GET", "/acme/foo.git/info/refs", "service=git-upload-pack",
			OpInfoRefsUpload, auth.ActionRead, "acme", "foo", false},
		{"info-refs receive", "GET", "/acme/foo.git/info/refs", "service=git-receive-pack",
			OpInfoRefsReceive, auth.ActionWrite, "acme", "foo", false},
		{"upload-pack", "POST", "/acme/foo.git/git-upload-pack", "",
			OpUploadPack, auth.ActionRead, "acme", "foo", false},
		{"receive-pack", "POST", "/acme/foo.git/git-receive-pack", "",
			OpReceivePack, auth.ActionWrite, "acme", "foo", false},

		{"missing .git suffix", "GET", "/acme/foo/info/refs", "service=git-upload-pack",
			0, 0, "", "", true},
		{"unknown service param", "GET", "/acme/foo.git/info/refs", "service=git-archive",
			0, 0, "", "", true},
		{"info-refs without service", "GET", "/acme/foo.git/info/refs", "",
			0, 0, "", "", true},
		{"upload-pack via GET", "GET", "/acme/foo.git/git-upload-pack", "",
			0, 0, "", "", true},
		{"trailing slash", "GET", "/acme/foo.git/info/refs/", "service=git-upload-pack",
			0, 0, "", "", true},
		{"invalid tenant", "GET", "/../foo.git/info/refs", "service=git-upload-pack",
			0, 0, "", "", true},
		{"invalid repo", "GET", "/acme/!!.git/info/refs", "service=git-upload-pack",
			0, 0, "", "", true},
		{"missing tenant", "GET", "/.git/info/refs", "service=git-upload-pack",
			0, 0, "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr, err := ParseRoute(c.method, c.path, c.query)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", rr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if rr.Op != c.wantOp || rr.RequiredAction != c.wantAct ||
				rr.Tenant != c.wantTen || rr.Repo != c.wantRepo {
				t.Fatalf("got %+v, want op=%v act=%v tenant=%q repo=%q",
					rr, c.wantOp, c.wantAct, c.wantTen, c.wantRepo)
			}
		})
	}
}

func TestParseRoute_NoMatchSentinel(t *testing.T) {
	_, err := ParseRoute("GET", "/some/random/path", "")
	if !errors.Is(err, ErrRouteNoMatch) {
		t.Fatalf("want ErrRouteNoMatch, got %v", err)
	}
}

func TestParseRoute_LFSBatch(t *testing.T) {
	rr, err := ParseRoute("POST", "/acme/foo.git/info/lfs/objects/batch", "")
	if err != nil {
		t.Fatalf("ParseRoute: %v", err)
	}
	if rr.Op != OpLFSBatch {
		t.Fatalf("Op=%v, want OpLFSBatch", rr.Op)
	}
	if rr.Tenant != "acme" || rr.Repo != "foo" {
		t.Fatalf("Tenant=%q Repo=%q", rr.Tenant, rr.Repo)
	}
	// RequiredAction is read; the handler upgrades to write inline for
	// upload op (the body is what determines it, and ParseRoute is
	// body-free).
	if rr.RequiredAction != auth.ActionRead {
		t.Fatalf("RequiredAction=%v want ActionRead", rr.RequiredAction)
	}
}

func TestParseRoute_LFSBatch_RejectsGET(t *testing.T) {
	_, err := ParseRoute("GET", "/acme/foo.git/info/lfs/objects/batch", "")
	if !errors.Is(err, ErrRouteNoMatch) {
		t.Fatalf("err=%v, want ErrRouteNoMatch", err)
	}
}

