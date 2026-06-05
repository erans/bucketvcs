package webhooks_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

func TestEgressIPDenied(t *testing.T) {
	deny := []string{
		"127.0.0.1", "::1", // loopback
		"169.254.169.254", "169.254.1.1", "fe80::1", // link-local / metadata
		"10.0.0.5", "172.16.3.4", "192.168.1.1", // RFC1918
		"fd12:3456::1",  // ULA
		"0.0.0.0", "::", // unspecified
		"224.0.0.1", "ff02::1", // multicast
		"255.255.255.255", // broadcast
		"::ffff:10.0.0.5", // v4-mapped v6 private (Unmap coverage)
	}
	allow := []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111", "93.184.216.34"}

	p := &webhooks.EgressPolicy{}
	for _, s := range deny {
		if !p.IPDenied(netip.MustParseAddr(s)) {
			t.Errorf("IPDenied(%s) = false, want true", s)
		}
	}
	for _, s := range allow {
		if p.IPDenied(netip.MustParseAddr(s)) {
			t.Errorf("IPDenied(%s) = true, want false", s)
		}
	}
}

func TestEgressAllowCIDRPunchesHole(t *testing.T) {
	p := &webhooks.EgressPolicy{AllowCIDRs: []netip.Prefix{
		netip.MustParsePrefix("192.168.1.0/24"),
		netip.MustParsePrefix("127.0.0.0/8"),
	}}
	for _, s := range []string{"192.168.1.7", "127.0.0.1"} {
		if p.IPDenied(netip.MustParseAddr(s)) {
			t.Errorf("IPDenied(%s) = true, want false (allow-cidr hole)", s)
		}
	}
	// Adjacent private space stays denied.
	if !p.IPDenied(netip.MustParseAddr("192.168.2.7")) {
		t.Errorf("IPDenied(192.168.2.7) = false, want true")
	}
}

func TestEgressHostDenied(t *testing.T) {
	p := &webhooks.EgressPolicy{DenyHosts: []string{"metadata.google.internal", "*.corp.example.com"}}
	cases := []struct {
		host   string
		denied bool
	}{
		{"metadata.google.internal", true},
		{"METADATA.GOOGLE.INTERNAL", true},  // case-insensitive
		{"metadata.google.internal.", true}, // trailing-dot FQDN form
		{"jenkins.corp.example.com", true},  // wildcard suffix
		{"a.b.corp.example.com", true},      // deep subdomain
		{"corp.example.com", false},         // *. requires a label before the suffix
		{"example.com", false},
		{"10.0.0.5", false}, // raw IPs never glob-match (IP layer handles them)
	}
	for _, c := range cases {
		if _, got := p.HostDenied(c.host); got != c.denied {
			t.Errorf("HostDenied(%q) = %v, want %v", c.host, got, c.denied)
		}
	}
}

func TestEgressHostDeniedRawIPNotGlobMatched(t *testing.T) {
	// A deny pattern that looks like it could glob-match an IP (e.g. "*.0.0.1"
	// matching "127.0.0.1") must NOT fire: raw IPs are the IP layer's concern.
	p := &webhooks.EgressPolicy{DenyHosts: []string{"*.0.0.1"}}
	if _, denied := p.HostDenied("127.0.0.1"); denied {
		t.Error(`HostDenied("127.0.0.1") with pattern "*.0.0.1" = true, want false (IP literals bypass host globs)`)
	}
}

func TestEgressDialDeniedAndAllowed(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	ctx := context.Background()
	denyAll := &webhooks.EgressPolicy{}
	if _, err := denyAll.DialContext(ctx, "tcp", ln.Addr().String()); err == nil {
		t.Fatal("dial to 127.0.0.1 with default policy: want error, got nil")
	} else {
		var denied *webhooks.EgressDeniedError
		if !errors.As(err, &denied) {
			t.Fatalf("want EgressDeniedError, got %T: %v", err, err)
		}
		if denied.DeniedBy != "ip" {
			t.Errorf("DeniedBy=%q, want ip", denied.DeniedBy)
		}
	}

	allowLoop := &webhooks.EgressPolicy{AllowCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}}
	c, err := allowLoop.DialContext(ctx, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial with 127.0.0.0/8 allow: %v", err)
	}
	c.Close()
}

func TestEgressHostDeniedAtDial(t *testing.T) {
	p := &webhooks.EgressPolicy{DenyHosts: []string{"*.internal.example.com"}}
	_, err := p.DialContext(context.Background(), "tcp", "ci.internal.example.com:443")
	if err == nil {
		t.Fatal("want error")
	}
	var denied *webhooks.EgressDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("want EgressDeniedError, got %T: %v", err, err)
	}
	if denied.DeniedBy != "host" || denied.Pattern != "*.internal.example.com" {
		t.Errorf("DeniedBy=%q Pattern=%q, want host / *.internal.example.com", denied.DeniedBy, denied.Pattern)
	}
}

func TestEgressClientDoesNotFollowRedirects(t *testing.T) {
	redirSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.invalid/next", http.StatusFound)
	}))
	defer redirSrv.Close()

	client := webhooks.NewHTTPClient(
		&webhooks.EgressPolicy{AllowCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}},
		2*time.Second)
	resp, err := client.Get(redirSrv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status=%d, want 302 (redirect must not be followed)", resp.StatusCode)
	}
}

func TestEgressDeniedErrorSurvivesClientDo(t *testing.T) {
	client := webhooks.NewHTTPClient(&webhooks.EgressPolicy{}, 2*time.Second)
	_, err := client.Get("http://127.0.0.1:1/hook")
	if err == nil {
		t.Fatal("want error")
	}
	var denied *webhooks.EgressDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("EgressDeniedError not unwrappable from client.Do error chain: %T: %v", err, err)
	}
}
