package main

import (
	"flag"
	"net/netip"
	"testing"
)

// TestRegisterServeFlags_WebhookEgress verifies the repeatable M25 egress
// flags accumulate into the serveFlags slices and that a malformed CIDR is
// surfaced as a parse error.
func TestRegisterServeFlags_WebhookEgress(t *testing.T) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	sf := registerServeFlags(fs)

	if err := fs.Parse([]string{
		"--webhook-allow-cidr=10.0.0.0/8",
		"--webhook-allow-cidr=192.168.1.0/24",
		"--webhook-deny-host=*.corp.example.com",
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}

	wantCIDRs := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("192.168.1.0/24"),
	}
	if len(sf.webhookAllowCIDRs) != len(wantCIDRs) {
		t.Fatalf("allow-cidr count: got %d, want %d (%v)", len(sf.webhookAllowCIDRs), len(wantCIDRs), sf.webhookAllowCIDRs)
	}
	for i, p := range wantCIDRs {
		if sf.webhookAllowCIDRs[i] != p {
			t.Errorf("allow-cidr[%d]: got %v, want %v", i, sf.webhookAllowCIDRs[i], p)
		}
	}

	if len(sf.webhookDenyHosts) != 1 || sf.webhookDenyHosts[0] != "*.corp.example.com" {
		t.Errorf("deny-host: got %v, want [*.corp.example.com]", sf.webhookDenyHosts)
	}
}

// TestRegisterServeFlags_BadCIDR asserts a malformed --webhook-allow-cidr
// value fails fs.Parse rather than being silently dropped.
func TestRegisterServeFlags_BadCIDR(t *testing.T) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(&nullWriter{})
	_ = registerServeFlags(fs)

	if err := fs.Parse([]string{"--webhook-allow-cidr=not-a-cidr"}); err == nil {
		t.Fatal("expected parse error for malformed CIDR, got nil")
	}
}

// TestRegisterServeFlags_BadDenyHost asserts a wildcard-only deny pattern is
// rejected at parse time.
func TestRegisterServeFlags_BadDenyHost(t *testing.T) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(&nullWriter{})
	_ = registerServeFlags(fs)

	if err := fs.Parse([]string{"--webhook-deny-host=*"}); err == nil {
		t.Fatal("expected parse error for wildcard-only deny host, got nil")
	}
}

type nullWriter struct{}

func (nullWriter) Write(p []byte) (int, error) { return len(p), nil }
