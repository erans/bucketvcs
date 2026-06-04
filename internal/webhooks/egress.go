package webhooks

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"time"
)

// EgressPolicy decides which hosts/IPs the webhook delivery worker may dial.
// The zero value is the secure default: no host patterns, no allow holes —
// loopback, link-local (incl. cloud metadata 169.254.169.254), RFC1918/ULA
// private space, multicast, unspecified, and broadcast addresses are denied.
//
// Enforcement is two-layered (spec: M25 §B):
//
//  1. HostDenied — operator policy on the hostname, checked BEFORE DNS
//     resolution. Not a security boundary (a raw IP or alternate name
//     bypasses it); it covers what IP rules can't: internal names that
//     resolve to public IPs (split-horizon DNS, internal apps behind
//     public load balancers).
//  2. IPDenied — checked on the RESOLVED address inside the dialer's
//     Control hook, which closes DNS rebinding: whatever the name resolved
//     to at delivery time is what gets checked.
//
// There is deliberately NO allow-host list: allow-by-name against an IP deny
// set would mean "this name may resolve to private IPs", re-opening rebinding
// through the allowed name. Private receivers are reached via AllowCIDRs.
//
// An EgressPolicy is immutable after construction and safe for concurrent
// use — the worker shares one instance across its delivery goroutines.
type EgressPolicy struct {
	DenyHosts  []string       // lowercase glob patterns: "exact.name" or "*.suffix"
	AllowCIDRs []netip.Prefix // holes punched in the IP deny set
}

// EgressDeniedError reports a delivery connection refused by policy.
// recordResult stores its Error() string as last_error, so the message
// carries the operator-facing remediation hint.
type EgressDeniedError struct {
	Host     string // hostname (or literal IP) from the endpoint URL
	IP       string // resolved IP that was denied ("" when DeniedBy=="host")
	DeniedBy string // "host" | "ip"
	Pattern  string // matched deny-host pattern (DeniedBy=="host" only)
}

func (e *EgressDeniedError) Error() string {
	if e.DeniedBy == "host" {
		return fmt.Sprintf("egress denied: host %q matches deny pattern %q (see --webhook-deny-host)", e.Host, e.Pattern)
	}
	return fmt.Sprintf("egress denied: %s resolves to %s in a blocked range (see --webhook-allow-cidr)", e.Host, e.IP)
}

// HostDenied reports whether host matches a deny pattern, returning the
// matched pattern. Matching is case-insensitive; a trailing dot (FQDN form)
// is ignored. "*.suffix" matches any name with at least one label before
// ".suffix" (so "*.corp.com" does NOT match the bare "corp.com").
func (p *EgressPolicy) HostDenied(host string) (string, bool) {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	// raw IP literals are the IP layer's concern; globs are hostname policy.
	if _, err := netip.ParseAddr(h); err == nil {
		return "", false
	}
	for _, pat := range p.DenyHosts {
		lp := strings.ToLower(pat)
		if rest, ok := strings.CutPrefix(lp, "*."); ok {
			if strings.HasSuffix(h, "."+rest) {
				return pat, true
			}
		} else if h == lp {
			return pat, true
		}
	}
	return "", false
}

// IPDenied reports whether ip is outside the allowed egress set. AllowCIDRs
// are checked first (an allow hole wins); otherwise the address must be
// global unicast and not private.
func (p *EgressPolicy) IPDenied(ip netip.Addr) bool {
	ip = ip.Unmap()
	for _, pfx := range p.AllowCIDRs {
		if pfx.Contains(ip) {
			return false
		}
	}
	// IsGlobalUnicast already excludes loopback, link-local, multicast,
	// unspecified, and the IPv4 broadcast address — but (per Go docs) it
	// returns true for RFC1918/ULA private space, hence the IsPrivate OR.
	return !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate()
}

// DialContext is the policy-enforcing dialer used by NewHTTPClient. The
// hostname check runs pre-resolution; the IP check runs in the Dialer's
// Control hook on every address the resolver produced (the stdlib tries
// candidates in order, so a name resolving to [private, public] connects to
// the public one).
func (p *EgressPolicy) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if pat, denied := p.HostDenied(host); denied {
		return nil, &EgressDeniedError{Host: host, DeniedBy: "host", Pattern: pat}
	}
	d := &net.Dialer{
		// Connect-phase bound; independent of the client-level request timeout set in NewHTTPClient.
		Timeout: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			h, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(h)
			if err != nil {
				return err
			}
			if p.IPDenied(ip) {
				return &EgressDeniedError{Host: host, IP: ip.String(), DeniedBy: "ip"}
			}
			return nil
		},
	}
	return d.DialContext(ctx, network, addr)
}

// NewHTTPClient builds the delivery worker's HTTP client: policy-enforcing
// dialer, no proxy (an env-configured HTTP_PROXY would dial on our behalf and
// bypass the policy), and no redirect following (a 3xx is a delivery failure;
// industry convention, and simpler to reason about than re-checking hops).
func NewHTTPClient(p *EgressPolicy, timeout time.Duration) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.DialContext = p.DialContext
	return &http.Client{
		Timeout:   timeout,
		Transport: t,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
