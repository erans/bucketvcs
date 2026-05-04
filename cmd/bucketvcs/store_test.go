package main

import (
	"strings"
	"testing"
)

func TestParseStoreURL_LocalFS(t *testing.T) {
	cases := []struct {
		url, wantPath string
	}{
		{"localfs:/tmp/x", "/tmp/x"},
		{"localfs:./relative", "./relative"},
		{"localfs:" + strings.Repeat("a", 200), strings.Repeat("a", 200)},
	}
	for _, c := range cases {
		scheme, path, err := parseStoreURL(c.url)
		if err != nil {
			t.Errorf("%q: %v", c.url, err)
			continue
		}
		if scheme != "localfs" || path != c.wantPath {
			t.Errorf("%q: got (%q,%q), want (localfs, %q)", c.url, scheme, path, c.wantPath)
		}
	}
}

func TestParseStoreURL_Errors(t *testing.T) {
	cases := []string{"", "localfs", "s3://bucket/key", "http://x", "localfs:"}
	for _, in := range cases {
		if _, _, err := parseStoreURL(in); err == nil {
			t.Errorf("%q: want error", in)
		}
	}
}
