package web

import "testing"

func TestSafeNext(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "/"},
		{"/", "/"},
		{"/repos", "/repos"},
		{"/acme/demo", "/acme/demo"},
		{"//evil.com", "/"},          // protocol-relative
		{"https://evil.com", "/"},    // absolute
		{"/\\evil.com", "/"},         // backslash → browser normalizes to "//"
		{"/\\/evil.com", "/"},        // mixed
		{"\\\\evil.com", "/"},        // not "/"-prefixed
		{"javascript:alert(1)", "/"}, // not "/"-prefixed
	}
	for _, c := range cases {
		if got := safeNext(c.in); got != c.want {
			t.Errorf("safeNext(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
