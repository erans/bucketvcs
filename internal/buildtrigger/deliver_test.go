package buildtrigger

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPDeliverer_PostsSignedBodyWithToken(t *testing.T) {
	var gotSig, gotBody, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get("BucketVCS-Signature")
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	tr := Trigger{Kind: KindGeneric, TokenMode: TokenInject, Config: Config{URL: srv.URL, Secret: "shh"}}
	p := BuildPayload{Tenant: "acme", Repo: "app", RefUpdate: RefUpdate{Refname: "refs/heads/main"}}

	d := &httpDeliverer{client: srv.Client(), mintFn: func(context.Context, Trigger, BuildPayload) (string, error) {
		return "bvts_minted", nil
	}}
	code, err := d.Deliver(context.Background(), tr, p)
	if err != nil || code != 200 {
		t.Fatalf("deliver: code=%d err=%v", code, err)
	}
	if !strings.HasPrefix(gotSig, "t=") || !strings.Contains(gotSig, ",v1=") {
		t.Fatalf("bad signature header: %q", gotSig)
	}
	if gotCT != "application/json" {
		t.Fatalf("content-type=%q", gotCT)
	}
	if !strings.Contains(gotBody, `"bvts_token":"bvts_minted"`) {
		t.Fatalf("token not injected: %s", gotBody)
	}
}

func TestHTTPDeliverer_NoTokenWhenModeNone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "bvts_token") {
			t.Errorf("token must be absent in TokenNone mode: %s", b)
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()
	tr := Trigger{Kind: KindGeneric, TokenMode: TokenNone, Config: Config{URL: srv.URL, Secret: "s"}}
	d := &httpDeliverer{client: srv.Client(), mintFn: func(context.Context, Trigger, BuildPayload) (string, error) {
		t.Fatal("mint must not be called in TokenNone mode")
		return "", nil
	}}
	if code, err := d.Deliver(context.Background(), tr, BuildPayload{Repo: "app"}); err != nil || code != 204 {
		t.Fatalf("code=%d err=%v", code, err)
	}
}

func TestHTTPDeliverer_Non2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	tr := Trigger{Kind: KindGeneric, TokenMode: TokenNone, Config: Config{URL: srv.URL, Secret: "s"}}
	d := &httpDeliverer{client: srv.Client()}
	code, err := d.Deliver(context.Background(), tr, BuildPayload{Repo: "app"})
	if code != 500 || err == nil {
		t.Fatalf("expected 500 + error, got code=%d err=%v", code, err)
	}
}

func TestHTTPDeliverer_BadSchemeRejected(t *testing.T) {
	tr := Trigger{Kind: KindGeneric, TokenMode: TokenNone, Config: Config{URL: "ftp://x", Secret: "s"}}
	d := &httpDeliverer{client: http.DefaultClient}
	if _, err := d.Deliver(context.Background(), tr, BuildPayload{}); err == nil {
		t.Fatal("expected error for non-http(s) scheme")
	}
}

func TestHTTPDeliverer_MintErrorIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	tr := Trigger{Kind: KindGeneric, TokenMode: TokenInject, Config: Config{URL: srv.URL, Secret: "s"}}
	d := &httpDeliverer{client: srv.Client(), mintFn: func(context.Context, Trigger, BuildPayload) (string, error) {
		return "", context.DeadlineExceeded
	}}
	if _, err := d.Deliver(context.Background(), tr, BuildPayload{}); err == nil {
		t.Fatal("mint failure must surface as a delivery error (retryable), not a silent success")
	}
}

func TestHTTPStatusPermanent(t *testing.T) {
	cases := map[int]bool{
		400: true, 401: true, 403: true, 404: true, 422: true,
		408: false, 429: false,
		500: false, 502: false, 503: false,
		200: false, 302: false,
	}
	for code, want := range cases {
		if got := httpStatusPermanent(code); got != want {
			t.Errorf("httpStatusPermanent(%d)=%v, want %v", code, got, want)
		}
	}
}

func TestPermanentf_IsErrPermanent(t *testing.T) {
	err := permanentf("HTTP %d", 404)
	if !errors.Is(err, ErrPermanent) {
		t.Fatal("permanentf result should satisfy errors.Is(err, ErrPermanent)")
	}
	if !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("error message lost detail: %q", err.Error())
	}
	if errors.Is(fmt.Errorf("plain"), ErrPermanent) {
		t.Error("a plain error must not be ErrPermanent")
	}
}
