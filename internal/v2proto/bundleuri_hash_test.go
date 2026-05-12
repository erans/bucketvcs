package v2proto

import "testing"

func TestBundleHashHex(t *testing.T) {
	const validHex = "deadbeef0123456789abcdef0123456789abcdef0123456789abcdef01234567" // 64 lowercase hex
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"valid 64-char lowercase hex", "sha256-" + validHex, validHex},
		{"empty input", "", ""},
		{"missing prefix", validHex, ""},
		{"bare prefix only", "sha256-", ""},
		{"too short", "sha256-deadbeef", ""},
		{"too long", "sha256-" + validHex + "00", ""},
		{"uppercase rejected", "sha256-DEADBEEF0123456789abcdef0123456789abcdef0123456789abcdef01234567", ""},
		{"non-hex char rejected", "sha256-zeadbeef0123456789abcdef0123456789abcdef0123456789abcdef01234567", ""},
		{"wrong prefix", "sha1-" + validHex, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := bundleHashHex(tc.in)
			if got != tc.want {
				t.Errorf("bundleHashHex(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
