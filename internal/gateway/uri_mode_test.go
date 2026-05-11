package gateway

import "testing"

func TestParseURIMode(t *testing.T) {
	cases := []struct {
		in   string
		want URIMode
		ok   bool
	}{
		{"auto", URIModeAuto, true},
		{"direct", URIModeDirect, true},
		{"proxied", URIModeProxied, true},
		{"off", URIModeOff, true},
		{"", URIModeAuto, false},
		{"weird", URIModeAuto, false},
	}
	for _, c := range cases {
		got, ok := ParseURIMode(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("ParseURIMode(%q) = (%v, %v), want (%v, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestParseURIMode_Roundtrip(t *testing.T) {
	for _, m := range []URIMode{URIModeAuto, URIModeDirect, URIModeProxied, URIModeOff} {
		got, ok := ParseURIMode(m.String())
		if !ok || got != m {
			t.Errorf("round-trip %v: ParseURIMode(%q) = (%v, %v)", m, m.String(), got, ok)
		}
	}
}
