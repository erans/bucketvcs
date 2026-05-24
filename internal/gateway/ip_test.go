package gateway_test

import (
	"net/http/httptest"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gateway"
)

func TestClientIP_NoTrustProxy(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:54321"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 10.0.0.1")

	got := gateway.ClientIP(r, false)
	if got != "10.0.0.5" {
		t.Errorf("ClientIP(trustProxy=false) = %q, want 10.0.0.5", got)
	}
}

func TestClientIP_TrustProxyUsesRightmostHop(t *testing.T) {
	// Spoofable-leftmost regression: a standard appending proxy turns
	// `X-Forwarded-For: <attacker-supplied>` into
	// `<attacker-supplied>, <real-client-IP>`. We must take the RIGHTMOST
	// hop (the value the trusted proxy appended), not the leftmost (which
	// the client controls).
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:54321"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 10.0.0.1")

	got := gateway.ClientIP(r, true)
	if got != "10.0.0.1" {
		t.Errorf("ClientIP(trustProxy=true) = %q, want 10.0.0.1 (rightmost, proxy-appended)", got)
	}
}

func TestClientIP_TrustProxyNoHeader(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:54321"
	got := gateway.ClientIP(r, true)
	if got != "10.0.0.5" {
		t.Errorf("ClientIP(trustProxy=true, no XFF) = %q, want 10.0.0.5", got)
	}
}

func TestClientIP_RemoteAddrNoPort(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.168.1.1"
	got := gateway.ClientIP(r, false)
	if got != "192.168.1.1" {
		t.Errorf("ClientIP(no port) = %q, want 192.168.1.1", got)
	}
}

func TestClientIP_XFFWhitespace(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:54321"
	r.Header.Set("X-Forwarded-For", "1.2.3.4,   4.5.6.7  ")
	got := gateway.ClientIP(r, true)
	if got != "4.5.6.7" {
		t.Errorf("ClientIP(XFF with whitespace) = %q, want 4.5.6.7", got)
	}
}

func TestClientIP_XFFEmptyRightSegmentFallsThrough(t *testing.T) {
	// Trailing comma / whitespace-only rightmost must NOT produce key="".
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:54321"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, ")
	got := gateway.ClientIP(r, true)
	if got != "10.0.0.5" {
		t.Errorf("ClientIP(empty rightmost) = %q, want 10.0.0.5 (fall through to RemoteAddr)", got)
	}
}
