package azureblob

import (
	"strings"
	"testing"
)

func TestNormalizePrefix(t *testing.T) {
	tests := []struct {
		in, want, wantErr string
	}{
		{"", "", ""},
		{"repos", "repos/", ""},
		{"repos/", "repos/", ""},
		{"a/b/c", "a/b/c/", ""},
		{"a/b/c/", "a/b/c/", ""},
		{"/leading", "", "leading"},
		{"//double", "", "double"},
		{"trailing//", "", "double"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := normalizePrefix(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("normalizePrefix(%q) err = %v, want containing %q", tc.in, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizePrefix(%q) err = %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("normalizePrefix(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestApplyPrefix(t *testing.T) {
	if got := applyPrefix("repos/", "objects/abc"); got != "repos/objects/abc" {
		t.Errorf("applyPrefix = %q", got)
	}
	if got := applyPrefix("", "objects/abc"); got != "objects/abc" {
		t.Errorf("applyPrefix empty = %q", got)
	}
}

func TestStripPrefix(t *testing.T) {
	if got := stripPrefix("repos/", "repos/objects/abc"); got != "objects/abc" {
		t.Errorf("stripPrefix = %q", got)
	}
	if got := stripPrefix("", "objects/abc"); got != "objects/abc" {
		t.Errorf("stripPrefix empty = %q", got)
	}
}
