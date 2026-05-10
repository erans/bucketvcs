package azureblob

import "testing"

func TestParseURL(t *testing.T) {
	tests := []struct {
		raw, wantContainer, wantPrefix, wantErr string
	}{
		{"azureblob://my-container", "my-container", "", ""},
		{"azureblob://my-container/repos", "my-container", "repos/", ""},
		{"azureblob://my-container/a/b/", "my-container", "a/b/", ""},
		{"azureblob://", "", "", "container required"},
		{"s3://x", "", "", "unsupported scheme"},
		{"azureblob://user:pw@c", "", "", "must not contain credentials"},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			cfg, err := ParseURL(tc.raw)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("ParseURL(%q): want nil, got %v", tc.raw, err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("ParseURL(%q): want %q, got nil", tc.raw, tc.wantErr)
			case tc.wantErr != "":
				if !contains(err.Error(), tc.wantErr) {
					t.Fatalf("ParseURL(%q): want %q, got %v", tc.raw, tc.wantErr, err)
				}
				return
			}
			if cfg.Container != tc.wantContainer {
				t.Errorf("Container = %q, want %q", cfg.Container, tc.wantContainer)
			}
			if cfg.Prefix != tc.wantPrefix {
				t.Errorf("Prefix = %q, want %q", cfg.Prefix, tc.wantPrefix)
			}
		})
	}
}
