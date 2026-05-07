package s3compat

import "testing"

func TestApplyPrefix(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
		key    string
		want   string
	}{
		{"empty prefix", "", "manifests/root.json", "manifests/root.json"},
		{"trailing slash", "tenants/", "acme/repo/manifests/root.json", "tenants/acme/repo/manifests/root.json"},
		{"no trailing slash", "tenants", "acme/repo", "tenants/acme/repo"},
		{"deeply nested", "a/b/c/", "x", "a/b/c/x"},
		{"empty key with prefix", "p/", "", "p/"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := applyPrefix(tc.prefix, tc.key)
			if got != tc.want {
				t.Fatalf("applyPrefix(%q, %q) = %q, want %q", tc.prefix, tc.key, got, tc.want)
			}
		})
	}
}

func TestStripPrefix(t *testing.T) {
	tests := []struct {
		name    string
		prefix  string
		stored  string
		want    string
		wantErr bool
	}{
		{"empty prefix", "", "manifests/root.json", "manifests/root.json", false},
		{"matching prefix", "tenants/", "tenants/acme/repo", "acme/repo", false},
		{"mismatch is fatal", "tenants/", "other/x", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := stripPrefix(tc.prefix, tc.stored)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("stripPrefix(%q, %q) want error, got %q", tc.prefix, tc.stored, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("stripPrefix(%q, %q): unexpected error %v", tc.prefix, tc.stored, err)
			}
			if got != tc.want {
				t.Fatalf("stripPrefix(%q, %q) = %q, want %q", tc.prefix, tc.stored, got, tc.want)
			}
		})
	}
}

func TestNormalizePrefix(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"foo", "foo/", false},
		{"foo/", "foo/", false},
		{"foo/bar/", "foo/bar/", false},
		{"/foo/", "", true},       // leading slash
		{"foo/../bar/", "", true}, // path traversal
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := normalizePrefix(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("normalizePrefix(%q) want error, got %q", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizePrefix(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("normalizePrefix(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
