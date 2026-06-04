package quota

import (
	"testing"
)

func TestParseSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input   string
		want    int64
		wantErr bool
	}{
		// Bare digit strings — treated as bytes.
		{input: "0", want: 0},
		{input: "1", want: 1},
		{input: "1048576", want: 1048576},

		// Binary suffixes (powers of 1024).
		{input: "1KiB", want: 1024},
		{input: "1MiB", want: 1 << 20},
		{input: "10GiB", want: 10 * (1 << 30)},
		{input: "2TiB", want: 2 * (1 << 40)},

		// Decimal suffixes (powers of 1000).
		{input: "1KB", want: 1_000},
		{input: "1MB", want: 1_000_000},
		{input: "1GB", want: 1_000_000_000},
		{input: "1TB", want: 1_000_000_000_000},

		// Plain "B" suffix.
		{input: "512B", want: 512},

		// Leading/trailing whitespace is trimmed.
		{input: "  1KiB  ", want: 1024},

		// Zero with suffix.
		{input: "0MiB", want: 0},

		// Error cases.
		{input: "", wantErr: true},       // empty string
		{input: "10XB", wantErr: true},   // unrecognised suffix → falls through to bare parse → strconv fails
		{input: "-5", wantErr: true},     // negative bare
		{input: "-1KiB", wantErr: true},  // negative with suffix
		{input: "abc", wantErr: true},    // non-numeric
		{input: "1.5GiB", wantErr: true}, // fractional not supported
		// Overflow: 99999999999TiB exceeds int64 max (2^40 * 99999999999 > 2^63-1).
		{input: "99999999999TiB", wantErr: true},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			got, err := ParseSize(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseSize(%q) = %d, want error", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSize(%q) unexpected error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("ParseSize(%q) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}
